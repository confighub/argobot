// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

// Package flux triggers a Flux reconciliation through the Kubernetes API. It
// patches an OCIRepository's reconcile.fluxcd.io/requestedAt annotation, which
// makes source-controller resolve the repository's tag again. When the tag
// points at a freshly published Release, source-controller stores the new
// artifact and kustomize-controller reconciles every Kustomization that uses
// the repository, so the Release reaches the cluster without waiting for the
// repository's interval; when the tag has not moved, nothing is applied.
package flux

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/confighub/argobot/kube"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// RequestedAtAnnotation asks a Flux controller to reconcile an object now
// whenever its value changes (fluxcd/pkg/apis/meta ReconcileRequestAnnotation).
const RequestedAtAnnotation = "reconcile.fluxcd.io/requestedAt"

var (
	// OCIRepositoriesGVR is the GroupVersionResource of Flux OCIRepositories,
	// which are namespaced. v1 is served from Flux 2.6 on.
	OCIRepositoriesGVR = schema.GroupVersionResource{
		Group:    "source.toolkit.fluxcd.io",
		Version:  "v1",
		Resource: "ocirepositories",
	}
	// KustomizationsGVR is the GroupVersionResource of Flux Kustomizations,
	// which are namespaced.
	KustomizationsGVR = schema.GroupVersionResource{
		Group:    "kustomize.toolkit.fluxcd.io",
		Version:  "v1",
		Resource: "kustomizations",
	}
)

// Config configures a Syncer. Namespace is where an OCIRepository named without
// a namespace is looked up; empty means "flux-system".
type Config struct {
	Namespace string
}

// Syncer triggers Flux reconciliations by annotating OCIRepositories.
//
// OCIRepositories are named by key, <namespace>/<name>. A bare name is looked up
// in the configured namespace, which is what the app-name == space-slug
// convention resolves to when listing the repositories fails.
type Syncer struct {
	client    dynamic.Interface
	namespace string
	// now stamps each request; a field so tests can fix it.
	now func() time.Time
}

// NewSyncer builds a Syncer. It uses the in-cluster ServiceAccount when running
// in a pod, falling back to the local kubeconfig (KUBECONFIG or ~/.kube/config)
// for out-of-cluster use.
func NewSyncer(cfg Config) (*Syncer, error) {
	client, err := kube.NewDynamicClient()
	if err != nil {
		return nil, err
	}
	namespace := cfg.Namespace
	if namespace == "" {
		namespace = "flux-system"
	}
	return &Syncer{client: client, namespace: namespace, now: time.Now}, nil
}

// Sync requests a reconciliation of the OCIRepository named by key by writing a
// new value to its reconcile.fluxcd.io/requestedAt annotation. Like Sveltos's
// redeploy annotation and unlike Argo CD's refresh annotation, Flux leaves it in
// place once handled, so each Sync writes a new value — the current time — to
// count as a change.
func (s *Syncer) Sync(ctx context.Context, key string) error {
	namespace, name, ok := strings.Cut(key, "/")
	if !ok {
		namespace, name = s.namespace, key
	}
	patch := fmt.Appendf(nil,
		`{"metadata":{"annotations":{%q:%q}}}`, RequestedAtAnnotation, s.now().UTC().Format(time.RFC3339Nano))

	_, err := s.client.Resource(OCIRepositoriesGVR).Namespace(namespace).
		Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch Flux OCIRepository %q in namespace %q: %w", name, namespace, err)
	}
	return nil
}

// RepoURLs lists the OCIRepositories in every namespace, mapping each key (see
// Syncer) to its spec.url. A repository with no URL still appears, with no URLs.
func (s *Syncer) RepoURLs(ctx context.Context) (map[string][]string, error) {
	list, err := s.client.Resource(OCIRepositoriesGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Flux OCIRepositories: %w", err)
	}
	urls := make(map[string][]string, len(list.Items))
	for i := range list.Items {
		u := &list.Items[i]
		var repoURLs []string
		if url := URL(u); url != "" {
			repoURLs = []string{url}
		}
		urls[u.GetNamespace()+"/"+u.GetName()] = repoURLs
	}
	return urls, nil
}

// URL returns an OCIRepository's spec.url (oci://<registry>/space/<slug>; the
// tag is in spec.ref, not the URL).
func URL(repo *unstructured.Unstructured) string {
	url, _, _ := unstructured.NestedString(repo.Object, "spec", "url")
	return url
}
