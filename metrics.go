// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"k8s.io/client-go/util/workqueue"
)

// Outcome counters, recorded with OpenTelemetry and served in the Prometheus
// text format at /metrics by the OpenTelemetry Prometheus exporter, which
// turns an instrument named argobot.syncs into argobot_syncs_total. Every
// data point carries a controller attribute naming the GitOps controller
// argobot drives.
//
// argobot.syncs counts Syncer.Sync calls, one per controller object an event
// resolved to. argobot.events.unresolved counts the events that resolved to
// none, which would otherwise leave no trace in the sync counts.
// argobot.status_reports counts the live-status reporter's passes over one
// watched object: a success is a pass that found the object's Release
// (whether or not it wrote, since an unchanged object writes nothing), a skip
// is one that found nothing to record on, and a failure is one that will be
// retried. A call that fails because argobot is shutting down is not counted.
const meterName = "github.com/confighub/argobot"

// Values of the result attribute.
const (
	resultSuccess = "success"
	resultSkipped = "skipped"
	resultFailure = "failure"
)

// instruments holds the counters. OpenTelemetry instruments are safe to use
// from the event handler and the reporter's worker at once.
type instruments struct {
	controller attribute.KeyValue
	syncs      metric.Int64Counter
	unresolved metric.Int64Counter
	reports    metric.Int64Counter
}

// meters holds the package's counters. Until main installs real ones with
// setMetrics they record to a no-op provider, so code paths that count work
// without metrics being set up (as in most tests). It is an atomic pointer so
// that installing the counters is safe whenever it happens, not only before
// the event handler and reporters start.
var meters atomic.Pointer[instruments]

func init() {
	meters.Store(mustNewInstruments(noop.NewMeterProvider(), ""))
}

func newInstruments(mp metric.MeterProvider, controller string) (*instruments, error) {
	m := mp.Meter(meterName)
	syncs, err := m.Int64Counter("argobot.syncs",
		metric.WithDescription("Syncs argobot requested of the GitOps controller, by result."),
		metric.WithUnit("{sync}"))
	if err != nil {
		return nil, err
	}
	unresolved, err := m.Int64Counter("argobot.events.unresolved",
		metric.WithDescription("Events argobot reacts to that resolved to no controller object to sync."),
		metric.WithUnit("{event}"))
	if err != nil {
		return nil, err
	}
	reports, err := m.Int64Counter("argobot.status_reports",
		metric.WithDescription("Live-status reports of one controller object, by result."),
		metric.WithUnit("{report}"))
	if err != nil {
		return nil, err
	}
	return &instruments{
		controller: attribute.String("controller", controller),
		syncs:      syncs,
		unresolved: unresolved,
		reports:    reports,
	}, nil
}

func mustNewInstruments(mp metric.MeterProvider, controller string) *instruments {
	i, err := newInstruments(mp, controller)
	if err != nil {
		panic(err)
	}
	return i
}

// setMetrics points the package's counters at mp, labeled with controller.
// Every series is created at zero, so each is exported from the start: a
// series Prometheus first sees at 1 hides that increment from increase() and
// rate(), and an alert on a failure series would otherwise see no data until
// the first failure.
func setMetrics(mp metric.MeterProvider, controller string) error {
	i, err := newInstruments(mp, controller)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for _, r := range []string{resultSuccess, resultFailure} {
		i.syncs.Add(ctx, 0, i.result(r))
	}
	i.unresolved.Add(ctx, 0, metric.WithAttributes(i.controller))
	for _, r := range []string{resultSuccess, resultSkipped, resultFailure} {
		i.reports.Add(ctx, 0, i.result(r))
	}
	meters.Store(i)
	return nil
}

func (i *instruments) result(r string) metric.AddOption {
	return metric.WithAttributes(i.controller, attribute.String("result", r))
}

// countUnresolved records an event that resolved to no sync target.
func countUnresolved(ctx context.Context) {
	m := meters.Load()
	m.unresolved.Add(ctx, 1, metric.WithAttributes(m.controller))
}

// countSync records the result of one Syncer.Sync call.
func countSync(ctx context.Context, err error) {
	m := meters.Load()
	switch {
	case err == nil:
		m.syncs.Add(ctx, 1, m.result(resultSuccess))
	case ctx.Err() == nil:
		m.syncs.Add(ctx, 1, m.result(resultFailure))
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
	m := meters.Load()
	switch {
	case err != nil:
		if ctx.Err() == nil {
			m.reports.Add(ctx, 1, m.result(resultFailure))
		}
	case outcome == reportSkipped:
		m.reports.Add(ctx, 1, m.result(resultSkipped))
	default:
		m.reports.Add(ctx, 1, m.result(resultSuccess))
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

// newPrometheusMetrics builds a MeterProvider whose metrics are exported to a
// fresh Prometheus registry, and a handler serving that registry. The registry
// holds only argobot's own counters: not the Go runtime collectors of the
// default registry, and not target_info. target_info would export the
// MeterProvider's resource, which the SDK always merges with
// OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME; every variable argobot reads
// is declared in envVars, and these are not. Prometheus identifies the target
// by its job and instance labels instead.
func newPrometheusMetrics() (*sdkmetric.MeterProvider, http.Handler, error) {
	reg := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(reg),
		otelprom.WithoutScopeInfo(), otelprom.WithoutTargetInfo())
	if err != nil {
		return nil, nil, fmt.Errorf("prometheus exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	return mp, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), nil
}

// setupMetrics points the package's counters at a Prometheus-exported
// MeterProvider and returns the handler that serves them. main calls it before
// anything counts, so no count is lost to the no-op counters. Like
// live-status reporting it is best-effort: on failure it warns, leaves the
// counters as no-ops, and returns nil.
func setupMetrics(controller string) http.Handler {
	mp, handler, err := newPrometheusMetrics()
	if err == nil {
		err = setMetrics(mp, controller)
	}
	if err != nil {
		log.Printf("[WARN] metrics disabled: %v", err)
		return nil
	}
	return handler
}

// serveMetrics serves handler at /metrics on addr until ctx is cancelled. A nil
// handler (metrics disabled) or a listener that cannot be opened returns nil at
// once rather than stopping argobot.
func serveMetrics(ctx context.Context, addr string, handler http.Handler) error {
	if handler == nil {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("[WARN] metrics disabled: %v", err)
		return nil
	}
	return serveMetricsOn(ctx, ln, handler)
}

// serveMetricsOn serves handler at /metrics on ln until ctx is cancelled, then
// closes it.
func serveMetricsOn(ctx context.Context, ln net.Listener, handler http.Handler) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", handler)
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
