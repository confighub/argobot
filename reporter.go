// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
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
	// liveStatusMaxMessage caps the Message field at the length ConfigHub accepts.
	liveStatusMaxMessage = 1024
	// liveStatusReporter identifies argobot as the reporting client.
	liveStatusReporter = "argobot"
	// ociSpacePathSep precedes the deployment Space slug in an Application's OCI
	// source repoURL: .../space/<space-slug>.
	ociSpacePathSep = "/space/"
)

// reporter watches Argo CD Application CRs and writes each one's live status to
// the ConfigHub Release the Application is running, as the Release's LiveStatus.
//
// Kubernetes watches cannot filter on what an Application deploys, so the
// reporter watches every Application in the namespace and maps each to its
// Release in-process. The Application's OCI source (.../space/<slug>), or
// equivalently its name, names the deployment Space, and its synced revision is
// the ManifestDigest of the Release it is running; the newest Release of the Space
// with that ManifestDigest is the one. The worker can read the Spaces and Releases
// of the Target it is granted on, and EditChildren on that Target lets it write
// the Releases' LiveStatus. An Application whose Space or Release does not resolve
// is ignored, which is also the filter for "an Application argobot is
// responsible for".
//
// It is best-effort feedback, not control: a dropped or delayed report costs
// only freshness. Writes are deduplicated against the last projection so an idle
// Application produces no churn, and coalesced per Application so a sync's
// burst of updates is one write.
type reporter struct {
	frontdoor *lib.WorkerFrontdoorClient
	namespace string
	informer  cache.SharedIndexInformer
	queue     workqueue.TypedRateLimitingInterface[string]

	// lastSig deduplicates writes: appKey -> signature of the last projection
	// written (the Release and the encoded status with ObservedAt omitted).
	lastSig map[string]string

	// spaceIDs caches resolved deployment-Space slug -> id lookups, and
	// releaseIDs resolved Space and manifest digest -> Release id lookups, which a
	// Release's immutable content makes safe to keep. Touched only by the single
	// worker goroutine, so they need no lock.
	spaceIDs   map[string]uuid.UUID
	releaseIDs map[string]uuid.UUID
}

// newReporter builds the live-status reporter: a dynamic Kubernetes client
// (in-cluster or kubeconfig), a ConfigHub API client under the worker identity,
// and an unfiltered informer over Argo Applications in the configured namespace.
func newReporter(cfg config) (*reporter, error) {
	dyn, err := kube.NewDynamicClient()
	if err != nil {
		return nil, err
	}

	// Authenticate once here to fail fast on bad credentials. The client itself is
	// deliberately not cached — see reporter.client.
	fc := lib.NewWorkerFrontdoorClient(cfg.ConfigHubURL, "", cfg.WorkerID, cfg.WorkerSecret)
	if fc.GetClient() == nil {
		_ = fc.Close()
		return nil, fmt.Errorf("worker frontdoor authentication failed")
	}

	// No label selector: what an Application deploys is not watch-filterable, so
	// the reporter watches all Applications and filters in-process.
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dyn, liveStatusResync, cfg.ArgoNamespace, nil)
	informer := factory.ForResource(kube.ApplicationsGVR).Informer()

	r := &reporter{
		frontdoor:  fc,
		namespace:  cfg.ArgoNamespace,
		informer:   informer,
		queue:      workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		lastSig:    make(map[string]string),
		spaceIDs:   make(map[string]uuid.UUID),
		releaseIDs: make(map[string]uuid.UUID),
	}

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { r.enqueue(obj) },
		UpdateFunc: func(_, obj any) { r.enqueue(obj) },
		// DeleteFunc: a deleted Application leaves its Release's last status in
		// place, which still describes the Release as it was last seen running.
	}); err != nil {
		_ = fc.Close()
		return nil, fmt.Errorf("register informer handler: %w", err)
	}

	return r, nil
}

