// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package kube

import (
	"context"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func application(namespace, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       spec,
	}}
}

func TestRepoURLs(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{ApplicationsGVR: "ApplicationList"},
		application("argocd", "single", map[string]any{
			"source": map[string]any{"repoURL": "oci://gw/space/argo-single"},
		}),
		application("argocd", "multi", map[string]any{
			"sources": []any{
				map[string]any{"repoURL": "https://charts.example.com/stable"},
				map[string]any{"repoURL": "oci://gw/space/argo-multi"},
			},
		}),
		application("argocd", "sourceless", map[string]any{}),
		application("elsewhere", "other-namespace", map[string]any{
			"source": map[string]any{"repoURL": "oci://gw/space/argo-other"},
		}),
	)
	s := &Syncer{client: client, namespace: "argocd"}

	got, err := s.RepoURLs(context.Background())
	if err != nil {
		t.Fatalf("RepoURLs() error = %v", err)
	}

	want := map[string][]string{
		"single":     {"oci://gw/space/argo-single"},
		"multi":      {"https://charts.example.com/stable", "oci://gw/space/argo-multi"},
		"sourceless": nil,
	}
	if len(got) != len(want) {
		t.Fatalf("RepoURLs() = %v, want %v", got, want)
	}
	for name, urls := range want {
		if !slices.Equal(got[name], urls) {
			t.Errorf("RepoURLs()[%q] = %v, want %v", name, got[name], urls)
		}
	}
	if _, ok := got["sourceless"]; !ok {
		t.Errorf("an Application with no source must still be listed: %v", got)
	}
}
