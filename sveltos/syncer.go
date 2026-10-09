// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

// Package sveltos triggers a Sveltos redeploy through the Kubernetes API. It
// patches a ClusterProfile's (or Profile's) profile.projectsveltos.io/redeploy
// annotation, which makes Sveltos redeploy every ClusterSummary the profile
// manages. A redeploy fetches the profile's remoteURL again, so a freshly
// published Release reaches the cluster without waiting for the profile's
// polling interval; a redeploy with no new Release writes no objects. Sveltos
// ignores the annotation for profiles with syncMode OneTime.
package sveltos

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

// RedeployAnnotation forces a redeploy of every ClusterSummary a profile manages
// whenever its value changes (addon-controller api/v1beta1/spec.go).
const RedeployAnnotation = "profile.projectsveltos.io/redeploy"

var (
	// ClusterProfilesGVR is the GroupVersionResource of Sveltos ClusterProfiles,
	// which are cluster-scoped.
	ClusterProfilesGVR = schema.GroupVersionResource{
		Group:    "config.projectsveltos.io",
		Version:  "v1beta1",
		Resource: "clusterprofiles",
	}
	// ProfilesGVR is the GroupVersionResource of Sveltos Profiles, which are
	// namespaced.
	ProfilesGVR = schema.GroupVersionResource{
		Group:    "config.projectsveltos.io",
		Version:  "v1beta1",
		Resource: "profiles",
	}
	// ClusterSummariesGVR is the GroupVersionResource of Sveltos ClusterSummaries:
	// one per profile and matching cluster, carrying the deploy status.
	ClusterSummariesGVR = schema.GroupVersionResource{
		Group:    "config.projectsveltos.io",
		Version:  "v1beta1",
		Resource: "clustersummaries",
	}
	// HealthCheckReportsGVR is the GroupVersionResource of Sveltos
	// HealthCheckReports: one per HealthCheck and cluster.
	HealthCheckReportsGVR = schema.GroupVersionResource{
		Group:    "lib.projectsveltos.io",
		Version:  "v1beta1",
		Resource: "healthcheckreports",
	}
)

// Syncer triggers Sveltos redeploys by annotating ClusterProfiles and Profiles.
//
// Profiles are named by key: a ClusterProfile by its name, a Profile by
// <namespace>/<name>. A ClusterProfile named after its variant Space therefore
// still matches the app-name == space-slug convention.
type Syncer struct {
	client dynamic.Interface
	// now stamps each redeploy; a field so tests can fix it.
	now func() time.Time
}

// NewSyncer builds a Syncer. It uses the in-cluster ServiceAccount when running
// in a pod, falling back to the local kubeconfig (KUBECONFIG or ~/.kube/config)
// for out-of-cluster use.
func NewSyncer() (*Syncer, error) {
	client, err := kube.NewDynamicClient()
	if err != nil {
		return nil, err
	}
	return &Syncer{client: client, now: time.Now}, nil
}

// Sync forces a redeploy of the profile named by key by writing a new value to
// its profile.projectsveltos.io/redeploy annotation. Unlike Argo CD's refresh
// annotation, Sveltos does not remove it once handled, so each Sync writes a new
// value — the current time — to count as a change.
func (s *Syncer) Sync(ctx context.Context, key string) error {
	patch := fmt.Appendf(nil,
		`{"metadata":{"annotations":{%q:%q}}}`, RedeployAnnotation, s.now().UTC().Format(time.RFC3339Nano))

	if namespace, name, ok := strings.Cut(key, "/"); ok {
		_, err := s.client.Resource(ProfilesGVR).Namespace(namespace).
			Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
		if err != nil {
			return fmt.Errorf("patch Sveltos Profile %q in namespace %q: %w", name, namespace, err)
		}
		return nil
	}
	_, err := s.client.Resource(ClusterProfilesGVR).
		Patch(ctx, key, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch Sveltos ClusterProfile %q: %w", key, err)
	}
	return nil
}

// RepoURLs lists the ClusterProfiles and Profiles, mapping each key (see Syncer)
// to the remoteURLs it pulls from: every spec.policyRefs[].remoteURL.url and
// spec.kustomizationRefs[].remoteURL.url. A profile with no remoteURL still
// appears, with no URLs.
func (s *Syncer) RepoURLs(ctx context.Context) (map[string][]string, error) {
	clusterProfiles, err := s.client.Resource(ClusterProfilesGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Sveltos ClusterProfiles: %w", err)
	}
	profiles, err := s.client.Resource(ProfilesGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Sveltos Profiles: %w", err)
	}

	urls := make(map[string][]string, len(clusterProfiles.Items)+len(profiles.Items))
	for i := range clusterProfiles.Items {
		u := &clusterProfiles.Items[i]
		urls[u.GetName()] = RemoteURLs(u.Object, "spec")
	}
	for i := range profiles.Items {
		u := &profiles.Items[i]
		urls[u.GetNamespace()+"/"+u.GetName()] = RemoteURLs(u.Object, "spec")
	}
	return urls, nil
}

// RemoteURLs returns the remoteURL.url of every policyRefs and kustomizationRefs
// entry of the profile spec found at fields within obj: "spec" for a profile, and
// "spec", "clusterProfileSpec" for a ClusterSummary, which carries a copy.
func RemoteURLs(obj map[string]any, fields ...string) []string {
	var urls []string
	for _, refs := range []string{"policyRefs", "kustomizationRefs"} {
		entries, _, _ := unstructured.NestedSlice(obj, append(fields, refs)...)
		for _, entry := range entries {
			if m, ok := entry.(map[string]any); ok {
				if url, _, _ := unstructured.NestedString(m, "remoteURL", "url"); url != "" {
					urls = append(urls, url)
				}
			}
		}
	}
	return urls
}
