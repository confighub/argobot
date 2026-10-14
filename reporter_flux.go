// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/confighub/argobot/flux"
	"github.com/confighub/argobot/kube"
	"github.com/confighub/sdk/core/cubapi"
	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// fluxSourceIndex indexes Kustomizations by the key (<namespace>/<name>) of the
// OCIRepository they apply.
const fluxSourceIndex = "ocirepository"

// fluxObservation is what one Flux object says about one Release.
type fluxObservation struct {
	// DataSource names the object, as <Kind>/<namespace>/<name>.
	DataSource string
	// Status is the normalized status, without Reporter, DataSource, Message or
	// ObservedAt.
	Status goclientnew.ReleaseLiveStatus
	// Message is the object's Ready message, when it is worth showing.
	Message string
}

// fluxReporter writes the live status of Releases that Flux deploys.
//
// Flux records the digests it fetched and applied, as Argo CD does, and each
// digest is the ManifestDigest of a Release. The OCIRepository's URL
// (.../space/<slug>), or its name, names the deployment Space.
//
// A LiveStatus describes its own Release, so each fact is written to the Release
// it is about:
//
//   - The Release the cluster runs gets the Kustomization's state: in sync once
//     applied, with its health and operation.
//   - A newer Release the Kustomization attempted but did not apply (it failed to
//     build or apply, or is still on its way) gets that attempt: out of sync, with
//     the operation running or failed and Flux's message. The Release it runs
//     meanwhile stays in sync.
//   - When the OCIRepository fails to pull, the Release it should have pulled
//     (the Space's newest published Release, which latest resolves to, or the
//     pinned digest) gets the failure, unless the cluster already runs or
//     attempts it.
//   - A Release argobot reported on for this repository that is neither run nor
//     attempted any more is marked replaced, so a stale status does not linger.
//
// Work is keyed by OCIRepository, not Kustomization: several Kustomizations can
// apply one repository (different paths, or one per tenant), so states that fall
// on one Release are combined into the least advanced one and written once.
// Writes are deduplicated against the status the Release already has in
// ConfigHub, so a restart does not rewrite every Release. A Release counts as
// this repository's when its DataSource names one of its Kustomizations or the
// repository itself; two clusters with same-named Kustomizations deploying one
// Space would mark each other's Releases replaced.
type fluxReporter struct {
	releaseLookup
	sources        cache.SharedIndexInformer
	kustomizations cache.SharedIndexInformer
	queue          workqueue.TypedRateLimitingInterface[string]
}

// newFluxReporter builds the Flux live-status reporter: a dynamic Kubernetes
// client (in-cluster or kubeconfig) and informers over OCIRepositories and
// Kustomizations in every namespace, writing through hub.
func newFluxReporter(_ config, hub *cubapi.Client) (*fluxReporter, error) {
	dyn, err := kube.NewDynamicClient()
	if err != nil {
		return nil, err
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dyn, liveStatusResync, metav1.NamespaceAll, nil)
	r := &fluxReporter{
		releaseLookup:  newReleaseLookup(liveStatusWriter{api: hub.API}),
		sources:        factory.ForResource(flux.OCIRepositoriesGVR).Informer(),
		kustomizations: factory.ForResource(flux.KustomizationsGVR).Informer(),
		queue:          workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}

	if err := r.kustomizations.AddIndexers(cache.Indexers{fluxSourceIndex: func(obj any) ([]string, error) {
		if k, ok := obj.(*unstructured.Unstructured); ok {
			if key := kustomizationSourceKey(k); key != "" {
				return []string{key}, nil
			}
		}
		return nil, nil
	}}); err != nil {
		return nil, fmt.Errorf("index Kustomizations by OCIRepository: %w", err)
	}

	if _, err := r.sources.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { r.enqueue(obj) },
		UpdateFunc: func(_, obj any) { r.enqueue(obj) },
		// DeleteFunc: a deleted repository leaves its Releases' last status in
		// place, which still describes them as they were last seen running.
	}); err != nil {
		return nil, fmt.Errorf("register OCIRepository informer handler: %w", err)
	}
	// A Kustomization carries the state of the repository it applies.
	enqueueSource := func(obj any) {
		if k, ok := obj.(*unstructured.Unstructured); ok {
			if key := kustomizationSourceKey(k); key != "" {
				r.queue.AddAfter(key, liveStatusDebounce)
			}
		}
	}
	if _, err := r.kustomizations.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    enqueueSource,
		UpdateFunc: func(_, obj any) { enqueueSource(obj) },
	}); err != nil {
		return nil, fmt.Errorf("register Kustomization informer handler: %w", err)
	}

	return r, nil
}