// Run starts the informer and a single worker, blocking until ctx is cancelled.
func (r *reporter) Run(ctx context.Context) error {
	defer r.frontdoor.Close()

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
	log.Printf("[INFO] live-status reporter: watching Argo Applications in namespace %q", r.namespace)

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
// changed since the last write, writes it to the LiveStatus of the Release the
// Application is running. Applications whose Space or Release this worker cannot
// see are ignored.
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
	spaceID, known := r.getSpaceID(ctx, slug)
	if !known {
		// Slug did not resolve to a Space this worker can see -- not ours.
		return nil
	}
	revision, _, _ := unstructured.NestedString(u.Object, "status", "sync", "revision")
	if revision == "" {
		// Argo has not resolved a revision yet, so there is no Release to report on.
		return nil
	}
	releaseID, known, err := r.getReleaseID(ctx, spaceID, revision)
	if err != nil {
		return err
	}
	if !known {
		// A revision that is no Release of the Space, such as one withdrawn and
		// deleted: nothing to record it on.
		return nil
	}

	status := projectStatus(u)

	// Deduplicate on the Release and everything except ObservedAt (which changes
	// every pass), so an unchanged Application produces no write.
	encoded, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal status signature: %w", err)
	}
	sig := releaseID.String() + " " + string(encoded)
	if r.lastSig[key] == sig {
		return nil
	}

	status.ObservedAt = time.Now().UTC()
	if err := r.patchRelease(ctx, spaceID, releaseID, status); err != nil {
		return err
	}
	r.lastSig[key] = sig
	log.Printf("[INFO] live-status: %s -> release %s of space %s (sync=%s health=%s)",
		u.GetName(), releaseID, spaceID, status.Sync, status.Health)
	return nil
}

// client returns the frontdoor's current ConfigHub client. It must be called per
// request rather than cached: the frontdoor refreshes the worker JWT on a timer,
// and a refresh builds a *new* client bound to the new token instead of mutating
// the old one in place. A cached client therefore keeps sending the token it was
// created with, and starts failing with 401 "token is expired" for the process's
// remaining lifetime once that token's 24h TTL runs out. This is a cheap
// RLock-guarded read of an existing client — it triggers no authentication.
func (r *reporter) client() (*goclientnew.ClientWithResponses, error) {
	c := r.frontdoor.GetClient()
	if c == nil {
		return nil, fmt.Errorf("no authenticated ConfigHub client")
	}
	return c, nil
}

// getSpaceID resolves a deployment Space slug to its id, caching hits. The worker
// can read the Spaces that release to the Target it is granted on, which are the
// deployment Spaces it reports on, so a slug that resolves is one this worker is
// responsible for. A slug that does not resolve (not found, or a Space the worker
// cannot see) is treated as not-ours and skipped.
func (r *reporter) getSpaceID(ctx context.Context, slug string) (uuid.UUID, bool) {
	if slug == "" {
		return uuid.Nil, false
	}
	if id, ok := r.spaceIDs[slug]; ok {
		return id, true
	}

	cub, err := r.client()
	if err != nil {
		log.Printf("[WARN] live-status reporter: resolve space %q: %v", slug, err)
		return uuid.Nil, false
	}

	where := fmt.Sprintf("Slug = '%s'", slug)
	resp, err := cub.ListSpacesWithResponse(ctx, &goclientnew.ListSpacesParams{Where: &where})
	if err != nil {
		log.Printf("[WARN] live-status reporter: resolve space %q: %v", slug, err)
		return uuid.Nil, false
	}
	if resp.JSON200 == nil {
		log.Printf("[WARN] live-status reporter: resolve space %q: unexpected HTTP %d: %s",
			slug, resp.HTTPResponse.StatusCode, string(resp.Body))
		return uuid.Nil, false
	}

	for _, es := range *resp.JSON200 {
		if es.Space != nil && es.Space.Slug == slug {
			r.spaceIDs[slug] = es.Space.SpaceID
			return es.Space.SpaceID, true
		}
	}
	return uuid.Nil, false
}

