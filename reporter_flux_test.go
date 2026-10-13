// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"testing"

	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// digestOf is the manifest digest of test Release n.
func digestOf(n int64) string { return fmt.Sprintf("sha256:%064d", n) }

func fluxRelease(n int64, published bool, ls *goclientnew.ReleaseLiveStatus) goclientnew.Release {
	return goclientnew.Release{
		ReleaseID:      uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n)),
		ReleaseNum:     n,
		Published:      published,
		ManifestDigest: digestOf(n),
		LiveStatus:     ls,
	}
}

// publishedReleases are Releases from to to, all published and with no status.
// Older Releases with no status would be marked replaced, so a test about
// something else starts from the Releases it is about.
func publishedReleases(from, to int64) []goclientnew.Release {
	var releases []goclientnew.Release
	for i := from; i <= to; i++ {
		releases = append(releases, fluxRelease(i, true, nil))
	}
	return releases
}

func fluxSource(revision string, conditions ...map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       "OCIRepository",
		"metadata":   map[string]any{"name": "fx-app", "namespace": "flux-system"},
		"spec":       map[string]any{"url": "oci://gw/space/fx-app", "ref": map[string]any{"tag": "latest"}},
		"status": map[string]any{
			"artifact":   map[string]any{"revision": revision},
			"conditions": conditionList(conditions),
		},
	}}
}

// condition is a Flux status condition.
func condition(typ, status, reason, message string) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": message}
}

func conditionList(conditions []map[string]any) []any {
	entries := make([]any, len(conditions))
	for i, c := range conditions {
		entries[i] = c
	}
	return entries
}

func kustomization(name string, wait bool, applied, attempted string, conditions ...map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       "Kustomization",
		"metadata":   map[string]any{"name": name, "namespace": "flux-system"},
		"spec": map[string]any{
			"path":      ".",
			"wait":      wait,
			"sourceRef": map[string]any{"kind": "OCIRepository", "name": "fx-app"},
		},
		"status": map[string]any{
			"lastAppliedRevision":   applied,
			"lastAttemptedRevision": attempted,
			"conditions":            conditionList(conditions),
		},
	}}
}

// rev is the Flux revision of test Release n.
func rev(n int64) string { return "latest@" + digestOf(n) }

func readyKustomization(name string, n int64) *unstructured.Unstructured {
	return kustomization(name, true, rev(n), rev(n),
		condition("Ready", "True", "ReconciliationSucceeded", "Applied revision: "+rev(n)),
		condition("Healthy", "True", "Succeeded", "Health check passed"))
}

// writesByNum indexes planned writes by ReleaseNum.
func writesByNum(writes []fluxWrite) map[int64]goclientnew.ReleaseLiveStatus {
	byNum := map[int64]goclientnew.ReleaseLiveStatus{}
	for _, w := range writes {
		byNum[w.Release.ReleaseNum] = w.Status
	}
	return byNum
}

func expectStatus(t *testing.T, label string, got goclientnew.ReleaseLiveStatus, sync goclientnew.ReleaseLiveStatusSync,
	health goclientnew.ReleaseLiveStatusHealth, op goclientnew.ReleaseLiveStatusOperation) {
	t.Helper()
	if got.Sync != sync || got.Health != health || got.Operation != op {
		t.Errorf("%s: sync %q health %q op %q, want %q %q %q", label, got.Sync, got.Health, got.Operation, sync, health, op)
	}
}