// Run starts the informers and a single worker, blocking until ctx is cancelled.
func (r *fluxReporter) Run(ctx context.Context) error {
	go r.sources.Run(ctx.Done())
	go r.kustomizations.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), r.sources.HasSynced, r.kustomizations.HasSynced) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Best-effort: a failed cache sync disables reporting for this run but
		// must not cancel the event consumers, so return nil rather than an error.
		log.Printf("[WARN] live-status reporter: Flux informer caches failed to sync; reporting disabled")
		return nil
	}
	log.Printf("[INFO] live-status reporter: watching Flux OCIRepositories and Kustomizations")

	go func() {
		for r.processNext(ctx) {
		}
	}()

	<-ctx.Done()
	r.queue.ShutDown()
	return ctx.Err()
}

func (r *fluxReporter) processNext(ctx context.Context) bool {
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
func (r *fluxReporter) enqueue(obj any) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Printf("[WARN] live-status reporter: deriving key: %v", err)
		return
	}
	r.queue.AddAfter(key, liveStatusDebounce)
}

// report projects the OCIRepository named by key and the Kustomizations that
// apply it onto the Releases of its Space, and writes each status that differs
// from the one the Release has. Spaces argobot cannot see are ignored.
func (r *fluxReporter) report(ctx context.Context, key string) error {
	obj, exists, err := r.sources.GetStore().GetByKey(key)
	if err != nil {
		return fmt.Errorf("look up %s: %w", key, err)
	}
	if !exists {
		// Deleted; see the DeleteFunc note in newFluxReporter.
		return nil
	}
	source, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected object type %T for %s", obj, key)
	}

	slug := spaceSlugFromRepoURL(flux.URL(source))
	if slug == "" {
		slug = source.GetName()
	}
	spaceID, known := r.getSpaceID(ctx, slug)
	if !known {
		// Slug did not resolve to a Space argobot can see -- not ours.
		return nil
	}
	releases, known, err := r.listReleases(ctx, spaceID)
	if err != nil {
		return err
	}
	if !known {
		return nil
	}

	items, err := r.kustomizations.GetIndexer().ByIndex(fluxSourceIndex, key)
	if err != nil {
		return fmt.Errorf("list the Kustomizations of %s: %w", key, err)
	}
	kustomizations := make([]*unstructured.Unstructured, 0, len(items))
	for _, item := range items {
		if k, ok := item.(*unstructured.Unstructured); ok {
			kustomizations = append(kustomizations, k)
		}
	}

	var errs []error
	for _, w := range planFluxWrites(source, kustomizations, releases) {
		if err := r.writeIfChanged(ctx, spaceID, w); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("report %s: %v", key, errs)
	}
	return nil
}

// fluxWrite is one status to give one Release.
type fluxWrite struct {
	Release goclientnew.Release
	Status  goclientnew.ReleaseLiveStatus
}

