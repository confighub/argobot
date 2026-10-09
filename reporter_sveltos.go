// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/confighub/argobot/kube"
	"github.com/confighub/argobot/sveltos"
	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// originAnnotation is stamped on every object of a published Release, naming the
// Space, Unit and Revision it came from (k8skit.OriginAnnotation in the confighub
// repo).
const originAnnotation = "confighub.com/origin"

// origin is the JSON value of the confighub.com/origin annotation
// (k8skit.Origin in the confighub repo).
type origin struct {
	SpaceID     string `json:"spaceId"`
	SpaceSlug   string `json:"spaceSlug"`
	UnitID      string `json:"unitId"`
	UnitSlug    string `json:"unitSlug"`
	RevisionNum int64  `json:"revisionNum"`
}

// unitRevision is one Unit at one Revision, as observed on the cluster.
type unitRevision struct {
	UnitID      uuid.UUID
	RevisionNum int64
}

// resourceHealth is one collected object's health, as a HealthCheck judged it.
type resourceHealth struct {
	Kind, Namespace, Name string
	HealthStatus, Message string
}

// spaceObservation is what one HealthCheckReport shows of one Space: the Unit
// revisions running, and the health of the objects that carry them.
type spaceObservation struct {
	SpaceSlug string
	Revisions map[unitRevision]bool
	Resources []resourceHealth
}

// releaseMatch caches the Release a Space's observed revisions resolved to, for as
// long as neither those revisions nor the Space's published Releases change.
type releaseMatch struct {
	key       string
	releaseID uuid.UUID
}

// sveltosReporter writes the live status of Releases that Sveltos deploys.
//
// Argo CD records the digest it synced, which names the Release; Sveltos records
// no digest, and its featureSummaries[].lastAppliedTime does not move when it
// applies a new Release by polling. So the reporter reads the deployed objects
// themselves. A HealthCheck with collectResources: true, run by a
// ClusterHealthCheck, makes Sveltos write a HealthCheckReport per cluster holding
// each collected object and its health. Every object of a Release carries
// confighub.com/origin, naming its Space, Unit and Revision, in the Release's
// bundle as on the cluster. So the Release a cluster runs is the newest published
// Release of the Space whose bundle holds every observed Unit revision. Several
// Releases can match, since a Unit that did not change keeps
// its revision; they bundle the same content, so the newest is reported on.
// Withdrawn Releases are skipped. Because the Release is identified from what is
// running, a publish landing between Sveltos's fetch and its apply cannot cause a
// wrong match.
//
// Health comes from the report. Sync and operation come from the cluster's
// ClusterSummary for the profile that deploys the Space: the one whose remoteURL
// names the Space, or whose profile is named after it.
//
// The HealthCheck must select at least one object of every Unit, or the revision
// of an unselected Unit is unknown and no Release may match. A report can hold
// objects of several Spaces (a cluster-wide selector catches them all); each
// Space's Release is written separately.
//
// There is one report per cluster but one LiveStatus per Release. With one
// profile per variant per cluster, as cub cluster up and sveltos-confighub
// arrange, these coincide. A Release that several clusters run is written by
// each of their reports in turn, so its LiveStatus shows whichever reported
// last.
type sveltosReporter struct {
	liveStatusWriter
	reports   cache.SharedIndexInformer
	summaries cache.SharedIndexInformer
	queue     workqueue.TypedRateLimitingInterface[string]

	// lastSig deduplicates writes: report key -> Space id -> signature of the last
	// projection written (the Release and the encoded status, ObservedAt omitted).
	lastSig map[string]map[uuid.UUID]string
	// releaseMatches caches, per Space, the Release its observed revisions last
	// resolved to. Touched only by the single worker goroutine, so it needs no
	// lock.
	releaseMatches map[uuid.UUID]releaseMatch
	// bundleRevisions caches, per Release, the Unit revisions its bundle holds.
	// A Release's bundle never changes, so an entry is never stale. Touched only
	// by the worker goroutine.
	bundleRevisions map[uuid.UUID]map[unitRevision]bool
}