func TestFluxManifestDigest(t *testing.T) {
	cases := []struct{ in, want string }{
		{rev(5), digestOf(5)},
		{digestOf(5), digestOf(5)},
		{"latest", ""},
		{"", ""},
		{"latest@md5:abc", ""},
	}
	for _, c := range cases {
		if got := fluxManifestDigest(c.in); got != c.want {
			t.Errorf("fluxManifestDigest(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestKustomizationSourceKey(t *testing.T) {
	k := kustomization("app", true, "", "")
	if got := kustomizationSourceKey(k); got != "flux-system/fx-app" {
		t.Errorf("same-namespace source = %q", got)
	}
	_ = unstructured.SetNestedField(k.Object, "team-a", "spec", "sourceRef", "namespace")
	if got := kustomizationSourceKey(k); got != "team-a/fx-app" {
		t.Errorf("cross-namespace source = %q", got)
	}
	_ = unstructured.SetNestedField(k.Object, "GitRepository", "spec", "sourceRef", "kind")
	if got := kustomizationSourceKey(k); got != "" {
		t.Errorf("GitRepository source = %q, want none", got)
	}
}

func TestFromFluxRunning(t *testing.T) {
	cases := []struct {
		name string
		in   fluxConditions
		sync goclientnew.ReleaseLiveStatusSync
		h    goclientnew.ReleaseLiveStatusHealth
		op   goclientnew.ReleaseLiveStatusOperation
	}{
		{"applied and healthy",
			fluxConditions{Ready: "True", ReadyReason: "ReconciliationSucceeded", Healthy: "True", HealthyReason: "Succeeded", HealthChecked: true},
			goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthHealthy, goclientnew.ReleaseLiveStatusOperationSucceeded},
		{"ready with no Healthy condition",
			fluxConditions{Ready: "True", ReadyReason: "ReconciliationSucceeded", HealthChecked: true},
			goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthHealthy, goclientnew.ReleaseLiveStatusOperationSucceeded},
		{"no health check",
			fluxConditions{Ready: "True", ReadyReason: "ReconciliationSucceeded"},
			goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthUnknown, goclientnew.ReleaseLiveStatusOperationSucceeded},
		{"running health checks",
			fluxConditions{Ready: "Unknown", ReadyReason: "Progressing", Healthy: "Unknown", HealthyReason: "Progressing", Reconciling: "True", HealthChecked: true},
			goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthProgressing, goclientnew.ReleaseLiveStatusOperationRunning},
		{"health check failed, retrying",
			fluxConditions{Ready: "False", ReadyReason: "HealthCheckFailed", Healthy: "False", HealthyReason: "HealthCheckFailed", Reconciling: "True", HealthChecked: true},
			goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthDegraded, goclientnew.ReleaseLiveStatusOperationFailed},
		{"re-apply of the same revision failed",
			fluxConditions{Ready: "False", ReadyReason: "ReconciliationFailed", Healthy: "True", HealthyReason: "Succeeded", HealthChecked: true},
			goclientnew.Unknown, goclientnew.ReleaseLiveStatusHealthHealthy, goclientnew.ReleaseLiveStatusOperationFailed},
	}
	for _, c := range cases {
		got := fromFluxRunning(c.in)
		expectStatus(t, c.name, got, c.sync, c.h, c.op)
		if got.ReporterSync != c.in.ReadyReason || got.ReporterHealth != c.in.HealthyReason || got.ReporterOperation != c.in.ReadyReason {
			t.Errorf("%s: did not keep Flux's reasons: %+v", c.name, got)
		}
	}
}

// A failed apply of a new Release is recorded on that Release, and the one the
// cluster still runs stays in sync.
func TestPlanFluxWritesFailedApply(t *testing.T) {
	source := fluxSource(rev(5), condition("Ready", "True", "Succeeded", "stored artifact"))
	k := kustomization("fx-app", true, rev(4), rev(5),
		condition("Ready", "False", "BuildFailed", "kustomize build failed: accumulating resources"),
		condition("Healthy", "True", "Succeeded", "Health check passed"))

	writes := writesByNum(planFluxWrites(source, []*unstructured.Unstructured{k}, publishedReleases(4, 5)))
	if len(writes) != 2 {
		t.Fatalf("writes = %+v, want Releases 4 and 5", writes)
	}
	expectStatus(t, "running Release 4", writes[4],
		goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthHealthy, goclientnew.ReleaseLiveStatusOperationSucceeded)
	if writes[4].Message != "" {
		t.Errorf("running Release 4 carries the failure: %q", writes[4].Message)
	}
	expectStatus(t, "failed Release 5", writes[5],
		goclientnew.OutOfSync, goclientnew.ReleaseLiveStatusHealthUnknown, goclientnew.ReleaseLiveStatusOperationFailed)
	if writes[5].Message != "Kustomization/flux-system/fx-app: kustomize build failed: accumulating resources" ||
		writes[5].ReporterOperation != "BuildFailed" {
		t.Errorf("failed Release 5 = %+v", writes[5])
	}
}

// A Release being applied is Running, while the one before it runs on.
func TestPlanFluxWritesApplyInProgress(t *testing.T) {
	source := fluxSource(rev(5), condition("Ready", "True", "Succeeded", "stored artifact"))
	k := kustomization("fx-app", true, rev(4), rev(5),
		condition("Reconciling", "True", "Progressing", "Building manifests for revision "+rev(5)),
		condition("Ready", "Unknown", "Progressing", "Reconciliation in progress"),
		condition("Healthy", "True", "Succeeded", "Health check passed"))

	writes := writesByNum(planFluxWrites(source, []*unstructured.Unstructured{k}, publishedReleases(4, 5)))
	expectStatus(t, "running Release 4", writes[4],
		goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthHealthy, goclientnew.ReleaseLiveStatusOperationSucceeded)
	expectStatus(t, "applying Release 5", writes[5],
		goclientnew.OutOfSync, goclientnew.ReleaseLiveStatusHealthUnknown, goclientnew.ReleaseLiveStatusOperationRunning)

	// Once applied and under health check, the cluster runs Release 5.
	k = kustomization("fx-app", true, rev(4), rev(5),
		condition("Reconciling", "True", "Progressing", "Running health checks for revision "+rev(5)),
		condition("Ready", "Unknown", "Progressing", "Reconciliation in progress"),
		condition("Healthy", "Unknown", "Progressing", "Running health checks for revision "+rev(5)))
	releases := publishedReleases(4, 5)
	releases[0].LiveStatus = &goclientnew.ReleaseLiveStatus{Reporter: liveStatusReporter, DataSource: "Kustomization/flux-system/fx-app",
		Sync: goclientnew.Synced, Health: goclientnew.ReleaseLiveStatusHealthHealthy}
	writes = writesByNum(planFluxWrites(source, []*unstructured.Unstructured{k}, releases))
	expectStatus(t, "checking Release 5", writes[5],
		goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthProgressing, goclientnew.ReleaseLiveStatusOperationRunning)
	expectStatus(t, "replaced Release 4", writes[4],
		goclientnew.OutOfSync, goclientnew.ReleaseLiveStatusHealthUnknown, "")
}

// A failed pull is recorded on the Release latest resolves to.
func TestPlanFluxWritesFailedPull(t *testing.T) {
	source := fluxSource(rev(4),
		condition("Ready", "False", "OCIPullFailed", "failed to pull artifact: unauthorized"))
	k := readyKustomization("fx-app", 4)
	releases := append(publishedReleases(4, 5), fluxRelease(6, false, nil)) // 6 was withdrawn

	writes := writesByNum(planFluxWrites(source, []*unstructured.Unstructured{k}, releases))
	if len(writes) != 2 {
		t.Fatalf("writes = %+v, want Releases 4 and 5", writes)
	}
	expectStatus(t, "running Release 4", writes[4],
		goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthHealthy, goclientnew.ReleaseLiveStatusOperationSucceeded)
	expectStatus(t, "unpulled Release 5", writes[5],
		goclientnew.OutOfSync, goclientnew.ReleaseLiveStatusHealthUnknown, goclientnew.ReleaseLiveStatusOperationFailed)
	if writes[5].Message != "OCIRepository/flux-system/fx-app: failed to pull artifact: unauthorized" ||
		writes[5].ReporterOperation != "OCIPullFailed" || writes[5].DataSource != "OCIRepository/flux-system/fx-app" {
		t.Errorf("unpulled Release 5 = %+v", writes[5])
	}

	// A pull failing after the newest Release arrived blames nothing else.
	source = fluxSource(rev(5),
		condition("Ready", "False", "OCIPullFailed", "failed to pull artifact: unauthorized"))
	writes = writesByNum(planFluxWrites(source, []*unstructured.Unstructured{readyKustomization("fx-app", 5)}, publishedReleases(5, 5)))
	if len(writes) != 1 {
		t.Fatalf("writes = %+v, want Release 5 only", writes)
	}
	expectStatus(t, "running Release 5", writes[5],
		goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthHealthy, goclientnew.ReleaseLiveStatusOperationSucceeded)
}

// Releases this repository reported on and no longer runs are marked replaced,
// keeping a failure, as are older published Releases nothing reported on; others
// are left alone.
func TestPlanFluxWritesMarksReplaced(t *testing.T) {
	status := func(dataSource string, sync goclientnew.ReleaseLiveStatusSync, health goclientnew.ReleaseLiveStatusHealth,
		op goclientnew.ReleaseLiveStatusOperation, message string) *goclientnew.ReleaseLiveStatus {
		return &goclientnew.ReleaseLiveStatus{Reporter: liveStatusReporter, DataSource: dataSource,
			Sync: sync, Health: health, Operation: op, Message: message}
	}
	healthy := func(dataSource string) *goclientnew.ReleaseLiveStatus {
		return status(dataSource, goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthHealthy, goclientnew.ReleaseLiveStatusOperationSucceeded, "")
	}
	releases := []goclientnew.Release{
		fluxRelease(1, true, healthy("flux-system/fx-app")), // named before kinds were
		fluxRelease(2, true, nil),                           // replaced before anything reported on it
		fluxRelease(3, true, status("Kustomization/flux-system/fx-app", goclientnew.OutOfSync, goclientnew.ReleaseLiveStatusHealthUnknown, "", "Replaced by release 4")),
		fluxRelease(4, true, status("Kustomization/flux-system/fx-app", goclientnew.OutOfSync, goclientnew.ReleaseLiveStatusHealthUnknown, goclientnew.ReleaseLiveStatusOperationFailed, "build failed")),
		fluxRelease(5, true, healthy("Kustomization/other-cluster/fx-app")), // another reporter's
		fluxRelease(6, true, nil),
		fluxRelease(7, true, status("Kustomization/flux-system/fx-app", goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthDegraded, goclientnew.ReleaseLiveStatusOperationFailed, "health check failed")),
		fluxRelease(8, true, status("flux-system/fx-app", goclientnew.OutOfSync, goclientnew.ReleaseLiveStatusHealthUnknown, "", "Replaced by release 4")),
		fluxRelease(9, true, nil), // newer than the one running
	}
	releases[4].LiveStatus.Reporter = "someone-else"
	releases[3].LiveStatus.ReporterOperation = "BuildFailed"
	releases[6].LiveStatus.ReporterOperation = "HealthCheckFailed"

	source := fluxSource(rev(6), condition("Ready", "True", "Succeeded", "stored artifact"))
	writes := planFluxWrites(source, []*unstructured.Unstructured{readyKustomization("fx-app", 6)}, releases)
	byNum := writesByNum(writes)

	cases := []struct {
		n               int64
		op              goclientnew.ReleaseLiveStatusOperation
		reporterOp, msg string
	}{
		{1, "", "", "Replaced by release 6"},
		{2, "", "", "Replaced by release 6"},
		{4, goclientnew.ReleaseLiveStatusOperationFailed, "BuildFailed", "Replaced by release 6, after it failed: build failed"},
		{7, goclientnew.ReleaseLiveStatusOperationFailed, "HealthCheckFailed", "Replaced by release 6, after it failed: health check failed"},
	}
	for _, c := range cases {
		got, ok := byNum[c.n]
		if !ok {
			t.Errorf("Release %d was not marked replaced", c.n)
			continue
		}
		expectStatus(t, fmt.Sprintf("Release %d", c.n), got, goclientnew.OutOfSync, goclientnew.ReleaseLiveStatusHealthUnknown, c.op)
		if got.Message != c.msg || got.ReporterOperation != c.reporterOp || got.DataSource != "Kustomization/flux-system/fx-app" ||
			got.Reporter != liveStatusReporter {
			t.Errorf("Release %d = %+v", c.n, got)
		}
	}
	if _, ok := byNum[3]; ok {
		t.Error("Release 3 was already marked replaced and must not be rewritten")
	}
	if _, ok := byNum[5]; ok {
		t.Error("Release 5 belongs to another reporter and must not be touched")
	}
	if got := byNum[8]; got.DataSource != "Kustomization/flux-system/fx-app" || got.Message != "Replaced by release 4" {
		t.Errorf("Release 8, marked with an older DataSource, = %+v; want only its DataSource rewritten", got)
	}
	if _, ok := byNum[9]; ok {
		t.Error("Release 9 is newer than the running Release and must not be marked replaced")
	}
	expectStatus(t, "running Release 6", byNum[6],
		goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthHealthy, goclientnew.ReleaseLiveStatusOperationSucceeded)
	for i := 1; i < len(writes); i++ {
		if writes[i-1].Release.ReleaseNum > writes[i].Release.ReleaseNum {
			t.Errorf("writes not in ReleaseNum order: %d before %d", writes[i-1].Release.ReleaseNum, writes[i].Release.ReleaseNum)
		}
	}

	// Nothing is marked while nothing runs: no replacement exists yet.
	k := kustomization("fx-app", true, "", rev(6),
		condition("Ready", "False", "BuildFailed", "kustomize build failed"))
	for _, w := range planFluxWrites(source, []*unstructured.Unstructured{k}, releases) {
		if w.Release.ReleaseNum != 6 {
			t.Errorf("Release %d written while nothing runs: %+v", w.Release.ReleaseNum, w.Status)
		}
	}
}

// Several Kustomizations on one Release combine into the least advanced state.
func TestPlanFluxWritesCombinesKustomizations(t *testing.T) {
	source := fluxSource(rev(5), condition("Ready", "True", "Succeeded", "stored artifact"))
	writes := writesByNum(planFluxWrites(source, []*unstructured.Unstructured{
		readyKustomization("b-app", 5),
		kustomization("a-broken", true, rev(4), rev(5),
			condition("Reconciling", "True", "ProgressingWithRetry", "Running health checks"),
			condition("Ready", "False", "HealthCheckFailed", "health check failed: [Deployment/fx/broken status: 'Failed']"),
			condition("Healthy", "False", "HealthCheckFailed", "failed early")),
		kustomization("c-new", true, "", "",
			condition("Ready", "Unknown", "Progressing", "Reconciliation in progress")),
	}, publishedReleases(5, 5)))

	if len(writes) != 1 {
		t.Fatalf("writes = %+v, want Release 5 only", writes)
	}
	got := writes[5]
	expectStatus(t, "Release 5", got,
		goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthDegraded, goclientnew.ReleaseLiveStatusOperationFailed)
	if got.ReporterHealth != "HealthCheckFailed" {
		t.Errorf("ReporterHealth = %q, want the failing Kustomization's reason", got.ReporterHealth)
	}
	if got.Message != "Kustomization/flux-system/a-broken: health check failed: [Deployment/fx/broken status: 'Failed']" {
		t.Errorf("Message = %q", got.Message)
	}
	if got.DataSource != "Kustomization/flux-system/a-broken,Kustomization/flux-system/b-app" || got.Reporter != liveStatusReporter {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if !got.ObservedAt.IsZero() {
		t.Errorf("ObservedAt = %v, want zero", got.ObservedAt)
	}
}