// planFluxWrites decides the status of every Release of the Space the
// repository's state says something about (see fluxReporter), in ReleaseNum
// order. ObservedAt is left zero.
func planFluxWrites(source *unstructured.Unstructured, kustomizations []*unstructured.Unstructured,
	releases []goclientnew.Release) []fluxWrite {
	// Several Releases can share a digest when the same content is republished;
	// the newest is the one, as for Argo CD.
	byDigest := map[string]goclientnew.Release{}
	for _, release := range releases {
		if release.ManifestDigest == "" {
			continue
		}
		if prev, ok := byDigest[release.ManifestDigest]; !ok || release.ReleaseNum > prev.ReleaseNum {
			byDigest[release.ManifestDigest] = release
		}
	}

	observations, sourceFailure := observeFluxSource(source, kustomizations)
	if sourceFailure != nil {
		if target, ok := fluxSourceTarget(source, releases); ok {
			if _, seen := observations[target.ManifestDigest]; !seen {
				observations[target.ManifestDigest] = []fluxObservation{*sourceFailure}
			}
		}
	}

	var writes []fluxWrite
	written := map[uuid.UUID]bool{}
	var running *fluxWrite
	for digest, obs := range observations {
		release, ok := byDigest[digest]
		if !ok {
			// A digest that is no Release of the Space, such as one withdrawn and
			// deleted: nothing to record it on.
			continue
		}
		w := fluxWrite{Release: release, Status: projectFluxStatus(obs)}
		writes = append(writes, w)
		written[release.ReleaseID] = true
		if w.Status.Sync == goclientnew.Synced && (running == nil || release.ReleaseNum > running.Release.ReleaseNum) {
			running = &w
		}
	}

	// Mark replaced what this repository reported on before and no longer runs
	// or attempts, and the published Releases older than the running one that no
	// reporter saw, such as one replaced before it was applied. Only once
	// something runs: until then nothing has replaced them.
	if running != nil {
		owners := map[string]bool{"OCIRepository/" + source.GetNamespace() + "/" + source.GetName(): true}
		for _, k := range kustomizations {
			owners["Kustomization/"+k.GetNamespace()+"/"+k.GetName()] = true
		}
		for _, release := range releases {
			if written[release.ReleaseID] {
				continue
			}
			ls := release.LiveStatus
			if ls == nil {
				if release.Published && release.ReleaseNum < running.Release.ReleaseNum {
					status := fluxReplacedStatus(goclientnew.ReleaseLiveStatus{DataSource: running.Status.DataSource}, running.Release.ReleaseNum)
					writes = append(writes, fluxWrite{Release: release, Status: status})
				}
				continue
			}
			dataSource := fluxDataSource(ls.DataSource)
			if ls.Reporter != liveStatusReporter ||
				!slices.ContainsFunc(strings.Split(dataSource, ","), func(s string) bool { return owners[s] }) {
				continue
			}
			if fluxMarkedReplaced(ls) {
				// Already marked; leave it rather than rewrite every old Release
				// on each deploy just to name the newest replacement, unless it
				// names its source in the older form.
				if dataSource != ls.DataSource {
					status := *ls
					status.ObservedAt = time.Time{}
					status.DataSource = dataSource
					writes = append(writes, fluxWrite{Release: release, Status: status})
				}
				continue
			}
			was := *ls
			was.DataSource = dataSource
			writes = append(writes, fluxWrite{Release: release, Status: fluxReplacedStatus(was, running.Release.ReleaseNum)})
		}
	}

	slices.SortFunc(writes, func(a, b fluxWrite) int { return cmp.Compare(a.Release.ReleaseNum, b.Release.ReleaseNum) })
	return writes
}

// fluxReplacedMessage begins the message of a Release marked replaced.
const fluxReplacedMessage = "Replaced by release"

// fluxReplacedStatus is the status of a Release that had status was and that
// Release replacement now replaces. A failure is kept, with Flux's reason and
// message, since the Release's status is the only record of it.
func fluxReplacedStatus(was goclientnew.ReleaseLiveStatus, replacement int64) goclientnew.ReleaseLiveStatus {
	status := fromFluxReplaced()
	status.Reporter = liveStatusReporter
	status.DataSource = was.DataSource
	status.Message = fmt.Sprintf("%s %d", fluxReplacedMessage, replacement)
	if was.Operation == goclientnew.ReleaseLiveStatusOperationFailed {
		status.Operation = was.Operation
		status.ReporterOperation = was.ReporterOperation
		if was.Message != "" {
			status.Message = truncate(status.Message+", after it failed: "+was.Message, liveStatusMaxMessage)
		}
	}
	return status
}