// newSveltosReporter builds the Sveltos live-status reporter: a dynamic
// Kubernetes client (in-cluster or kubeconfig), a ConfigHub API client under the
// worker identity, and informers over HealthCheckReports and ClusterSummaries in
// every namespace (where Sveltos puts a remote cluster's reports is not fixed).
func newSveltosReporter(cfg config) (*sveltosReporter, error) {
	dyn, err := kube.NewDynamicClient()
	if err != nil {
		return nil, err
	}

	writer, err := newLiveStatusWriter(cfg)
	if err != nil {
		return nil, err
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dyn, liveStatusResync, metav1.NamespaceAll, nil)
	r := &sveltosReporter{
		liveStatusWriter: writer,
		reports:          factory.ForResource(sveltos.HealthCheckReportsGVR).Informer(),
		summaries:        factory.ForResource(sveltos.ClusterSummariesGVR).Informer(),
		queue:            workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		lastSig:          make(map[string]map[uuid.UUID]string),
		releaseMatches:   make(map[uuid.UUID]releaseMatch),
		bundleRevisions:  make(map[uuid.UUID]map[unitRevision]bool),
	}

	if _, err := r.reports.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { r.enqueue(obj) },
		UpdateFunc: func(_, obj any) { r.enqueue(obj) },
		// DeleteFunc: a deleted report leaves its Releases' last status in place,
		// which still describes them as they were last seen running.
	}); err != nil {
		_ = writer.frontdoor.Close()
		return nil, fmt.Errorf("register HealthCheckReport informer handler: %w", err)
	}
	// A ClusterSummary carries the sync and operation of its cluster's reports.
	if _, err := r.summaries.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { r.enqueueClusterReports(obj) },
		UpdateFunc: func(_, obj any) { r.enqueueClusterReports(obj) },
	}); err != nil {
		_ = writer.frontdoor.Close()
		return nil, fmt.Errorf("register ClusterSummary informer handler: %w", err)
	}

	return r, nil
}

// Run starts the informers and a single worker, blocking until ctx is cancelled.
func (r *sveltosReporter) Run(ctx context.Context) error {
	defer r.frontdoor.Close()

	go r.reports.Run(ctx.Done())
	go r.summaries.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), r.reports.HasSynced, r.summaries.HasSynced) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Best-effort: a failed cache sync disables reporting for this run but
		// must not cancel the event consumers, so return nil rather than an error.
		log.Printf("[WARN] live-status reporter: Sveltos informer caches failed to sync; reporting disabled")
		return nil
	}
	log.Printf("[INFO] live-status reporter: watching Sveltos HealthCheckReports")

	go func() {
		for r.processNext(ctx) {
		}
	}()

	<-ctx.Done()
	r.queue.ShutDown()
	return ctx.Err()
}

func (r *sveltosReporter) processNext(ctx context.Context) bool {
	key, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer r.queue.Done(key)

	if err := r.report(ctx, key); err != nil {
		log.Printf("[WARN] live-status reporter: %v", err)
		r.queue.AddRateLimited(key)
		return true
	}
	r.queue.Forget(key)
	return true
}

// enqueue schedules a report after the debounce window, coalescing a burst of
// updates into one write.
func (r *sveltosReporter) enqueue(obj any) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Printf("[WARN] live-status reporter: deriving key: %v", err)
		return
	}
	r.queue.AddAfter(key, liveStatusDebounce)
}

// enqueueClusterReports schedules every report of the ClusterSummary's cluster.
func (r *sveltosReporter) enqueueClusterReports(obj any) {
	summary, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	cluster := clusterOf(summary)
	for _, item := range r.reports.GetStore().List() {
		if report, ok := item.(*unstructured.Unstructured); ok && clusterOf(report) == cluster {
			r.enqueue(report)
		}
	}
}

