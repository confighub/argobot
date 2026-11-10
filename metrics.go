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
)

// Outcome counters, served in the Prometheus text format at /metrics. They are
// few and fixed, so argobot keeps them itself rather than linking a metrics
// library: each is an atomic counter, safe to bump from the event handler and
// the reporter's worker at once.
//
// argobot_syncs_total counts Syncer.Sync calls, one per controller object an
// event resolved to. argobot_status_reports_total counts the live-status
// reporter's passes over one watched object: a success is a pass that finished,
// whether or not it wrote (an unchanged object writes nothing), and a failure
// is one that will be retried.
var (
	syncSuccesses   atomic.Uint64
	syncFailures    atomic.Uint64
	reportSuccesses atomic.Uint64
	reportFailures  atomic.Uint64
)

// countOutcome adds one to success when err is nil, otherwise to failure.
func countOutcome(err error, success, failure *atomic.Uint64) {
	if err != nil {
		failure.Add(1)
		return
	}
	success.Add(1)
}

// writeMetrics writes the counters in the Prometheus text exposition format,
// labeled with the controller argobot drives.
func writeMetrics(w io.Writer, controller string) error {
	_, err := fmt.Fprintf(w, `# HELP argobot_syncs_total Syncs argobot requested of the GitOps controller, by result.
# TYPE argobot_syncs_total counter
argobot_syncs_total{controller=%[1]q,result="success"} %[2]d
argobot_syncs_total{controller=%[1]q,result="failure"} %[3]d
# HELP argobot_status_reports_total Live-status reports of one controller object, by result.
# TYPE argobot_status_reports_total counter
argobot_status_reports_total{controller=%[1]q,result="success"} %[4]d
argobot_status_reports_total{controller=%[1]q,result="failure"} %[5]d
`, controller, syncSuccesses.Load(), syncFailures.Load(), reportSuccesses.Load(), reportFailures.Load())
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
	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler(controller))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("[WARN] metrics disabled: %v", err)
		return nil
	}
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