// fluxMarkedReplaced says a status is one planFluxWrites marked replaced.
func fluxMarkedReplaced(ls *goclientnew.ReleaseLiveStatus) bool {
	return ls.Sync == goclientnew.OutOfSync && strings.HasPrefix(ls.Message, fluxReplacedMessage+" ")
}

// fluxDataSource rewrites a DataSource in the form this version writes. Earlier
// versions named a Kustomization as <namespace>/<name>, without its kind.
func fluxDataSource(dataSource string) string {
	if dataSource == "" {
		return ""
	}
	sources := strings.Split(dataSource, ",")
	for i, s := range sources {
		if strings.Count(s, "/") == 1 {
			sources[i] = "Kustomization/" + s
		}
	}
	return truncate(strings.Join(sources, ","), 253)
}

// fluxSourceTarget is the Release a failing OCIRepository should have pulled:
// its pinned digest's, or, for a tag (the gateway serves only latest), the
// Space's newest published Release.
func fluxSourceTarget(source *unstructured.Unstructured, releases []goclientnew.Release) (goclientnew.Release, bool) {
	pinned, _, _ := unstructured.NestedString(source.Object, "spec", "ref", "digest")
	var target goclientnew.Release
	found := false
	for _, release := range releases {
		switch {
		case pinned != "":
			if release.ManifestDigest != pinned {
				continue
			}
		case !release.Published:
			continue
		}
		if !found || release.ReleaseNum > target.ReleaseNum {
			target, found = release, true
		}
	}
	return target, found
}

// writeIfChanged writes w's status to its Release unless the Release already has
// it, ObservedAt aside.
func (r *fluxReporter) writeIfChanged(ctx context.Context, spaceID uuid.UUID, w fluxWrite) error {
	if current := w.Release.LiveStatus; current != nil {
		was := *current
		was.ObservedAt = time.Time{}
		before, err := json.Marshal(was)
		if err != nil {
			return fmt.Errorf("marshal status signature: %w", err)
		}
		after, err := json.Marshal(w.Status)
		if err != nil {
			return fmt.Errorf("marshal status signature: %w", err)
		}
		if string(before) == string(after) {
			return nil
		}
	}
	status := w.Status
	status.ObservedAt = time.Now().UTC()
	if err := r.patchRelease(ctx, spaceID, w.Release.ReleaseID, status); err != nil {
		return err
	}
	log.Printf("[INFO] live-status: %s -> release %d (%s) of space %s (sync=%s health=%s op=%s)",
		status.DataSource, w.Release.ReleaseNum, w.Release.ReleaseID, spaceID, status.Sync, status.Health, status.Operation)
	return nil
}