// getReleaseID resolves the Release of the Space whose ManifestDigest is the
// revision Argo synced: the newest, should a republish of the same content have
// made more than one. Hits are cached; a miss is not, since the Release may not
// be listed yet.
func (r *reporter) getReleaseID(ctx context.Context, spaceID uuid.UUID, manifestDigest string) (uuid.UUID, bool, error) {
	cacheKey := spaceID.String() + "/" + manifestDigest
	if id, ok := r.releaseIDs[cacheKey]; ok {
		return id, true, nil
	}
	cub, err := r.client()
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("find the release of space %s with digest %s: %w", spaceID, manifestDigest, err)
	}
	where := fmt.Sprintf("ManifestDigest = '%s'", manifestDigest)
	selectFields := "ReleaseID,ReleaseNum,SpaceID,OrganizationID"
	resp, err := cub.ListExtendedReleasesWithResponse(ctx, spaceID,
		&goclientnew.ListExtendedReleasesParams{Where: &where, Select: &selectFields})
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("find the release of space %s with digest %s: %w", spaceID, manifestDigest, err)
	}
	if resp.JSON200 == nil {
		return uuid.Nil, false, fmt.Errorf("find the release of space %s with digest %s: unexpected HTTP %d: %s",
			spaceID, manifestDigest, resp.HTTPResponse.StatusCode, string(resp.Body))
	}
	var newest *goclientnew.Release
	for _, er := range *resp.JSON200 {
		if er.Release != nil && (newest == nil || er.Release.ReleaseNum > newest.ReleaseNum) {
			newest = er.Release
		}
	}
	if newest == nil {
		return uuid.Nil, false, nil
	}
	r.releaseIDs[cacheKey] = newest.ReleaseID
	return newest.ReleaseID, true, nil
}

// patchRelease writes status as the Release's LiveStatus. The worker is
// authorized by EditChildren on the Release's Target.
func (r *reporter) patchRelease(ctx context.Context, spaceID, releaseID uuid.UUID, status goclientnew.ReleaseLiveStatus) error {
	liveStatus, err := mergePatchObject(status)
	if err != nil {
		return fmt.Errorf("encode the live status of release %s: %w", releaseID, err)
	}
	// Only LiveStatus is sent: a merge patch reads every other field it carries,
	// and the generated body would carry them all.
	patch, err := json.Marshal(map[string]any{"LiveStatus": liveStatus})
	if err != nil {
		return fmt.Errorf("encode the live status of release %s: %w", releaseID, err)
	}
	cub, err := r.client()
	if err != nil {
		return fmt.Errorf("patch release %s: %w", releaseID, err)
	}
	resp, err := cub.PatchReleaseWithBodyWithResponse(
		ctx, spaceID, releaseID, &goclientnew.PatchReleaseParams{},
		"application/merge-patch+json", bytes.NewReader(patch))
	if err != nil {
		return fmt.Errorf("patch release %s: %w", releaseID, err)
	}
	if resp.JSON200 == nil {
		return fmt.Errorf("patch release %s: unexpected HTTP %d: %s",
			releaseID, resp.HTTPResponse.StatusCode, string(resp.Body))
	}
	return nil
}

// mergePatchObject encodes a status for a merge patch. The generated struct omits
// its empty fields, and a merge patch keeps whatever a field it omits held, so an
// operation that has ended would go on reading as running. Every field the status
// has is therefore sent, and an empty one as null, which removes it.
func mergePatchObject(status goclientnew.ReleaseLiveStatus) (map[string]any, error) {
	encoded, err := json.Marshal(status)
	if err != nil {
		return nil, err
	}
	present := map[string]any{}
	if err := json.Unmarshal(encoded, &present); err != nil {
		return nil, err
	}
	all := map[string]any{}
	for _, field := range reflect.VisibleFields(reflect.TypeOf(status)) {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		all[name] = present[name]
	}
	return all, nil
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

// projectStatus reads the Argo Application's status subresource into a Release
// LiveStatus. ObservedAt is left zero here so the result doubles as a stable dedup
// signature; the caller stamps it before writing.
func projectStatus(u *unstructured.Unstructured) goclientnew.ReleaseLiveStatus {
	obj := u.Object
	syncStatus, _, _ := unstructured.NestedString(obj, "status", "sync", "status")
	healthStatus, _, _ := unstructured.NestedString(obj, "status", "health", "status")
	healthMsg, _, _ := unstructured.NestedString(obj, "status", "health", "message")
	opPhase, _, _ := unstructured.NestedString(obj, "status", "operationState", "phase")
	opMsg, _, _ := unstructured.NestedString(obj, "status", "operationState", "message")

	// Prefer the health message; fall back to the operation message.
	msg := healthMsg
	if msg == "" {
		msg = opMsg
	}

	status := livestatus.FromArgoCD(syncStatus, healthStatus, opPhase)
	status.Reporter = liveStatusReporter
	status.DataSource = u.GetName()
	status.Message = truncate(msg, liveStatusMaxMessage)
	return status
}

// truncate shortens s to at most n bytes, appending an ellipsis marker when it
// cuts. Kept byte-oriented since the limit ConfigHub checks is in bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
