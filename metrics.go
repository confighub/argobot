// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"k8s.io/client-go/util/workqueue"
)

// Outcome counters, served in the Prometheus text format at /metrics. They are
// few and fixed, so argobot keeps them itself rather than linking a metrics
// library: each is an atomic counter, safe to bump from the event handler and
// the reporter's worker at once. They are package state, so tests read them
// before and after and compare the difference; such tests must not run in
// parallel with each other.
//
// argobot_syncs_total counts Syncer.Sync calls, one per controller object an
// event resolved to. argobot_events_unresolved_total counts the events that
// resolved to none, which would otherwise leave no trace in the sync counts.
// argobot_status_reports_total counts the live-status reporter's passes over
// one watched object: a success is a pass that found the object's Release
// (whether or not it wrote, since an unchanged object writes nothing), a skip
// is one that found nothing to record on, and a failure is one that will be
// retried. A call that fails because argobot is shutting down is not counted.
var (
	syncSuccesses    atomic.Uint64
	syncFailures     atomic.Uint64
	unresolvedEvents atomic.Uint64
	reportSuccesses  atomic.Uint64
	reportSkips      atomic.Uint64
	reportFailures   atomic.Uint64
)

// countSync records the result of one Syncer.Sync call.
func countSync(ctx context.Context, err error) {
	switch {
	case err == nil:
		syncSuccesses.Add(1)
	case ctx.Err() == nil:
		syncFailures.Add(1)
	}
}

// reportOutcome is how one live-status pass over a watched object ended.
type reportOutcome int

const (
	// reportFailed: the pass returned an error and will be retried.
	reportFailed reportOutcome = iota
	// reportSkipped: nothing to record on. The object is gone, or it names no
	// Space or Release argobot can see.
	reportSkipped
	// reportRecorded: the object's Release was found, and its LiveStatus was
	// written or was already current.
	reportRecorded
)

// countReport records the result of one live-status pass.
func countReport(ctx context.Context, outcome reportOutcome, err error) {
	switch {
	case err != nil:
		if ctx.Err() == nil {
			reportFailures.Add(1)
		}
	case outcome == reportSkipped:
		reportSkips.Add(1)
	default:
		reportSuccesses.Add(1)
	}
}

// processReport takes one key off a reporter's queue, reports it, counts the
// outcome, and requeues it with backoff on error. It returns false once the
// queue is shut down. Every reporter's worker loops on it.
func processReport(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string],
	report func(context.Context, string) (reportOutcome, error)) bool {
	key, shutdown := queue.Get()
	if shutdown {
		return false
	}
	defer queue.Done(key)

	outcome, err := report(ctx, key)
	countReport(ctx, outcome, err)
	if err != nil {
		log.Printf("[WARN] live-status reporter: %v", err)
		queue.AddRateLimited(key)
		return true
	}
	queue.Forget(key)
	return true
}

// writeMetrics writes the counters in the Prometheus text exposition format,
// labeled with the controller argobot drives.
func writeMetrics(w io.Writer, controller string) error {
	_, err := fmt.Fprintf(w, `# HELP argobot_syncs_total Syncs argobot requested of the GitOps controller, by result.
# TYPE argobot_syncs_total counter
argobot_syncs_total{controller=%[1]q,result="success"} %[2]d
argobot_syncs_total{controller=%[1]q,result="failure"} %[3]d
# HELP argobot_events_unresolved_total Events argobot reacts to that resolved to no controller object to sync.
# TYPE argobot_events_unresolved_total counter
argobot_events_unresolved_total{controller=%[1]q} %[4]d
# HELP argobot_status_reports_total Live-status reports of one controller object, by result.
# TYPE argobot_status_reports_total counter
argobot_status_reports_total{controller=%[1]q,result="success"} %[5]d
argobot_status_reports_total{controller=%[1]q,result="skipped"} %[6]d
argobot_status_reports_total{controller=%[1]q,result="failure"} %[7]d
`, controller, syncSuccesses.Load(), syncFailures.Load(), unresolvedEvents.Load(),
		reportSuccesses.Load(), reportSkips.Load(), reportFailures.Load())
	return err
}

// metricsHandler serves the counters.
func metricsHandler(controller string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := writeMetrics(w, controller); err != nil {
			log.Printf("[WARN] metrics: writing response: %v", err)
		}
	})
}

// serveMetrics serves /metrics on addr until ctx is cancelled. Like live-status
// reporting it is best-effort: a listener that cannot be opened warns and
// returns nil rather than stopping argobot.
func serveMetrics(ctx context.Context, addr, controller string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("[WARN] metrics disabled: %v", err)
		return nil
	}
	return serveMetricsOn(ctx, ln, controller)
}

// serveMetricsOn serves /metrics on ln until ctx is cancelled, then closes it.
func serveMetricsOn(ctx context.Context, ln net.Listener, controller string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler(controller))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("[INFO] argobot: serving metrics on %s/metrics", ln.Addr())

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("[WARN] metrics server stopped: %v", err)
	}
	return nil
}