// listReleases lists the Releases of a Space with their digests and live
// status. A Space argobot cannot see is not known.
func (r *fluxReporter) listReleases(ctx context.Context, spaceID uuid.UUID) ([]goclientnew.Release, bool, error) {
	selectFields := "ReleaseID,ReleaseNum,Published,ManifestDigest,LiveStatus,SpaceID,OrganizationID"
	resp, err := r.api.ListExtendedReleasesWithResponse(ctx, spaceID,
		&goclientnew.ListExtendedReleasesParams{Select: &selectFields})
	if err != nil {
		return nil, false, fmt.Errorf("list the releases of space %s: %w", spaceID, err)
	}
	if resp.JSON200 == nil {
		if code := resp.HTTPResponse.StatusCode; code == http.StatusNotFound || code == http.StatusForbidden {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("list the releases of space %s: unexpected HTTP %d: %s",
			spaceID, resp.HTTPResponse.StatusCode, string(resp.Body))
	}
	releases := make([]goclientnew.Release, 0, len(*resp.JSON200))
	for _, er := range *resp.JSON200 {
		if er.Release != nil {
			releases = append(releases, *er.Release)
		}
	}
	return releases, true, nil
}

// observeFluxSource reads what an OCIRepository and the Kustomizations that
// apply it say, grouped by the manifest digest of the Release each statement is
// about, each group in DataSource order; and the repository's failure to pull,
// if it is failing.
//
// The Release a Kustomization runs is normally its lastAppliedRevision. But Flux
// records a revision as applied only once its health check passes, so a revision
// whose objects were applied and are being checked, or failed the check, is still
// only lastAttemptedRevision, though it is what the cluster runs (see
// fluxApplied). A lastAttemptedRevision that is not on the cluster is a newer
// Release being attempted, and is reported on separately.
func observeFluxSource(source *unstructured.Unstructured, kustomizations []*unstructured.Unstructured) (map[string][]fluxObservation, *fluxObservation) {
	byDigest := map[string][]fluxObservation{}
	for _, k := range kustomizations {
		wait, _, _ := unstructured.NestedBool(k.Object, "spec", "wait")
		healthChecks, _, _ := unstructured.NestedSlice(k.Object, "spec", "healthChecks")
		c := fluxConditions{HealthChecked: wait || len(healthChecks) > 0}
		ready := readCondition(k, fluxConditionReady)
		healthy := readCondition(k, fluxConditionHealthy)
		c.Ready, c.ReadyReason = ready.Status, ready.Reason
		c.Healthy, c.HealthyReason = healthy.Status, healthy.Reason
		c.Reconciling = readCondition(k, fluxConditionReconciling).Status
		c.Stalled = readCondition(k, fluxConditionStalled).Status

		applied, _, _ := unstructured.NestedString(k.Object, "status", "lastAppliedRevision")
		attempted, _, _ := unstructured.NestedString(k.Object, "status", "lastAttemptedRevision")
		if attempted == "" {
			attempted = applied
		}
		running := applied
		if fluxApplied(c) {
			running = attempted
		}
		runningDigest, attemptedDigest := fluxManifestDigest(running), fluxManifestDigest(attempted)

		dataSource := "Kustomization/" + k.GetNamespace() + "/" + k.GetName()
		message := ""
		if c.Ready != "True" {
			message = ready.Message
		}
		if attemptedDigest == runningDigest {
			if runningDigest != "" {
				byDigest[runningDigest] = append(byDigest[runningDigest], fluxObservation{
					DataSource: dataSource, Status: fromFluxRunning(c), Message: message})
			}
			continue
		}
		if runningDigest != "" {
			byDigest[runningDigest] = append(byDigest[runningDigest], fluxObservation{
				DataSource: dataSource, Status: fromFluxSuperseded(c)})
		}
		if attemptedDigest != "" {
			byDigest[attemptedDigest] = append(byDigest[attemptedDigest], fluxObservation{
				DataSource: dataSource, Status: fromFluxAttempted(c), Message: message})
		}
	}
	for _, group := range byDigest {
		slices.SortFunc(group, func(a, b fluxObservation) int { return strings.Compare(a.DataSource, b.DataSource) })
	}

	var sourceFailure *fluxObservation
	if ready := readCondition(source, fluxConditionReady); ready.Status == "False" {
		sourceFailure = &fluxObservation{
			DataSource: "OCIRepository/" + source.GetNamespace() + "/" + source.GetName(),
			Status:     fromFluxSourceFailed(ready.Reason),
			Message:    ready.Message,
		}
	}
	return byDigest, sourceFailure
}

// fluxCondition is one status condition of a Flux object.
type fluxCondition struct {
	Status, Reason, Message string
}

// readCondition returns the condition of type typ, or a zero one when absent.
func readCondition(u *unstructured.Unstructured, typ string) fluxCondition {
	entries, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(m, "type"); t != typ {
			continue
		}
		var c fluxCondition
		c.Status, _, _ = unstructured.NestedString(m, "status")
		c.Reason, _, _ = unstructured.NestedString(m, "reason")
		c.Message, _, _ = unstructured.NestedString(m, "message")
		return c
	}
	return fluxCondition{}
}

