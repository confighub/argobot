// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

// Package kube triggers an Argo CD sync through the Kubernetes API rather than
// the Argo CD REST API. It patches the Argo CD Application custom resource with
// the argocd.argoproj.io/refresh annotation, which tells Argo CD to re-compare
// against its source (and, for an OCI source, re-resolve tag->digest). Unlike
// the REST /sync call, a refresh only deploys when the Application has auto-sync
// enabled — an intentional gate.
package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// applicationsGVR is the GroupVersionResource of Argo CD Application custom
// resources.
var applicationsGVR = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "applications",
}

// Config configures a Syncer. Namespace is where the Application resources live
// (Argo CD's namespace, or the app's own namespace in apps-in-any-namespace
// mode). RefreshType is "hard" or "normal"; empty means "hard".
type Config struct {
	Namespace   string
	RefreshType string
}

// Syncer triggers Argo CD syncs by annotating Application resources.
type Syncer struct {
	client      dynamic.Interface
	namespace   string
	refreshType string
}

// NewSyncer builds a Syncer. It uses the in-cluster ServiceAccount when running
// in a pod, falling back to the local kubeconfig (KUBECONFIG or ~/.kube/config)
// for out-of-cluster use.
func NewSyncer(cfg Config) (*Syncer, error) {
	restCfg, err := restConfig()
	if err != nil {
		return nil, fmt.Errorf("build kubernetes config: %w", err)
	}
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	refreshType := cfg.RefreshType
	if refreshType == "" {
		refreshType = "hard"
	}
	return &Syncer{
		client:      client,
		namespace:   cfg.Namespace,
		refreshType: refreshType,
	}, nil
}

// restConfig prefers in-cluster credentials and falls back to a kubeconfig file.
func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

// Sync requests an Argo CD refresh of the named Application by patching its
// argocd.argoproj.io/refresh annotation. Argo CD removes the annotation once it
// has processed the refresh, so re-patching re-triggers on the next event.
func (s *Syncer) Sync(ctx context.Context, appName string) error {
	patch := fmt.Appendf(nil,
		`{"metadata":{"annotations":{"argocd.argoproj.io/refresh":%q}}}`, s.refreshType)

	_, err := s.client.Resource(applicationsGVR).Namespace(s.namespace).
		Patch(ctx, appName, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch Argo CD application %q in namespace %q: %w", appName, s.namespace, err)
	}
	return nil
}
