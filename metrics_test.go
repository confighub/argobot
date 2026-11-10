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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/util/workqueue"
)

// The counters are package state, so these tests compare differences and must
// not call t.Parallel.

func TestSyncMetrics(t *testing.T) {
	successes, failures, unresolved := syncSuccesses.Load(), syncFailures.Load(), unresolvedEvents.Load()
	apps := map[string][]string{"a": {"oci://gw/space/s"}, "b": {"oci://gw/space/s"}}
	payload := map[string]any{"SpaceSlug": "s"}

	makeEventHandler(&fakeSyncer{apps: apps}, config{})(context.Background(), event(eventTypeReleasePublished, payload))
	makeEventHandler(&fakeSyncer{apps: apps, err: errors.New("boom")}, config{})(context.Background(), event(eventTypeReleasePublished, payload))
	makeEventHandler(&fakeSyncer{}, config{})(context.Background(), event(eventTypeReleasePublished, map[string]any{"SpaceSlug": "other"}))

	// A sync that fails because argobot is shutting down is not a failure.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	makeEventHandler(&fakeSyncer{apps: apps, err: context.Canceled}, config{})(cancelled, event(eventTypeReleasePublished, payload))

	if got := syncSuccesses.Load() - successes; got != 2 {
		t.Errorf("sync successes = %d, want 2", got)
	}
	if got := syncFailures.Load() - failures; got != 2 {
		t.Errorf("sync failures = %d, want 2", got)
	}
	if got := unresolvedEvents.Load() - unresolved; got != 1 {
		t.Errorf("unresolved events = %d, want 1", got)
	}
}

func TestProcessReport(t *testing.T) {
	cases := []struct {
		name        string
		cancel      bool
		outcome     reportOutcome
		err         error
		counter     *atomic.Uint64 // the counter that must go up by one; nil for none
		wantRequeue bool
	}{
		{name: "recorded", outcome: reportRecorded, counter: &reportSuccesses},
		{name: "skipped", outcome: reportSkipped, counter: &reportSkips},
		{name: "failed", err: errors.New("boom"), counter: &reportFailures, wantRequeue: true},
		{name: "failed while shutting down", cancel: true, err: context.Canceled, wantRequeue: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
			defer queue.ShutDown()
			queue.Add("ns/obj")

			before := map[*atomic.Uint64]uint64{}
			for _, c := range []*atomic.Uint64{&reportSuccesses, &reportSkips, &reportFailures} {
				before[c] = c.Load()
			}
			var reported string
			more := processReport(ctx, queue, func(_ context.Context, key string) (reportOutcome, error) {
				reported = key
				return tc.outcome, tc.err
			})

			if !more || reported != "ns/obj" {
				t.Fatalf("processReport = %v, reported %q; want true, \"ns/obj\"", more, reported)
			}
			for c, b := range before {
				want := b
				if c == tc.counter {
					want++
				}
				if got := c.Load(); got != want {
					t.Errorf("counter went from %d to %d, want %d", b, got, want)
				}
			}
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

func TestMetricsHandler(t *testing.T) {
	before := reportFailures.Load()
	countReport(context.Background(), reportFailed, errors.New("boom"))
	countReport(context.Background(), reportRecorded, nil)

	rec := httptest.NewRecorder()
	metricsHandler(ControllerFlux).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	text := string(body)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", ct)
	}
	for _, want := range []string{
		"# TYPE argobot_syncs_total counter\n",
		`argobot_syncs_total{controller="flux",result="success"} `,
		`argobot_syncs_total{controller="flux",result="failure"} `,
		"# TYPE argobot_events_unresolved_total counter\n",
		`argobot_events_unresolved_total{controller="flux"} `,
		"# TYPE argobot_status_reports_total counter\n",
		`argobot_status_reports_total{controller="flux",result="success"} `,
		`argobot_status_reports_total{controller="flux",result="skipped"} `,
		`argobot_status_reports_total{controller="flux",result="failure"} ` + strconv.FormatUint(before+1, 10) + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
}

func TestServeMetrics(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveMetricsOn(ctx, ln, ControllerArgoCD) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `argobot_syncs_total{controller="argocd"`) {
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

func TestServeMetricsAddressInUse(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	// Best-effort: an address that cannot be opened returns nil at once rather
	// than failing argobot or blocking.
	done := make(chan error, 1)
	go func() { done <- serveMetrics(context.Background(), taken.Addr().String(), ControllerArgoCD) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveMetrics = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveMetrics blocked on an address in use")
	}
}