// projectFluxStatus combines what several Flux objects say about one Release
// into its LiveStatus: each of sync, health and operation is the least advanced
// among them, with the Flux reason of the object it came from. The message is the
// first one given, prefixed with its object. ObservedAt is left zero; the caller
// stamps it before writing.
func projectFluxStatus(observations []fluxObservation) goclientnew.ReleaseLiveStatus {
	syncRank := func(s goclientnew.ReleaseLiveStatusSync) int {
		switch s {
		case goclientnew.Synced:
			return 0
		case goclientnew.OutOfSync:
			return 2
		}
		return 1
	}
	healthRank := func(h goclientnew.ReleaseLiveStatusHealth) int {
		switch h {
		case goclientnew.ReleaseLiveStatusHealthHealthy:
			return 0
		case goclientnew.ReleaseLiveStatusHealthSuspended:
			return 1
		case goclientnew.ReleaseLiveStatusHealthProgressing:
			return 2
		case goclientnew.ReleaseLiveStatusHealthDegraded, goclientnew.ReleaseLiveStatusHealthMissing:
			return 3
		}
		return 4 // Unknown reads as the worst.
	}
	operationRank := func(o goclientnew.ReleaseLiveStatusOperation) int {
		switch o {
		case goclientnew.ReleaseLiveStatusOperationSucceeded:
			return 0
		case goclientnew.ReleaseLiveStatusOperationRunning:
			return 2
		case goclientnew.ReleaseLiveStatusOperationFailed:
			return 3
		}
		return 1
	}

	var status goclientnew.ReleaseLiveStatus
	sources := make([]string, 0, len(observations))
	msg := ""
	for i, o := range observations {
		s := o.Status
		if i == 0 || syncRank(s.Sync) > syncRank(status.Sync) {
			status.Sync, status.ReporterSync = s.Sync, s.ReporterSync
		}
		if i == 0 || healthRank(s.Health) > healthRank(status.Health) {
			status.Health, status.ReporterHealth = s.Health, s.ReporterHealth
		}
		if i == 0 || operationRank(s.Operation) > operationRank(status.Operation) {
			status.Operation, status.ReporterOperation = s.Operation, s.ReporterOperation
		}
		if msg == "" && o.Message != "" {
			msg = o.DataSource + ": " + o.Message
		}
		if !slices.Contains(sources, o.DataSource) {
			sources = append(sources, o.DataSource)
		}
	}

	status.Reporter = liveStatusReporter
	status.DataSource = truncate(strings.Join(sources, ","), 253)
	status.Message = truncate(msg, liveStatusMaxMessage)
	return status
}

// fluxManifestDigest returns the manifest digest in a Flux OCI revision,
// <tag>@<digest> (or a bare <digest> when the repository is pinned to one), or
// "" when it holds none.
func fluxManifestDigest(revision string) string {
	if _, digest, ok := strings.Cut(revision, "@"); ok {
		revision = digest
	}
	if !strings.HasPrefix(revision, "sha256:") {
		return ""
	}
	return revision
}

// kustomizationSourceKey is the key (<namespace>/<name>) of the OCIRepository a
// Kustomization applies, or "" when its source is of another kind. A sourceRef
// with no namespace is in the Kustomization's own.
func kustomizationSourceKey(k *unstructured.Unstructured) string {
	kind, _, _ := unstructured.NestedString(k.Object, "spec", "sourceRef", "kind")
	if kind != "OCIRepository" {
		return ""
	}
	name, _, _ := unstructured.NestedString(k.Object, "spec", "sourceRef", "name")
	if name == "" {
		return ""
	}
	namespace, _, _ := unstructured.NestedString(k.Object, "spec", "sourceRef", "namespace")
	if namespace == "" {
		namespace = k.GetNamespace()
	}
	return namespace + "/" + name
}
