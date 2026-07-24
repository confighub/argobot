// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/confighub/argobot/kube"
	"github.com/confighub/sdk/core/livestatus"
	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/confighub/sdk/core/worker/lib"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
)

// reporter watches Argo CD Application CRs and writes each one's live status
// back to its ConfigHub deployment Space as the confighub.com/live-status
// annotation. It maps an Application to its Space via the confighub.com/space-id
// label ConfigHub stamps on the Application, so the watched set is a
// self-updating, label-selected slice with no re-query: Applications appear and
// disappear in the informer as they are labeled.
//
// It is best-effort feedback, not control: a dropped or delayed report costs
// only freshness. Writes are deduplicated against the last projection so an
// idle Application produces no Space churn, and coalesced per Application so a
// sync's burst of updates is one write.
type reporter struct {
	frontdoor *lib.WorkerFrontdoorClient
	cub       *goclientnew.ClientWithResponses
	namespace string
	informer  cache.SharedIndexInformer
	queue     workqueue.TypedRateLimitingInterface[string]

	// lastSig deduplicates writes: appKey -> signature of the last projection
	// written (the encoded Status with ObservedAt omitted). Accessed only by the
	// single worker goroutine, so it needs no lock.
	lastSig map[string]string
}

// newReporter builds the live-status reporter. It constructs a dynamic
// Kubernetes client (in-cluster or kubeconfig) and a ConfigHub API client under
// the worker identity, and sets up a label-filtered informer over Argo
// Applications in the configured namespace.
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

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dyn, liveStatusResync, cfg.ArgoNamespace,
		func(o *metav1.ListOptions) {
			// A bare label key selects Applications that carry the label,
			// i.e. the deployment Applications ConfigHub created.
			o.LabelSelector = livestatus.LabelSpaceID
		},
	)
	informer := factory.ForResource(kube.ApplicationsGVR).Informer()

	r := &reporter{
		frontdoor: fc,
		cub:       client,
		namespace: cfg.ArgoNamespace,
		informer:  informer,
		queue:     workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		lastSig:   make(map[string]string),
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
	log.Printf("[INFO] live-status reporter: watching Argo Applications labeled %s in namespace %q",
		livestatus.LabelSpaceID, r.namespace)

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
// annotation.
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

	spaceIDStr := u.GetLabels()[livestatus.LabelSpaceID]
	if spaceIDStr == "" {
		return nil // not a ConfigHub-managed deployment Application
	}
	spaceID, err := uuid.Parse(spaceIDStr)
	if err != nil {
		return fmt.Errorf("application %s has invalid %s label %q: %w", key, livestatus.LabelSpaceID, spaceIDStr, err)
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
