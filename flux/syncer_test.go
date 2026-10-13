// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package flux

import (
	"context"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func ociRepository(namespace, name, url string) *unstructured.Unstructured {
	spec := map[string]any{"ref": map[string]any{"tag": "latest"}}
	if url != "" {
		spec["url"] = url
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       "OCIRepository",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       spec,
	}}
}

func fakeClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			OCIRepositoriesGVR: "OCIRepositoryList",
		},
		objs...,
	)
}

func TestRepoURLs(t *testing.T) {
	client := fakeClient(
		ociRepository("flux-system", "fx-app", "oci://gw/space/fx-app"),
		ociRepository("team-a", "api", "oci://gw/space/api-dev"),
		ociRepository("flux-system", "no-url", ""),
	)
	s := &Syncer{client: client, namespace: "flux-system", now: time.Now}

	got, err := s.RepoURLs(context.Background())
	if err != nil {
		t.Fatalf("RepoURLs() error = %v", err)
	}

	want := map[string][]string{
		"flux-system/fx-app": {"oci://gw/space/fx-app"},
		"team-a/api":         {"oci://gw/space/api-dev"},
		"flux-system/no-url": nil,
	}
	if len(got) != len(want) {
		t.Fatalf("RepoURLs() = %v, want %v", got, want)
	}
	for key, urls := range want {
		if !slices.Equal(got[key], urls) {
			t.Errorf("RepoURLs()[%q] = %v, want %v", key, got[key], urls)
		}
	}
	if _, ok := got["flux-system/no-url"]; !ok {
		t.Errorf("a repository with no URL must still be listed: %v", got)
	}
}

func TestSyncWritesNewRequestedAtValue(t *testing.T) {
	client := fakeClient(
		ociRepository("flux-system", "fx-app", "oci://gw/space/fx-app"),
		ociRepository("team-a", "api", "oci://gw/space/api-dev"),
	)
	stamp := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	s := &Syncer{client: client, namespace: "flux-system", now: func() time.Time { return stamp }}
	ctx := context.Background()

	requestedAt := func(namespace, name string) string {
		t.Helper()
		u, err := client.Resource(OCIRepositoriesGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get ocirepository %s/%s: %v", namespace, name, err)
		}
		return u.GetAnnotations()[RequestedAtAnnotation]
	}

	if err := s.Sync(ctx, "team-a/api"); err != nil {
		t.Fatalf("Sync(team-a/api) error = %v", err)
	}
	first := requestedAt("team-a", "api")
	if first != stamp.Format(time.RFC3339Nano) {
		t.Errorf("requestedAt annotation = %q, want %q", first, stamp.Format(time.RFC3339Nano))
	}

	// The annotation persists, so a second Sync must write a different value.
	stamp = stamp.Add(time.Second)
	if err := s.Sync(ctx, "team-a/api"); err != nil {
		t.Fatalf("second Sync error = %v", err)
	}
	if second := requestedAt("team-a", "api"); second == first {
		t.Errorf("second Sync left the requestedAt annotation at %q", second)
	}

	// A bare name is looked up in the default namespace.
	if err := s.Sync(ctx, "fx-app"); err != nil {
		t.Fatalf("Sync(fx-app) error = %v", err)
	}
	if got := requestedAt("flux-system", "fx-app"); got != stamp.Format(time.RFC3339Nano) {
		t.Errorf("bare-name requestedAt annotation = %q", got)
	}

	if err := s.Sync(ctx, "missing"); err == nil {
		t.Error("Sync of a missing OCIRepository: error = nil")
	}
}
