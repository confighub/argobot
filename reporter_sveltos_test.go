// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"testing"

	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
)

var (
	testSpaceID  = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	otherSpaceID = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	unitNS       = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	unitDeploy   = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002")
)

// collected encodes an object as a HealthCheckReport carries it, stamped with a
// confighub.com/origin annotation unless o is nil.
func collected(t *testing.T, kind, namespace, name string, o *origin) string {
	t.Helper()
	metadata := map[string]any{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	if o != nil {
		value, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		metadata["annotations"] = map[string]any{originAnnotation: string(value)}
	}
	raw, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": kind, "metadata": metadata})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func healthReport(name string, statuses ...map[string]any) *unstructured.Unstructured {
	entries := make([]any, len(statuses))
	for i, s := range statuses {
		entries[i] = s
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "lib.projectsveltos.io/v1beta1",
		"kind":       "HealthCheckReport",
		"metadata":   map[string]any{"name": name, "namespace": "projectsveltos"},
		"spec": map[string]any{
			"clusterNamespace": "mgmt",
			"clusterName":      "mgmt",
			"clusterType":      "Sveltos",
			"healthCheckName":  "release-test-health",
			"resourceStatuses": entries,
		},
	}}
}

func TestObserveSpaces(t *testing.T) {
	report := healthReport("r",
		map[string]any{
			"resource":     collected(t, "Namespace", "", "release-test", &origin{SpaceID: testSpaceID.String(), SpaceSlug: "release-test", UnitID: unitNS.String(), RevisionNum: 2}),
			"healthStatus": "Healthy",
		},
		map[string]any{
			"resource":     collected(t, "Deployment", "release-test", "web", &origin{SpaceID: testSpaceID.String(), SpaceSlug: "release-test", UnitID: unitDeploy.String(), RevisionNum: 5}),
			"healthStatus": "Progressing",
			"message":      "ready 3/4",
		},
		// Another Space, caught by a cluster-wide selector.
		map[string]any{
			"resource":     collected(t, "Namespace", "", "other", &origin{SpaceID: otherSpaceID.String(), SpaceSlug: "other", UnitID: unitNS.String(), RevisionNum: 1}),
			"healthStatus": "Healthy",
		},
		// No origin: not a ConfigHub object.
		map[string]any{"resource": collected(t, "Namespace", "", "kube-system", nil), "healthStatus": "Healthy"},
		// Not collected.
		map[string]any{"healthStatus": "Healthy"},
		// Not base64.
		map[string]any{"resource": "!!!", "healthStatus": "Healthy"},
	)

	got := observeSpaces(report)
	if len(got) != 2 {
		t.Fatalf("observeSpaces() found %d Spaces, want 2: %v", len(got), got)
	}
	obs := got[testSpaceID]
	if obs == nil || obs.SpaceSlug != "release-test" {
		t.Fatalf("observation of %s = %+v", testSpaceID, obs)
	}
	wantRevs := map[unitRevision]bool{{unitNS, 2}: true, {unitDeploy, 5}: true}
	if len(obs.Revisions) != len(wantRevs) {
		t.Errorf("Revisions = %v, want %v", obs.Revisions, wantRevs)
	}
	for rev := range wantRevs {
		if !obs.Revisions[rev] {
			t.Errorf("Revisions missing %v: %v", rev, obs.Revisions)
		}
	}
	if len(obs.Resources) != 2 || obs.Resources[1] != (resourceHealth{
		Kind: "Deployment", Namespace: "release-test", Name: "web", HealthStatus: "Progressing", Message: "ready 3/4",
	}) {
		t.Errorf("Resources = %+v", obs.Resources)
	}
}

func TestNewestCoveringRelease(t *testing.T) {
	releaseID := func(n int) uuid.UUID {
		return uuid.MustParse("bbbbbbbb-0000-0000-0000-00000000000" + string(rune('0'+n)))
	}
	release := func(n int) goclientnew.Release {
		return goclientnew.Release{ReleaseID: releaseID(n), ReleaseNum: int64(n)}
	}
	// The measured case: Release 1 bundles Namespace revision 2 and Deployment
	// revision 2; Releases 2 and 3 bundle Namespace revision 2 and Deployment
	// revision 3. Release 2 is withdrawn, so only 3 and 1 are published (newest
	// first), and 3 is the match.
	bundles := map[uuid.UUID]map[unitRevision]bool{
		releaseID(1): {{unitNS, 2}: true, {unitDeploy, 2}: true},
		releaseID(2): {{unitNS, 2}: true, {unitDeploy, 3}: true},
		releaseID(3): {{unitNS, 2}: true, {unitDeploy, 3}: true},
	}
	var read []uuid.UUID
	bundleOf := func(id uuid.UUID) (map[unitRevision]bool, error) {
		read = append(read, id)
		return bundles[id], nil
	}
	observed := map[unitRevision]bool{{unitNS, 2}: true, {unitDeploy, 3}: true}
	published := []goclientnew.Release{release(3), release(1)}
	if got, ok, err := newestCoveringRelease(published, observed, bundleOf); err != nil || !ok || got != releaseID(3) {
		t.Errorf("newestCoveringRelease() = %v, %v, %v; want release 3", got, ok, err)
	}
	if len(read) != 1 {
		t.Errorf("read %d bundles, want only the newest: %v", len(read), read)
	}

	// With 3 withdrawn as well, the remaining Release does not bundle Deployment
	// revision 3, so nothing matches.
	if got, ok, _ := newestCoveringRelease([]goclientnew.Release{release(1)}, observed, bundleOf); ok {
		t.Errorf("newestCoveringRelease() = %v, want no match", got)
	}

	// A revision no bundle holds matches nothing, and neither does a Release
	// whose bundle could not be read.
	observed[unitRevision{unitDeploy, 4}] = true
	if got, ok, _ := newestCoveringRelease(published, observed, bundleOf); ok {
		t.Errorf("newestCoveringRelease() = %v, want no match", got)
	}
	if got, ok, _ := newestCoveringRelease([]goclientnew.Release{release(9)}, map[unitRevision]bool{{unitNS, 2}: true}, bundleOf); ok {
		t.Errorf("newestCoveringRelease() = %v, want no match for an unreadable bundle", got)
	}
}