// report projects the HealthCheckReport named by key and, for each Space whose
// objects it collected, writes the status to the LiveStatus of the Release that
// Space is running, if it changed since the last write. Spaces or Releases this
// worker cannot see are ignored.
func (r *sveltosReporter) report(ctx context.Context, key string) error {
	obj, exists, err := r.reports.GetStore().GetByKey(key)
	if err != nil {
		return fmt.Errorf("look up %s: %w", key, err)
	}
	if !exists {
		// Deleted; see the DeleteFunc note in newSveltosReporter.
		delete(r.lastSig, key)
		return nil
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected object type %T for %s", obj, key)
	}
	if !namesCluster(u) {
		// The agent's own copy, which Sveltos also collects into the management
		// cluster as a copy that names the cluster. Reporting from both would
		// let whichever is processed last decide the LiveStatus, and this one
		// cannot be matched to a ClusterSummary.
		return nil
	}

	observations := observeSpaces(u)
	spaceIDs := make([]uuid.UUID, 0, len(observations))
	for spaceID := range observations {
		spaceIDs = append(spaceIDs, spaceID)
	}
	slices.SortFunc(spaceIDs, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })

	var errs []error
	for _, spaceID := range spaceIDs {
		if err := r.reportSpace(ctx, key, u, spaceID, observations[spaceID]); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("report %s: %v", key, errs)
	}
	return nil
}

// reportSpace writes one Space's part of a report to the Release it is running.
func (r *sveltosReporter) reportSpace(ctx context.Context, key string, report *unstructured.Unstructured,
	spaceID uuid.UUID, obs *spaceObservation) error {
	releaseID, known, err := r.findRelease(ctx, spaceID, obs.Revisions)
	if err != nil {
		return err
	}
	if !known {
		// Not a Space this worker can see, or revisions no published Release
		// bundles: nothing to record it on.
		return nil
	}

	featureStatus, dataSource := r.deployStatus(clusterOf(report), obs.SpaceSlug)
	if dataSource == "" {
		dataSource = report.GetName()
	}
	status := projectSveltosStatus(obs.Resources, featureStatus, dataSource)

	// Deduplicate on the Release and everything except ObservedAt (which changes
	// every pass), so an unchanged report produces no write.
	encoded, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal status signature: %w", err)
	}
	sig := releaseID.String() + " " + string(encoded)
	if r.lastSig[key][spaceID] == sig {
		return nil
	}

	status.ObservedAt = time.Now().UTC()
	if err := r.patchRelease(ctx, spaceID, releaseID, status); err != nil {
		return err
	}
	if r.lastSig[key] == nil {
		r.lastSig[key] = make(map[uuid.UUID]string)
	}
	r.lastSig[key][spaceID] = sig
	log.Printf("[INFO] live-status: %s -> release %s of space %s (sync=%s health=%s)",
		dataSource, releaseID, spaceID, status.Sync, status.Health)
	return nil
}

