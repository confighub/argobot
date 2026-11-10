// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestSyncMetrics(t *testing.T) {
	successes, failures := syncSuccesses.Load(), syncFailures.Load()
	apps := map[string][]string{"a": {"oci://gw/space/s"}, "b": {"oci://gw/space/s"}}
	payload := map[string]any{"SpaceSlug": "s"}

	makeEventHandler(&fakeSyncer{apps: apps}, config{})(context.Background(), event(eventTypeReleasePublished, payload))
	makeEventHandler(&fakeSyncer{apps: apps, err: errors.New("boom")}, config{})(context.Background(), event(eventTypeReleasePublished, payload))

	if got := syncSuccesses.Load() - successes; got != 2 {
		t.Errorf("sync successes = %d, want 2", got)
	}
	if got := syncFailures.Load() - failures; got != 2 {
		t.Errorf("sync failures = %d, want 2", got)
	}
}

func TestMetricsHandler(t *testing.T) {
	before := reportFailures.Load()
	countOutcome(errors.New("boom"), &reportSuccesses, &reportFailures)
	countOutcome(nil, &reportSuccesses, &reportFailures)

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
		"# TYPE argobot_status_reports_total counter\n",
		`argobot_status_reports_total{controller="flux",result="success"} `,
		`argobot_status_reports_total{controller="flux",result="failure"} ` + strconv.FormatUint(before+1, 10) + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
}