func TestBundleRevisions(t *testing.T) {
	annotated := func(kind, name string, o origin) string {
		value, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		return "apiVersion: v1\nkind: " + kind + "\nmetadata:\n  name: " + name +
			"\n  annotations:\n    confighub.com/origin: '" + string(value) + "'\n"
	}
	files := map[string]string{
		"release-test-namespace.yaml": annotated("Namespace", "release-test", origin{UnitID: unitNS.String(), RevisionNum: 2}),
		// Two objects in one file, one without an origin.
		"release-test-deployment.yaml": annotated("Deployment", "web", origin{UnitID: unitDeploy.String(), RevisionNum: 4}) +
			"---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: plain\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := bundleRevisions(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := map[unitRevision]bool{{unitNS, 2}: true, {unitDeploy, 4}: true}
	if len(got) != len(want) {
		t.Fatalf("bundleRevisions() = %v, want %v", got, want)
	}
	for rev := range want {
		if !got[rev] {
			t.Errorf("bundleRevisions() missing %v: %v", rev, got)
		}
	}

	if _, err := bundleRevisions([]byte("not gzip")); err == nil {
		t.Error("bundleRevisions() of a non-gzip body: want an error")
	}
}

func TestNamesCluster(t *testing.T) {
	collectedCopy := healthReport("sveltos--release-test-health--mgmt")
	if !namesCluster(collectedCopy) {
		t.Error("namesCluster() = false for the collected copy, which names its cluster")
	}

	// The agent's copy, as seen in projectsveltos: the cluster fields are empty.
	agentCopy := healthReport("release-test-health")
	spec := agentCopy.Object["spec"].(map[string]any)
	for _, field := range []string{"clusterNamespace", "clusterName", "clusterType"} {
		spec[field] = ""
	}
	if namesCluster(agentCopy) {
		t.Error("namesCluster() = true for the agent's copy, which names no cluster")
	}
}

func TestReleaseMatchKeyChanges(t *testing.T) {
	revs := map[unitRevision]bool{{unitNS, 2}: true, {unitDeploy, 3}: true}
	r3 := goclientnew.Release{ReleaseID: uuid.New()}
	r4 := goclientnew.Release{ReleaseID: uuid.New()}

	a := releaseMatchKey(revs, []goclientnew.Release{r3})
	if b := releaseMatchKey(revs, []goclientnew.Release{r3}); a != b {
		t.Errorf("key not stable: %q vs %q", a, b)
	}
	if b := releaseMatchKey(revs, []goclientnew.Release{r4, r3}); a == b {
		t.Error("key unchanged by a new published Release")
	}
	if b := releaseMatchKey(map[unitRevision]bool{{unitNS, 2}: true, {unitDeploy, 4}: true}, []goclientnew.Release{r3}); a == b {
		t.Error("key unchanged by a new observed revision")
	}
}

func TestCombineFeatureStatuses(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"Provisioned"}, "Provisioned"},
		{[]string{"Provisioned", "Provisioning"}, "Provisioning"},
		{[]string{"Provisioning", "FailedNonRetriable", "Provisioned"}, "FailedNonRetriable"},
		{[]string{"Provisioned", "Removing"}, "Removing"},
	}
	for _, c := range cases {
		if got := combineFeatureStatuses(c.in); got != c.want {
			t.Errorf("combineFeatureStatuses(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFromSveltos(t *testing.T) {
	cases := []struct {
		feature, health string
		sync            goclientnew.ReleaseLiveStatusSync
		h               goclientnew.ReleaseLiveStatusHealth
		op              goclientnew.ReleaseLiveStatusOperation
	}{
		{"Provisioned", "Healthy", goclientnew.Synced, goclientnew.ReleaseLiveStatusHealthHealthy, goclientnew.ReleaseLiveStatusOperationSucceeded},
		{"Provisioning", "Progressing", goclientnew.Unknown, goclientnew.ReleaseLiveStatusHealthProgressing, goclientnew.ReleaseLiveStatusOperationRunning},
		{"Failed", "Degraded", goclientnew.Unknown, goclientnew.ReleaseLiveStatusHealthDegraded, goclientnew.ReleaseLiveStatusOperationFailed},
		{"FailedNonRetriable", "Suspended", goclientnew.Unknown, goclientnew.ReleaseLiveStatusHealthSuspended, goclientnew.ReleaseLiveStatusOperationFailed},
		{"", "", goclientnew.Unknown, goclientnew.ReleaseLiveStatusHealthUnknown, ""},
	}
	for _, c := range cases {
		got := fromSveltos(c.feature, c.health)
		if got.Sync != c.sync || got.Health != c.h || got.Operation != c.op {
			t.Errorf("fromSveltos(%q, %q) = %+v", c.feature, c.health, got)
		}
		if got.ReporterSync != c.feature || got.ReporterOperation != c.feature || got.ReporterHealth != c.health {
			t.Errorf("fromSveltos(%q, %q) did not keep Sveltos's words: %+v", c.feature, c.health, got)
		}
	}
}

func TestProjectSveltosStatus(t *testing.T) {
	resources := []resourceHealth{
		{Kind: "Namespace", Name: "release-test", HealthStatus: "Healthy"},
		{Kind: "Deployment", Name: "web", HealthStatus: "Progressing", Message: "ready 3/4"},
		{Kind: "Deployment", Name: "api", HealthStatus: "Progressing", Message: "ready 0/1"},
	}
	got := projectSveltosStatus(resources, "Provisioned", "release-test-mgmt")
	if got.Health != goclientnew.ReleaseLiveStatusHealthProgressing || got.ReporterHealth != "Progressing" {
		t.Errorf("health fields wrong: %+v", got)
	}
	if got.Message != "Deployment/web: ready 3/4" {
		t.Errorf("Message = %q", got.Message)
	}
	if got.Reporter != liveStatusReporter || got.DataSource != "release-test-mgmt" {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if !got.ObservedAt.IsZero() {
		t.Errorf("ObservedAt = %v, want zero", got.ObservedAt)
	}

	// A worse state later in the list wins, with its own message.
	resources = append(resources, resourceHealth{Kind: "Service", Name: "web", HealthStatus: "Degraded", Message: "no endpoints"})
	got = projectSveltosStatus(resources, "Provisioned", "p")
	if got.Health != goclientnew.ReleaseLiveStatusHealthDegraded || got.Message != "Service/web: no endpoints" {
		t.Errorf("worst health not reported: %+v", got)
	}
}

func TestDeployStatus(t *testing.T) {
	summary := func(name, owner, clusterName, url string, statuses ...string) *unstructured.Unstructured {
		features := make([]any, len(statuses))
		for i, s := range statuses {
			features[i] = map[string]any{"featureID": "Resources", "status": s}
		}
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "config.projectsveltos.io/v1beta1",
			"kind":       "ClusterSummary",
			"metadata":   map[string]any{"name": name, "namespace": "mgmt"},
			"spec": map[string]any{
				"clusterNamespace": "mgmt",
				"clusterName":      clusterName,
				"clusterType":      "Sveltos",
				"clusterProfileSpec": map[string]any{
					"policyRefs": []any{map[string]any{"remoteURL": map[string]any{"url": url}}},
				},
			},
			"status": map[string]any{"featureSummaries": features},
		}}
		if owner != "" {
			u.Object["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{
				"apiVersion": "config.projectsveltos.io/v1beta1", "kind": "ClusterProfile", "name": owner, "uid": "u",
			}}
		}
		return u
	}

	informer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
	for _, s := range []*unstructured.Unstructured{
		summary("release-test-mgmt-sveltos-mgmt", "release-test-mgmt", "mgmt", "oci://gw/space/sveltos-release-2453:latest", "Provisioned"),
		summary("by-name", "sveltos-variant", "mgmt", "oci://gw/space/elsewhere", "Provisioning"),
		summary("other-cluster", "other", "prod", "oci://gw/space/sveltos-release-2453:latest", "Failed"),
	} {
		if err := informer.GetStore().Add(s); err != nil {
			t.Fatal(err)
		}
	}
	r := &sveltosReporter{summaries: informer}
	mgmt := clusterRef{Namespace: "mgmt", Name: "mgmt", Type: "sveltos"}

	// Matched by the remoteURL; the other cluster's ClusterSummary is ignored.
	if status, name := r.deployStatus(mgmt, "sveltos-release-2453"); status != "Provisioned" || name != "release-test-mgmt" {
		t.Errorf("deployStatus by remoteURL = %q, %q", status, name)
	}
	// Matched by the profile named after the Space.
	if status, name := r.deployStatus(mgmt, "sveltos-variant"); status != "Provisioning" || name != "sveltos-variant" {
		t.Errorf("deployStatus by name = %q, %q", status, name)
	}
	if status, name := r.deployStatus(mgmt, "unknown"); status != "" || name != "" {
		t.Errorf("deployStatus of an undeployed Space = %q, %q", status, name)
	}
}