// findRelease resolves the Release a Space is running from the Unit revisions
// observed on the cluster: the newest published Release whose bundle holds every
// one of them. A Space the worker cannot read is not known, and neither is a set
// of revisions no published Release covers.
//
// The bundle is what the cluster pulled, and the worker can read it: a Release
// is authorized by its Target, which the worker is granted on. A Unit's
// revisions are not, so their Tags cannot be used.
func (r *sveltosReporter) findRelease(ctx context.Context, spaceID uuid.UUID, revisions map[unitRevision]bool) (uuid.UUID, bool, error) {
	if len(revisions) == 0 {
		return uuid.Nil, false, nil
	}
	cub, err := r.client()
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("list the releases of space %s: %w", spaceID, err)
	}

	selectFields := "ReleaseID,ReleaseNum,Published,SpaceID,OrganizationID"
	resp, err := cub.ListExtendedReleasesWithResponse(ctx, spaceID,
		&goclientnew.ListExtendedReleasesParams{Select: &selectFields})
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("list the releases of space %s: %w", spaceID, err)
	}
	if resp.JSON200 == nil {
		if code := resp.HTTPResponse.StatusCode; code == http.StatusNotFound || code == http.StatusForbidden {
			// A Space this worker cannot see -- not ours.
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("list the releases of space %s: unexpected HTTP %d: %s",
			spaceID, resp.HTTPResponse.StatusCode, string(resp.Body))
	}
	var published []goclientnew.Release
	for _, er := range *resp.JSON200 {
		if er.Release != nil && er.Release.Published {
			published = append(published, *er.Release)
		}
	}
	slices.SortFunc(published, func(a, b goclientnew.Release) int { return cmp.Compare(b.ReleaseNum, a.ReleaseNum) })

	matchKey := releaseMatchKey(revisions, published)
	if m, ok := r.releaseMatches[spaceID]; ok && m.key == matchKey {
		return m.releaseID, true, nil
	}

	releaseID, ok, err := newestCoveringRelease(published, revisions, func(releaseID uuid.UUID) (map[unitRevision]bool, error) {
		return r.releaseBundleRevisions(ctx, spaceID, releaseID)
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	if !ok {
		return uuid.Nil, false, nil
	}
	r.releaseMatches[spaceID] = releaseMatch{key: matchKey, releaseID: releaseID}
	return releaseID, true, nil
}

// releaseBundleRevisions returns the Unit revisions a Release's bundle holds,
// read from the confighub.com/origin annotation of each object in it. A bundle
// is immutable, so it is downloaded once per Release. A Release the worker
// cannot read holds nothing it can match.
func (r *sveltosReporter) releaseBundleRevisions(ctx context.Context, spaceID, releaseID uuid.UUID) (map[unitRevision]bool, error) {
	if revs, ok := r.bundleRevisions[releaseID]; ok {
		return revs, nil
	}
	cub, err := r.client()
	if err != nil {
		return nil, fmt.Errorf("read the bundle of release %s: %w", releaseID, err)
	}
	resp, err := cub.DownloadReleaseDataWithResponse(ctx, spaceID, releaseID)
	if err != nil {
		return nil, fmt.Errorf("read the bundle of release %s: %w", releaseID, err)
	}
	switch code := resp.HTTPResponse.StatusCode; {
	case code == http.StatusNotFound || code == http.StatusForbidden:
		return nil, nil
	case code != http.StatusOK:
		return nil, fmt.Errorf("read the bundle of release %s: unexpected HTTP %d: %s",
			releaseID, code, string(resp.Body))
	}
	revs, err := bundleRevisions(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read the bundle of release %s: %w", releaseID, err)
	}
	r.bundleRevisions[releaseID] = revs
	return revs, nil
}

// bundleRevisions returns the Unit revisions in a Release bundle (a gzipped tar
// of YAML files), from each object's confighub.com/origin annotation. Objects
// without one are skipped.
func bundleRevisions(bundle []byte) (map[unitRevision]bool, error) {
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	revs := map[unitRevision]bool{}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return revs, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		decoder := utilyaml.NewYAMLOrJSONDecoder(tr, 4096)
		for {
			var obj map[string]any
			if err := decoder.Decode(&obj); err == io.EOF {
				break
			} else if err != nil {
				return nil, fmt.Errorf("%s: %w", header.Name, err)
			}
			value, _, _ := unstructured.NestedString(obj, "metadata", "annotations", originAnnotation)
			if value == "" {
				continue
			}
			var o origin
			if err := json.Unmarshal([]byte(value), &o); err != nil {
				continue
			}
			unitID, err := uuid.Parse(o.UnitID)
			if err != nil {
				continue
			}
			revs[unitRevision{UnitID: unitID, RevisionNum: o.RevisionNum}] = true
		}
	}
}

// newestCoveringRelease returns the first of published (newest first) whose
// bundle, as bundleOf reads it, holds every revision in observed.
func newestCoveringRelease(published []goclientnew.Release, observed map[unitRevision]bool,
	bundleOf func(releaseID uuid.UUID) (map[unitRevision]bool, error)) (uuid.UUID, bool, error) {
	for _, release := range published {
		inBundle, err := bundleOf(release.ReleaseID)
		if err != nil {
			return uuid.Nil, false, err
		}
		covers := true
		for rev := range observed {
			if !inBundle[rev] {
				covers = false
				break
			}
		}
		if covers {
			return release.ReleaseID, true, nil
		}
	}
	return uuid.Nil, false, nil
}

// releaseMatchKey identifies the inputs to a Release match: the observed
// revisions and the Space's published Releases. A publish or a withdrawal changes
// it, and so does a new revision reaching the cluster.
func releaseMatchKey(revisions map[unitRevision]bool, published []goclientnew.Release) string {
	parts := make([]string, 0, len(revisions)+len(published))
	for rev := range revisions {
		parts = append(parts, fmt.Sprintf("%s@%d", rev.UnitID, rev.RevisionNum))
	}
	slices.Sort(parts)
	for _, release := range published {
		parts = append(parts, release.ReleaseID.String())
	}
	return strings.Join(parts, ",")
}

// deployStatus returns the feature status of the ClusterSummaries on cluster
// whose profile deploys the Space spaceSlug, combined into the least advanced
// one, and the name of that profile. Both are empty when there is none.
func (r *sveltosReporter) deployStatus(cluster clusterRef, spaceSlug string) (featureStatus, profileName string) {
	var statuses []string
	var names []string
	for _, item := range r.summaries.GetStore().List() {
		summary, ok := item.(*unstructured.Unstructured)
		if !ok || clusterOf(summary) != cluster {
			continue
		}
		name := summaryProfileName(summary)
		deploys := name == spaceSlug || slices.ContainsFunc(
			sveltos.RemoteURLs(summary.Object, "spec", "clusterProfileSpec"),
			func(u string) bool { return spaceSlugFromRepoURL(u) == spaceSlug })
		if !deploys {
			continue
		}
		names = append(names, name)
		features, _, _ := unstructured.NestedSlice(summary.Object, "status", "featureSummaries")
		for _, f := range features {
			if m, ok := f.(map[string]any); ok {
				status, _, _ := unstructured.NestedString(m, "status")
				statuses = append(statuses, status)
			}
		}
	}
	slices.Sort(names)
	if len(names) > 0 {
		profileName = names[0]
	}
	return combineFeatureStatuses(statuses), profileName
}

// combineFeatureStatuses returns the least advanced of a profile's feature
// statuses: a failure outranks a deploy under way, which outranks anything but
// Provisioned. Provisioned is reported only when every feature is.
func combineFeatureStatuses(statuses []string) string {
	rank := func(s string) int {
		switch s {
		case "Provisioned":
			return 0
		case "Provisioning":
			return 2
		case "Failed", "FailedNonRetriable":
			return 3
		}
		return 1
	}
	combined := ""
	for i, s := range statuses {
		if i == 0 || rank(s) > rank(combined) {
			combined = s
		}
	}
	return combined
}

// summaryProfileName is the name of the ClusterProfile or Profile that owns a
// ClusterSummary, or the ClusterSummary's own name when no owner says.
func summaryProfileName(summary *unstructured.Unstructured) string {
	for _, owner := range summary.GetOwnerReferences() {
		if owner.Kind == "ClusterProfile" || owner.Kind == "Profile" {
			return owner.Name
		}
	}
	return summary.GetName()
}

// clusterRef names a cluster as Sveltos's reports and ClusterSummaries do.
type clusterRef struct {
	Namespace, Name, Type string
}

// clusterOf reads the cluster a HealthCheckReport or ClusterSummary is about.
// namesCluster reports whether a HealthCheckReport says which cluster it
// describes. The Sveltos agent writes its report in projectsveltos on the cluster
// it checks with the cluster fields empty; the copy Sveltos collects into the
// cluster's namespace on the management cluster fills them in.
func namesCluster(u *unstructured.Unstructured) bool {
	c := clusterOf(u)
	return c.Namespace != "" && c.Name != ""
}

func clusterOf(u *unstructured.Unstructured) clusterRef {
	ns, _, _ := unstructured.NestedString(u.Object, "spec", "clusterNamespace")
	name, _, _ := unstructured.NestedString(u.Object, "spec", "clusterName")
	typ, _, _ := unstructured.NestedString(u.Object, "spec", "clusterType")
	return clusterRef{Namespace: ns, Name: name, Type: strings.ToLower(typ)}
}

// observeSpaces reads a HealthCheckReport's collected objects, grouping by Space
// the Unit revisions they carry and their health. Objects without a usable
// confighub.com/origin annotation, or not collected, are skipped.
func observeSpaces(report *unstructured.Unstructured) map[uuid.UUID]*spaceObservation {
	observations := map[uuid.UUID]*spaceObservation{}
	entries, _, _ := unstructured.NestedSlice(report.Object, "spec", "resourceStatuses")
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		encoded, _, _ := unstructured.NestedString(m, "resource")
		obj, o, ok := decodeCollectedResource(encoded)
		if !ok {
			continue
		}
		spaceID, err := uuid.Parse(o.SpaceID)
		if err != nil {
			continue
		}
		unitID, err := uuid.Parse(o.UnitID)
		if err != nil {
			continue
		}
		obs := observations[spaceID]
		if obs == nil {
			obs = &spaceObservation{SpaceSlug: o.SpaceSlug, Revisions: map[unitRevision]bool{}}
			observations[spaceID] = obs
		}
		obs.Revisions[unitRevision{UnitID: unitID, RevisionNum: o.RevisionNum}] = true

		healthStatus, _, _ := unstructured.NestedString(m, "healthStatus")
		message, _, _ := unstructured.NestedString(m, "message")
		obs.Resources = append(obs.Resources, resourceHealth{
			Kind:         obj.GetKind(),
			Namespace:    obj.GetNamespace(),
			Name:         obj.GetName(),
			HealthStatus: healthStatus,
			Message:      message,
		})
	}
	return observations
}

