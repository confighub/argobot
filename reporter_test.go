// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"strings"
	"testing"

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

	if got.Source != liveStatusSource {
		t.Errorf("Source = %q, want %q", got.Source, liveStatusSource)
	}
	if got.App != "orders-prod" {
		t.Errorf("App = %q, want orders-prod", got.App)
	}
	if got.SyncStatus != "Synced" || got.Revision != "sha256:abc" {
		t.Errorf("sync fields wrong: %+v", got)
	}
	if got.HealthStatus != "Healthy" {
		t.Errorf("HealthStatus = %q", got.HealthStatus)
	}
	if got.OperationPhase != "Succeeded" {
		t.Errorf("OperationPhase = %q", got.OperationPhase)
	}
	// Health message is preferred over the operation message.
	if got.Message != "all good" {
		t.Errorf("Message = %q, want health message", got.Message)
	}
	// ObservedAt is left empty so the projection doubles as a dedup signature.
	if got.ObservedAt != "" {
		t.Errorf("ObservedAt = %q, want empty", got.ObservedAt)
	}
}

func TestProjectStatusFallsBackToOperationMessage(t *testing.T) {
	u := app("api", map[string]any{
		"sync":           map[string]any{"status": "OutOfSync"},
		"health":         map[string]any{"status": "Degraded"},
		"operationState": map[string]any{"phase": "Failed", "message": "boom"},
	})
	got := projectStatus(u)
	if got.Message != "boom" {
		t.Errorf("Message = %q, want operation message fallback", got.Message)
	}
}

func TestProjectStatusMissingFields(t *testing.T) {
	// A freshly created Application with no status yet must not panic and must
	// project empty strings, still carrying Source and App.
	u := app("fresh", map[string]any{})
	got := projectStatus(u)
	if got.Source != liveStatusSource || got.App != "fresh" {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if got.SyncStatus != "" || got.HealthStatus != "" || got.Message != "" {
		t.Errorf("expected empty status fields, got %+v", got)
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
