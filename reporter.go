// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/confighub/argobot/kube"
	"github.com/confighub/sdk/core/livestatus"
	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/confighub/sdk/core/worker/lib"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	// liveStatusDebounce coalesces the burst of Application updates Argo emits
	// during a sync into a single report per Application.
	liveStatusDebounce = 2 * time.Second
	// liveStatusResync bounds how long a missed watch event can leave state
	// stale: the informer relists at this interval.
	liveStatusResync = 10 * time.Minute
	// liveStatusMaxMessage caps the Message field so the encoded Status stays
	// well within the 1024-byte Space-annotation limit.
	liveStatusMaxMessage = 200
	// liveStatusSource identifies argobot as the reporting client.
	liveStatusSource = "argobot"
	// spacesRefreshInterval rate-limits re-listing the worker's Targets when an
	// Application's Space is not yet known (e.g. a deployment added after start).
	spacesRefreshInterval = 30 * time.Second
	// ociSpacePathSep precedes the deployment Space slug in an Application's OCI
	// source repoURL: .../space/<space-slug>.
	ociSpacePathSep = "/space/"
)

// reporter watches Argo CD Application CRs and writes each one's live status
// back to its ConfigHub deployment Space as the confighub.com/live-status
// annotation.
//
// Kubernetes watches cannot filter on annotations, so the reporter watches every
// Application in the namespace and maps each to its Space in-process: the
// Application's OCI source (.../space/<slug>) — equivalently its name — names the
// deployment Space, and slug->id resolves against the Targets this worker is the
// bridge for. An Application whose Space is not one of those is ignored, which is
// also the filter for "an Application argobot is responsible for".
//
// It is best-effort feedback, not control: a dropped or delayed report costs
// only freshness. Writes are deduplicated against the last projection so an idle
// Application produces no Space churn, and coalesced per Application so a sync's
// burst of updates is one write.
type reporter struct {
	frontdoor *lib.WorkerFrontdoorClient
	cub       *goclientnew.ClientWithResponses
	workerID  string
	namespace string
	informer  cache.SharedIndexInformer
	queue     workqueue.TypedRateLimitingInterface[string]

	// lastSig deduplicates writes: appKey -> signature of the last projection
	// written (the encoded Status with ObservedAt omitted).
	lastSig map[string]string

	// spaces maps a deployment Space slug to its id, built from the Targets this
	// worker is the bridge for. spacesFetched rate-limits refreshes. Both are
	// touched only by the single worker goroutine (and once at startup), so they
	// need no lock.
	spaces        map[string]uuid.UUID
	spacesFetched time.Time
}

// newReporter builds the live-status reporter: a dynamic Kubernetes client
// (in-cluster or kubeconfig), a ConfigHub API client under the worker identity,
// and an unfiltered informer over Argo Applications in the configured namespace.
func newReporter(cfg config) (*reporter, error) {
	dyn, err := kube.NewDynamicClient()
	if err != nil {
		return nil, err
	}

	fc := lib.NewWorkerFrontdoorClient(cfg.ConfigHubURL, "", cfg.WorkerID, cfg.WorkerSecret)
	client := fc.GetClient()
	if client == nil {
		_ = fc.Close()
		return nil, fmt.Errorf("worker frontdoor authentication failed")
	}

	// No label selector: annotations are not watch-filterable, so the reporter
	// watches all Applications and filters in-process by Space ownership.
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dyn, liveStatusResync, cfg.ArgoNamespace, nil)
	informer := factory.ForResource(kube.ApplicationsGVR).Informer()

	r := &reporter{
		frontdoor: fc,
		cub:       client,
		workerID:  cfg.WorkerID,
		namespace: cfg.ArgoNamespace,
		informer:  informer,
		queue:     workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		lastSig:   make(map[string]string),
		spaces:    make(map[string]uuid.UUID),
	}

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { r.enqueue(obj) },
		UpdateFunc: func(_, obj any) { r.enqueue(obj) },
		// DeleteFunc: clearing live-status when an Application is deleted is a
		// follow-up; today a deleted Application leaves its last status in place.
	}); err != nil {
		_ = fc.Close()
		return nil, fmt.Errorf("register informer handler: %w", err)
	}

	return r, nil
}