// decodeCollectedResource decodes a collected object (base64-encoded JSON, as a
// []byte field serializes) and its confighub.com/origin annotation.
func decodeCollectedResource(encoded string) (*unstructured.Unstructured, origin, bool) {
	if encoded == "" {
		return nil, origin{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, origin{}, false
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(raw); err != nil {
		return nil, origin{}, false
	}
	value := obj.GetAnnotations()[originAnnotation]
	if value == "" {
		return nil, origin{}, false
	}
	var o origin
	if err := json.Unmarshal([]byte(value), &o); err != nil {
		return nil, origin{}, false
	}
	return obj, o, true
}

// projectSveltosStatus combines a Space's collected objects and its profile's
// feature status into a Release LiveStatus. Health is the worst of the objects'
// health; the message is the first message among the objects in that state.
// ObservedAt is left zero so the result doubles as a stable dedup signature; the
// caller stamps it before writing.
func projectSveltosStatus(resources []resourceHealth, featureStatus, dataSource string) goclientnew.ReleaseLiveStatus {
	rank := func(s string) int {
		switch s {
		case "Healthy":
			return 0
		case "Suspended":
			return 1
		case "Progressing":
			return 2
		case "Degraded":
			return 3
		}
		return 4 // An unrecognized status reads as unknown, the worst.
	}
	health, msg := "", ""
	for i, res := range resources {
		if i == 0 || rank(res.HealthStatus) > rank(health) {
			health, msg = res.HealthStatus, ""
		}
		if res.HealthStatus == health && msg == "" && res.Message != "" {
			msg = res.Kind + "/" + res.Name + ": " + res.Message
		}
	}

	status := fromSveltos(featureStatus, health)
	status.Reporter = liveStatusReporter
	status.DataSource = dataSource
	status.Message = truncate(msg, liveStatusMaxMessage)
	return status
}
