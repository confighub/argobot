// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package sveltos

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

func profile(kind, namespace, name string, spec map[string]any) *unstructured.Unstructured {
	metadata := map[string]any{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "config.projectsveltos.io/v1beta1",
		"kind":       kind,
		"metadata":   metadata,
		"spec":       spec,
	}}
}

func fakeClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			ClusterProfilesGVR: "ClusterProfileList",
			ProfilesGVR:        "ProfileList",
		},
		objs...,
	)
}

func TestRepoURLs(t *testing.T) {
	client := fakeClient(
		profile("ClusterProfile", "", "orders-prod", map[string]any{
			"policyRefs": []any{
				map[string]any{"remoteURL": map[string]any{"url": "oci://gw/space/orders-prod:latest"}},
				map[string]any{"kind": "ConfigMap", "name": "inline"},
			},
		}),
		profile("ClusterProfile", "", "kustomized", map[string]any{
			"kustomizationRefs": []any{
				map[string]any{"remoteURL": map[string]any{"url": "oci://gw/space/kustomized"}},
			},
		}),
		profile("ClusterProfile", "", "no-remote", map[string]any{}),
		profile("Profile", "team-a", "api", map[string]any{
			"policyRefs": []any{
				map[string]any{"remoteURL": map[string]any{"url": "oci://gw/space/api-dev@sha256:abc"}},
			},
		}),
	)
	s := &Syncer{client: client, now: time.Now}

	got, err := s.RepoURLs(context.Background())
	if err != nil {
		t.Fatalf("RepoURLs() error = %v", err)
	}

	want := map[string][]string{
		"orders-prod": {"oci://gw/space/orders-prod:latest"},
		"kustomized":  {"oci://gw/space/kustomized"},
		"no-remote":   nil,
		"team-a/api":  {"oci://gw/space/api-dev@sha256:abc"},
	}
	if len(got) != len(want) {
		t.Fatalf("RepoURLs() = %v, want %v", got, want)
	}
	for key, urls := range want {
		if !slices.Equal(got[key], urls) {
			t.Errorf("RepoURLs()[%q] = %v, want %v", key, got[key], urls)
		}
	}
	if _, ok := got["no-remote"]; !ok {
		t.Errorf("a profile with no remoteURL must still be listed: %v", got)
	}
}

func TestSyncWritesNewRedeployValue(t *testing.T) {
	client := fakeClient(
		profile("ClusterProfile", "", "orders-prod", map[string]any{}),
		profile("Profile", "team-a", "api", map[string]any{}),
	)
	stamp := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := &Syncer{client: client, now: func() time.Time { return stamp }}
	ctx := context.Background()

	redeployValue := func(gvr schema.GroupVersionResource, namespace, name string) string {
		t.Helper()
		u, err := client.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get %s %s/%s: %v", gvr.Resource, namespace, name, err)
		}
		return u.GetAnnotations()[RedeployAnnotation]
	}

	if err := s.Sync(ctx, "orders-prod"); err != nil {
		t.Fatalf("Sync(ClusterProfile) error = %v", err)
	}
	first := redeployValue(ClusterProfilesGVR, "", "orders-prod")
	if first != stamp.Format(time.RFC3339Nano) {
		t.Errorf("redeploy annotation = %q, want %q", first, stamp.Format(time.RFC3339Nano))
	}

	// The annotation persists, so a second Sync must write a different value.
	stamp = stamp.Add(time.Second)
	if err := s.Sync(ctx, "orders-prod"); err != nil {
		t.Fatalf("second Sync error = %v", err)
	}
	if second := redeployValue(ClusterProfilesGVR, "", "orders-prod"); second == first {
		t.Errorf("second Sync left the redeploy annotation at %q", second)
	}

	if err := s.Sync(ctx, "team-a/api"); err != nil {
		t.Fatalf("Sync(Profile) error = %v", err)
	}
	if got := redeployValue(ProfilesGVR, "team-a", "api"); got != stamp.Format(time.RFC3339Nano) {
		t.Errorf("Profile redeploy annotation = %q", got)
	}

	if err := s.Sync(ctx, "missing"); err == nil {
		t.Error("Sync of a missing ClusterProfile: error = nil")
	}
}
