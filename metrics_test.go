// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"k8s.io/client-go/util/workqueue"
)

// useTestMetrics points the package's counters at a fresh in-memory reader for
// the duration of the test. The counters are package state, so tests that use
// it must not call t.Parallel.
func useTestMetrics(t *testing.T, controller string) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := meters.Load()
	if err := setMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), controller); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { meters.Store(prev) })
	return reader
}

// collectCounts returns every counter data point, keyed "name" or
// "name/result", checking that each carries the controller attribute.
func collectCounts(t *testing.T, reader *sdkmetric.ManualReader, controller string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || !sum.IsMonotonic {
				t.Fatalf("%s is %T, want a monotonic int64 sum", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				if c, _ := dp.Attributes.Value("controller"); c.AsString() != controller {
					t.Errorf("%s controller = %q, want %q", m.Name, c.AsString(), controller)
				}
				key := m.Name
				if r, ok := dp.Attributes.Value("result"); ok {
					key += "/" + r.AsString()
				}
				counts[key] += dp.Value
			}
		}
	}
	return counts
}

func assertCounts(t *testing.T, got, want map[string]int64) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %d, want %d", k, got[k], w)
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok && g != 0 {
			t.Errorf("%s = %d, want 0", k, g)
		}
	}
}

func TestSyncMetrics(t *testing.T) {
	reader := useTestMetrics(t, ControllerArgoCD)
	apps := map[string][]string{"a": {"oci://gw/space/s"}, "b": {"oci://gw/space/s"}}
	payload := map[string]any{"SpaceSlug": "s"}

	makeEventHandler(&fakeSyncer{apps: apps}, config{})(context.Background(), event(eventTypeReleasePublished, payload))
	makeEventHandler(&fakeSyncer{apps: apps, err: errors.New("boom")}, config{})(context.Background(), event(eventTypeReleasePublished, payload))
	makeEventHandler(&fakeSyncer{}, config{})(context.Background(), event(eventTypeReleasePublished, map[string]any{"SpaceSlug": "other"}))

	// A sync that fails because argobot is shutting down is not a failure.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	makeEventHandler(&fakeSyncer{apps: apps, err: context.Canceled}, config{})(cancelled, event(eventTypeReleasePublished, payload))

	assertCounts(t, collectCounts(t, reader, ControllerArgoCD), map[string]int64{
		"argobot.syncs/success":     2,
		"argobot.syncs/failure":     2,
		"argobot.events.unresolved": 1,
	})
}

func TestProcessReport(t *testing.T) {
	cases := []struct {
		name        string
		cancel      bool
		outcome     reportOutcome
		err         error
		counter     string // the count that must be one; empty for none
		wantRequeue bool
	}{
		{name: "recorded", outcome: reportRecorded, counter: "argobot.status_reports/success"},
		{name: "skipped", outcome: reportSkipped, counter: "argobot.status_reports/skipped"},
		{name: "failed", err: errors.New("boom"), counter: "argobot.status_reports/failure", wantRequeue: true},
		{name: "failed while shutting down", cancel: true, err: context.Canceled, wantRequeue: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := useTestMetrics(t, ControllerFlux)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
			defer queue.ShutDown()
			queue.Add("ns/obj")

			var reported string
			more := processReport(ctx, queue, func(_ context.Context, key string) (reportOutcome, error) {
				reported = key
				return tc.outcome, tc.err
			})

			if !more || reported != "ns/obj" {
				t.Fatalf("processReport = %v, reported %q; want true, \"ns/obj\"", more, reported)
			}
			want := map[string]int64{}
			if tc.counter != "" {
				want[tc.counter] = 1
			}
			assertCounts(t, collectCounts(t, reader, ControllerFlux), want)
			if got := queue.NumRequeues("ns/obj") > 0; got != tc.wantRequeue {
				t.Errorf("requeued = %v, want %v", got, tc.wantRequeue)
			}
		})
	}

	t.Run("shut down", func(t *testing.T) {
		queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
		queue.ShutDown()
		if processReport(context.Background(), queue, func(context.Context, string) (reportOutcome, error) {
			t.Error("report called on a shut-down queue")
			return reportRecorded, nil
		}) {
			t.Error("processReport = true on a shut-down queue, want false")
		}
	})
}