// Run starts the informer and a single worker, blocking until ctx is cancelled.
func (r *reporter) Run(ctx context.Context) error {
	defer r.frontdoor.Close()

	// Prime the Space map. A failure here is not fatal: the worker refreshes on
	// demand when it meets an unknown Application.
	if err := r.refreshSpaces(ctx); err != nil {
		log.Printf("[WARN] live-status reporter: initial Target discovery failed: %v", err)
	}

	go r.informer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), r.informer.HasSynced) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Best-effort: a failed cache sync disables reporting for this run but
		// must not cancel the event consumers, so return nil rather than an error.
		log.Printf("[WARN] live-status reporter: informer cache failed to sync; reporting disabled")
		return nil
	}
	log.Printf("[INFO] live-status reporter: watching Argo Applications in namespace %q (%d deployment Space(s) known)",
		r.namespace, len(r.spaces))

	go r.runWorker(ctx)

	<-ctx.Done()
	r.queue.ShutDown()
	return ctx.Err()
}

func (r *reporter) runWorker(ctx context.Context) {
	for r.processNext(ctx) {
	}
}

func (r *reporter) processNext(ctx context.Context) bool {
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

// enqueue schedules a report for the object after the debounce window, coalescing
// the burst of updates a single sync produces into one write.
func (r *reporter) enqueue(obj any) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Printf("[WARN] live-status reporter: deriving key: %v", err)
		return
	}
	r.queue.AddAfter(key, liveStatusDebounce)
}

// report projects the current state of the Application named by key and, if it
// changed since the last write, patches its Space's confighub.com/live-status
// annotation. Applications whose Space this worker does not serve are ignored.
func (r *reporter) report(ctx context.Context, key string) error {
	obj, exists, err := r.informer.GetStore().GetByKey(key)
	if err != nil {
		return fmt.Errorf("look up %s: %w", key, err)
	}
	if !exists {
		// Deleted; see the DeleteFunc note in newReporter.
		delete(r.lastSig, key)
		return nil
	}

	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected object type %T for %s", obj, key)
	}

	slug := deploymentSpaceSlug(u)
	spaceID, known := r.lookupSpace(ctx, slug)
	if !known {
		// Not a Space this worker is the bridge for — not ours to report on.
		return nil
	}

	status := projectStatus(u)

	// Deduplicate on everything except ObservedAt (which changes every pass), so
	// an unchanged Application produces no write and no Space revision churn.
	sig, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal status signature: %w", err)
	}
	if r.lastSig[key] == string(sig) {
		return nil
	}

	status.ObservedAt = time.Now().UTC().Format(time.RFC3339)
	payload, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}

	if err := r.patchSpace(ctx, spaceID, string(payload)); err != nil {
		return err
	}
	r.lastSig[key] = string(sig)
	log.Printf("[INFO] live-status: %s -> space %s (sync=%s health=%s)",
		u.GetName(), spaceID, status.SyncStatus, status.HealthStatus)
	return nil
}

// lookupSpace resolves a deployment Space slug to its id from the worker's
// Targets, refreshing the map (rate-limited) if the slug is not yet known — a
// deployment can be added after the reporter starts.
func (r *reporter) lookupSpace(ctx context.Context, slug string) (uuid.UUID, bool) {
	if slug == "" {
		return uuid.Nil, false
	}
	if id, ok := r.spaces[slug]; ok {
		return id, true
	}
	if time.Since(r.spacesFetched) < spacesRefreshInterval {
		return uuid.Nil, false
	}
	if err := r.refreshSpaces(ctx); err != nil {
		log.Printf("[WARN] live-status reporter: Target discovery: %v", err)
		return uuid.Nil, false
	}
	id, ok := r.spaces[slug]
	return id, ok
}

