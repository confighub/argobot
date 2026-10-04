// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"strings"
	"testing"

	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func app(name string, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name},
		"status":     status,
	}}
}

func TestProjectStatus(t *testing.T) {
	u := app("orders-prod", map[string]any{
		"sync":           map[string]any{"status": "Synced", "revision": "sha256:abc"},
		"health":         map[string]any{"status": "Healthy", "message": "all good"},
		"operationState": map[string]any{"phase": "Succeeded", "message": "op ok"},
	})

	got := projectStatus(u)

	if got.Reporter != liveStatusReporter {
		t.Errorf("Reporter = %q, want %q", got.Reporter, liveStatusReporter)
	}
	if got.DataSource != "orders-prod" {
		t.Errorf("DataSource = %q, want orders-prod", got.DataSource)
	}
	if got.Sync != goclientnew.Synced || got.ReporterSync != "Synced" {
		t.Errorf("sync fields wrong: %+v", got)
	}
	if got.Health != goclientnew.ReleaseLiveStatusHealthHealthy || got.ReporterHealth != "Healthy" {
		t.Errorf("health fields wrong: %+v", got)
	}
	if got.Operation != goclientnew.ReleaseLiveStatusOperationSucceeded || got.ReporterOperation != "Succeeded" {
		t.Errorf("operation fields wrong: %+v", got)
	}
	// Health message is preferred over the operation message.
	if got.Message != "all good" {
		t.Errorf("Message = %q, want health message", got.Message)
	}
	// ObservedAt is left zero so the projection doubles as a dedup signature.
	if !got.ObservedAt.IsZero() {
		t.Errorf("ObservedAt = %v, want zero", got.ObservedAt)
	}
}

func TestProjectStatusFallsBackToOperationMessage(t *testing.T) {
	u := app("api", map[string]any{
		"sync":           map[string]any{"status": "OutOfSync"},
		"health":         map[string]any{"status": "Degraded"},
		"operationState": map[string]any{"phase": "Error", "message": "boom"},
	})
	got := projectStatus(u)
	if got.Message != "boom" {
		t.Errorf("Message = %q, want operation message fallback", got.Message)
	}
	// Argo's Error is a failed operation, and its own word is kept.
	if got.Operation != goclientnew.ReleaseLiveStatusOperationFailed || got.ReporterOperation != "Error" {
		t.Errorf("operation fields wrong: %+v", got)
	}
}

func TestProjectStatusMissingFields(t *testing.T) {
	// A freshly created Application with no status yet must not panic, and reads
	// as unknown rather than as anything a gate would accept.
	u := app("fresh", map[string]any{})
	got := projectStatus(u)
	if got.Reporter != liveStatusReporter || got.DataSource != "fresh" {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if got.Sync != goclientnew.Unknown || got.Health != goclientnew.ReleaseLiveStatusHealthUnknown ||
		got.Operation != "" || got.Message != "" {
		t.Errorf("expected unknown status, got %+v", got)
	}
}

// A merge patch keeps a field it omits, so every field is sent, and an empty one
// as null: an operation that has ended must stop reading as running.
func TestMergePatchObjectNullsEmptyFields(t *testing.T) {
	status := projectStatus(app("api", map[string]any{
		"sync":   map[string]any{"status": "Synced"},
		"health": map[string]any{"status": "Healthy"},
	}))
	got, err := mergePatchObject(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"Operation", "ReporterOperation", "Message"} {
		value, present := got[field]
		if !present || value != nil {
			t.Errorf("%s = %v (present %v), want null", field, value, present)
		}
	}
	if got["Sync"] != "Synced" || got["Reporter"] != liveStatusReporter {
		t.Errorf("set fields wrong: %v", got)
	}
}

// TestDedupSignatureStable confirms two projections of the same state marshal
// identically (ObservedAt is not part of the projection), so dedup suppresses a
// redundant write.
func TestDedupSignatureStable(t *testing.T) {
	status := map[string]any{
		"sync":   map[string]any{"status": "Synced", "revision": "r1"},
		"health": map[string]any{"status": "Healthy"},
	}
	a, _ := json.Marshal(projectStatus(app("x", status)))
	b, _ := json.Marshal(projectStatus(app("x", status)))
	if string(a) != string(b) {
		t.Errorf("signatures differ:\n a=%s\n b=%s", a, b)
	}

	// A health change must produce a different signature.
	status2 := map[string]any{
		"sync":   map[string]any{"status": "Synced", "revision": "r1"},
		"health": map[string]any{"status": "Degraded"},
	}
	c, _ := json.Marshal(projectStatus(app("x", status2)))
	if string(a) == string(c) {
		t.Errorf("signature unchanged despite health change: %s", a)
	}
}

func TestSpaceSlugFromRepoURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://oci.example.com/space/orders-prod", "orders-prod"},
		{"https://oci.example.com/space/orders-prod/", "orders-prod"},
		{"http://host.docker.internal:9092/space/api-staging", "api-staging"},
		{"https://oci.example.com/space/", ""},
		{"https://oci.example.com/notspace/orders-prod", ""},
		{"", ""},
		// A trailing extra path segment is not a bare slug.
		{"https://oci.example.com/space/orders/prod", ""},
	}
	for _, c := range cases {
		if got := spaceSlugFromRepoURL(c.in); got != c.want {
			t.Errorf("spaceSlugFromRepoURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDeploymentSpaceSlugFallsBackToName(t *testing.T) {
	// No OCI source: fall back to the Application name (== Space slug).
	u := app("orders-prod", map[string]any{})
	if got := deploymentSpaceSlug(u); got != "orders-prod" {
		t.Errorf("deploymentSpaceSlug fallback = %q, want orders-prod", got)
	}

	// With an OCI source, the repoURL wins.
	u2 := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "some-app"},
		"spec": map[string]any{
			"source": map[string]any{"repoURL": "https://oci.example.com/space/orders-prod"},
		},
	}}
	if got := deploymentSpaceSlug(u2); got != "orders-prod" {
		t.Errorf("deploymentSpaceSlug from repoURL = %q, want orders-prod", got)
	}
}

func TestTruncateBounds(t *testing.T) {
	if got := truncate("hello", 100); got != "hello" {
		t.Errorf("no-op truncate = %q", got)
	}
	long := strings.Repeat("a", 300)
	got := truncate(long, 200)
	if len(got) != 200 {
		t.Errorf("truncated len = %d, want 200", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated should end with ellipsis: %q", got[len(got)-5:])
	}
}