// TestPrometheusExposition checks the names and labels the exporter gives the
// counters, which dashboards and alerts depend on.
func TestPrometheusExposition(t *testing.T) {
	// OTel's own environment variables, which argobot does not declare, must
	// not reach /metrics.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "leaked=yes")
	t.Setenv("OTEL_SERVICE_NAME", "leaked")
	prev := meters.Load()
	t.Cleanup(func() { meters.Store(prev) })
	handler := setupMetrics(ControllerFlux)
	if handler == nil {
		t.Fatal("setupMetrics = nil")
	}

	// Every series is exported at zero before anything is counted.
	fresh := scrape(t, handler)
	for _, want := range []string{
		`argobot_syncs_total{controller="flux",result="success"} 0` + "\n",
		`argobot_syncs_total{controller="flux",result="failure"} 0` + "\n",
		`argobot_events_unresolved_total{controller="flux"} 0` + "\n",
		`argobot_status_reports_total{controller="flux",result="success"} 0` + "\n",
		`argobot_status_reports_total{controller="flux",result="skipped"} 0` + "\n",
		`argobot_status_reports_total{controller="flux",result="failure"} 0` + "\n",
	} {
		if !strings.Contains(fresh, want) {
			t.Errorf("metrics before any count missing %q:\n%s", want, fresh)
		}
	}
	if strings.Contains(fresh, "leaked") || strings.Contains(fresh, "target_info") {
		t.Errorf("metrics carry target_info or OTEL_* environment attributes:\n%s", fresh)
	}

	countSync(context.Background(), nil)
	countSync(context.Background(), errors.New("boom"))
	countUnresolved(context.Background())
	countReport(context.Background(), reportRecorded, nil)
	countReport(context.Background(), reportSkipped, nil)
	countReport(context.Background(), reportFailed, errors.New("boom"))
	countReport(context.Background(), reportFailed, errors.New("boom"))

	text := scrape(t, handler)
	for _, want := range []string{
		"# TYPE argobot_syncs_total counter\n",
		`argobot_syncs_total{controller="flux",result="success"} 1` + "\n",
		`argobot_syncs_total{controller="flux",result="failure"} 1` + "\n",
		"# TYPE argobot_events_unresolved_total counter\n",
		`argobot_events_unresolved_total{controller="flux"} 1` + "\n",
		"# TYPE argobot_status_reports_total counter\n",
		`argobot_status_reports_total{controller="flux",result="success"} 1` + "\n",
		`argobot_status_reports_total{controller="flux",result="skipped"} 1` + "\n",
		`argobot_status_reports_total{controller="flux",result="failure"} 2` + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
}

// scrape fetches /metrics from handler in the Prometheus text format.
func scrape(t *testing.T, handler http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", ct)
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestServeMetrics(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	go func() { done <- serveMetricsOn(ctx, ln, handler) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("GET /metrics = %d:\n%s", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveMetricsOn = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveMetricsOn did not return after cancel")
	}
}

func TestServeMetricsNotServing(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	handler := http.NotFoundHandler()

	// Best-effort: an address that cannot be opened, or metrics that could not
	// be set up (a nil handler), return nil at once rather than failing
	// argobot or blocking.
	for name, call := range map[string]func() error{
		"address in use":   func() error { return serveMetrics(context.Background(), taken.Addr().String(), handler) },
		"metrics disabled": func() error { return serveMetrics(context.Background(), "127.0.0.1:0", nil) },
	} {
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s: serveMetrics = %v, want nil", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: serveMetrics blocked", name)
		}
	}
}