// refreshSpaces rebuilds the deployment-Space slug->id map from the Targets this
// worker is the bridge for. A worker may read its own Targets even though it
// cannot list Spaces, and each Target carries its Space's id and slug.
func (r *reporter) refreshSpaces(ctx context.Context) error {
	where := fmt.Sprintf("BridgeWorkerID = '%s'", r.workerID)
	resp, err := r.cub.ListAllTargetsWithResponse(ctx, &goclientnew.ListAllTargetsParams{Where: &where})
	if err != nil {
		return fmt.Errorf("list worker targets: %w", err)
	}
	if resp.JSON200 == nil {
		return fmt.Errorf("list worker targets: unexpected HTTP %d: %s",
			resp.HTTPResponse.StatusCode, string(resp.Body))
	}

	m := make(map[string]uuid.UUID)
	for _, et := range *resp.JSON200 {
		if et.Target == nil || et.Target.SpaceSlug == "" {
			continue
		}
		m[et.Target.SpaceSlug] = et.Target.SpaceID
	}
	r.spaces = m
	r.spacesFetched = time.Now()
	return nil
}

// patchSpace merge-patches the confighub.com/live-status annotation onto the
// Space. The worker identity is authorized because it is the Space's release
// bridge worker.
func (r *reporter) patchSpace(ctx context.Context, spaceID uuid.UUID, value string) error {
	v := value
	annotations := map[string]*string{livestatus.Annotation: &v}
	body := goclientnew.PatchSpaceApplicationMergePatchPlusJSONRequestBody{
		Annotations: &annotations,
	}
	resp, err := r.cub.PatchSpaceWithApplicationMergePatchPlusJSONBodyWithResponse(
		ctx, spaceID, &goclientnew.PatchSpaceParams{}, body)
	if err != nil {
		return fmt.Errorf("patch space %s: %w", spaceID, err)
	}
	if resp.JSON200 == nil {
		return fmt.Errorf("patch space %s: unexpected HTTP %d: %s",
			spaceID, resp.HTTPResponse.StatusCode, string(resp.Body))
	}
	return nil
}

// deploymentSpaceSlug returns the deployment Space slug an Application belongs to:
// the last path segment of its OCI source repoURL (.../space/<slug>), falling
// back to the Application name, which equals the Space slug by convention.
func deploymentSpaceSlug(u *unstructured.Unstructured) string {
	repoURL, _, _ := unstructured.NestedString(u.Object, "spec", "source", "repoURL")
	if slug := spaceSlugFromRepoURL(repoURL); slug != "" {
		return slug
	}
	return u.GetName()
}

// spaceSlugFromRepoURL extracts <slug> from an OCI repoURL of the form
// .../space/<slug>. It returns "" when the URL has no such segment or the slug
// is empty or itself contains a path separator.
func spaceSlugFromRepoURL(repoURL string) string {
	i := strings.LastIndex(repoURL, ociSpacePathSep)
	if i < 0 {
		return ""
	}
	slug := strings.Trim(repoURL[i+len(ociSpacePathSep):], "/")
	if slug == "" || strings.Contains(slug, "/") {
		return ""
	}
	return slug
}

// projectStatus reads the Argo Application's status subresource into a
// livestatus.Status. ObservedAt is left empty here so the result doubles as a
// stable dedup signature; the caller stamps it before writing.
func projectStatus(u *unstructured.Unstructured) livestatus.Status {
	obj := u.Object
	syncStatus, _, _ := unstructured.NestedString(obj, "status", "sync", "status")
	revision, _, _ := unstructured.NestedString(obj, "status", "sync", "revision")
	healthStatus, _, _ := unstructured.NestedString(obj, "status", "health", "status")
	healthMsg, _, _ := unstructured.NestedString(obj, "status", "health", "message")
	opPhase, _, _ := unstructured.NestedString(obj, "status", "operationState", "phase")
	opMsg, _, _ := unstructured.NestedString(obj, "status", "operationState", "message")

	// Prefer the health message; fall back to the operation message.
	msg := healthMsg
	if msg == "" {
		msg = opMsg
	}

	return livestatus.Status{
		Source:         liveStatusSource,
		App:            u.GetName(),
		SyncStatus:     syncStatus,
		HealthStatus:   healthStatus,
		OperationPhase: opPhase,
		Revision:       revision,
		Message:        truncate(msg, liveStatusMaxMessage),
	}
}

// truncate shortens s to at most n bytes, appending an ellipsis marker when it
// cuts. Kept byte-oriented since the concern is the annotation size limit.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
