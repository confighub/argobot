// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"slices"
	"strings"
	"testing"

	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
)

// event builds a delivered event the way the generated client decodes one: the
// payload a generic JSON object.
func event(eventType string, payload map[string]any) *goclientnew.EventLog {
	e := &goclientnew.EventLog{EventType: eventType}
	if payload != nil {
		e.Payload = payload
	}
	return e
}

func TestResolveAppNames(t *testing.T) {
	payload := func(v map[string]any) map[string]any { return v }

	// Applications as the syncer lists them: name -> source repoURLs.
	apps := map[string][]string{
		"nonprod-argobot":  nil,
		"nonprod-argocd":   {"oci://gw/space/nonprod-argocd"},
		"dev-1-apptique":   {"oci://gw/space/argo-apptique-dev-1"},
		"app-dev":          {"oci://gw/space/app-dev"},
		"app-dev-2":        {"oci://gw/space/app-dev-2"},
		"multi":            {"https://charts.example.com/stable", "oci://gw/space/argo-multi"},
		"argo-both":        {"oci://gw/space/argo-both"},
		"both-renamed":     {"oci://gw/space/argo-both/"},
		"with-other-space": {"oci://gw/space/elsewhere"},
	}

	tests := []struct {
		name  string
		cfg   config
		entry *goclientnew.EventLog
		want  []string
	}{
		{
			name:  "release payload resolves via SpaceSlug",
			entry: event(eventTypeReleasePublished, payload(map[string]any{"BundleBaseName": "e9786ad6-uuid", "SpaceSlug": "nonprod-argobot", "ReleaseNum": 5})),
			want:  []string{"nonprod-argobot"},
		},
		{
			name:  "apply payload resolves via SpaceSlug",
			entry: event(eventTypeApplyCompleted, payload(map[string]any{"SpaceSlug": "nonprod-argocd"})),
			want:  []string{"nonprod-argocd"},
		},
		{
			name:  "ArgoApp override wins over payload",
			cfg:   config{ArgoApp: "pinned-app"},
			entry: event(eventTypeReleasePublished, payload(map[string]any{"SpaceSlug": "nonprod-argobot"})),
			want:  []string{"pinned-app"},
		},
		{
			name:  "Application named differently is found by its source",
			entry: event(eventTypeReleasePublished, payload(map[string]any{"SpaceSlug": "argo-apptique-dev-1"})),
			want:  []string{"dev-1-apptique"},
		},
		{
			name:  "multi-source Application is found by any of its sources",
			entry: event(eventTypeReleasePublished, payload(map[string]any{"SpaceSlug": "argo-multi"})),
			want:  []string{"multi"},
		},
		{
			name:  "Application matching by name and by source is listed once, with every other match",
			entry: event(eventTypeReleasePublished, payload(map[string]any{"SpaceSlug": "argo-both"})),
			want:  []string{"argo-both", "both-renamed"},
		},
		{
			name:  "a shared prefix is not a match",
			entry: event(eventTypeReleasePublished, payload(map[string]any{"SpaceSlug": "app-dev"})),
			want:  []string{"app-dev"},
		},
		{
			name:  "no Application for the Space resolves to none",
			entry: event(eventTypeReleasePublished, payload(map[string]any{"SpaceSlug": "unknown-space"})),
			want:  nil,
		},
		{
			name:  "BundleBaseName alone resolves to none",
			entry: event(eventTypeReleasePublished, payload(map[string]any{"BundleBaseName": "e9786ad6-uuid"})),
			want:  nil,
		},
		{
			name:  "nil payload resolves to none",
			entry: event(eventTypeApplyCompleted, nil),
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveAppNames(context.Background(), &fakeSyncer{apps: apps}, tt.cfg, tt.entry)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("resolveAppNames() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveAppNamesListFailureFallsBackToName(t *testing.T) {
	syncer := &fakeSyncer{listErr: errors.New("forbidden")}
	entry := event(eventTypeReleasePublished, map[string]any{"SpaceSlug": "nonprod-argobot"})
	got := resolveAppNames(context.Background(), syncer, config{}, entry)
	if want := []string{"nonprod-argobot"}; !slices.Equal(got, want) {
		t.Fatalf("resolveAppNames() = %v, want %v", got, want)
	}
}

func TestAppNamesForSpace(t *testing.T) {
	// /space/app-dev must not match the Space app-dev-2, nor the reverse.
	apps := map[string][]string{
		"a": {"oci://gw/space/app-dev"},
		"b": {"oci://gw/space/app-dev-2"},
		"c": {"oci://gw/space/"},
		"d": {"oci://gw/space/app-dev/extra"},
		// Sveltos profiles keep the tag in the URL.
		"team-a/e": {"oci://gw/space/app-dev-2:latest"},
	}
	if got, want := appNamesForSpace(apps, "app-dev"), []string{"a"}; !slices.Equal(got, want) {
		t.Errorf("appNamesForSpace(app-dev) = %v, want %v", got, want)
	}
	if got, want := appNamesForSpace(apps, "app-dev-2"), []string{"b", "team-a/e"}; !slices.Equal(got, want) {
		t.Errorf("appNamesForSpace(app-dev-2) = %v, want %v", got, want)
	}
}

func TestSubscriptions(t *testing.T) {
	subs := subscriptions()
	if len(subs) != 1 {
		t.Fatalf("len = %d, want 1", len(subs))
	}
	got := subs[0].EventTypes
	if len(got) != 2 || got[0] != eventTypeApplyCompleted || got[1] != eventTypeReleasePublished {
		t.Fatalf("EventTypes = %v, want [%s %s]", got, eventTypeApplyCompleted, eventTypeReleasePublished)
	}
	if subs[0].SpaceID != nil || subs[0].TargetID != nil {
		t.Fatalf("default subscription is scoped: %+v", subs[0])
	}
}

// fakeSyncer records the app names it was asked to sync and lists apps (name ->
// source repoURLs) as the Applications it can see.
type fakeSyncer struct {
	synced  []string
	err     error
	apps    map[string][]string
	listErr error
}

func (f *fakeSyncer) Sync(_ context.Context, appName string) error {
	f.synced = append(f.synced, appName)
	return f.err
}

func (f *fakeSyncer) RepoURLs(context.Context) (map[string][]string, error) {
	return f.apps, f.listErr
}

func TestMakeEventHandler(t *testing.T) {
	payload := func(slug string) map[string]any { return map[string]any{"SpaceSlug": slug} }

	// handle delivers a release for slug to a handler over apps and returns what
	// was synced and what was logged.
	handle := func(t *testing.T, apps map[string][]string, slug string) (synced []string, logged string) {
		t.Helper()
		var buf bytes.Buffer
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(os.Stderr) })

		syncer := &fakeSyncer{apps: apps}
		makeEventHandler(syncer, config{})(context.Background(), event(eventTypeReleasePublished, payload(slug)))
		return syncer.synced, buf.String()
	}

	t.Run("resolved app is synced", func(t *testing.T) {
		got, _ := handle(t, map[string][]string{"nonprod-argobot": nil}, "nonprod-argobot")
		if want := []string{"nonprod-argobot"}; !slices.Equal(got, want) {
			t.Fatalf("synced = %v, want %v", got, want)
		}
	})

	t.Run("app found by name only", func(t *testing.T) {
		apps := map[string][]string{"orders": {"https://git.example.com/orders.git"}, "other": nil}
		got, _ := handle(t, apps, "orders")
		if want := []string{"orders"}; !slices.Equal(got, want) {
			t.Fatalf("synced = %v, want %v", got, want)
		}
	})

	t.Run("app found by source only, under a different name", func(t *testing.T) {
		apps := map[string][]string{
			"dev-1-apptique": {"oci://gw/space/argo-apptique-dev-1"},
			"dev-2-apptique": {"oci://gw/space/argo-apptique-dev-2"},
		}
		got, _ := handle(t, apps, "argo-apptique-dev-1")
		if want := []string{"dev-1-apptique"}; !slices.Equal(got, want) {
			t.Fatalf("synced = %v, want %v", got, want)
		}
	})

	t.Run("multi-source app found by one of its sources", func(t *testing.T) {
		apps := map[string][]string{"multi": {"https://charts.example.com/stable", "oci://gw/space/argo-multi"}}
		got, _ := handle(t, apps, "argo-multi")
		if want := []string{"multi"}; !slices.Equal(got, want) {
			t.Fatalf("synced = %v, want %v", got, want)
		}
	})

	t.Run("app matching by name and source is synced once, alongside the others", func(t *testing.T) {
		apps := map[string][]string{
			"argo-both":    {"oci://gw/space/argo-both"},
			"both-renamed": {"oci://gw/space/argo-both"},
		}
		got, _ := handle(t, apps, "argo-both")
		if want := []string{"argo-both", "both-renamed"}; !slices.Equal(got, want) {
			t.Fatalf("synced = %v, want %v", got, want)
		}
	})

	t.Run("a Space with a shared prefix is not synced", func(t *testing.T) {
		apps := map[string][]string{
			"app-dev":   {"oci://gw/space/app-dev"},
			"app-dev-2": {"oci://gw/space/app-dev-2"},
			"other":     {"oci://gw/space/app-dev-20"},
		}
		got, _ := handle(t, apps, "app-dev")
		if want := []string{"app-dev"}; !slices.Equal(got, want) {
			t.Fatalf("synced = %v, want %v", got, want)
		}
	})

	t.Run("no app for the Space warns once and syncs nothing", func(t *testing.T) {
		apps := map[string][]string{"elsewhere": {"oci://gw/space/elsewhere"}}
		got, logged := handle(t, apps, "argo-apptique-dev-1")
		if len(got) != 0 {
			t.Fatalf("synced = %v, want none", got)
		}
		if n := strings.Count(logged, "\n"); n != 1 {
			t.Fatalf("logged %d lines, want 1:\n%s", n, logged)
		}
		if !strings.Contains(logged, "[WARN]") || !strings.Contains(logged, `"argo-apptique-dev-1"`) {
			t.Fatalf("log = %q, want a WARN naming the slug", logged)
		}
		if strings.Contains(logged, "[ERROR]") {
			t.Fatalf("log = %q, want no error", logged)
		}
	})

	t.Run("a failed sync does not stop the others", func(t *testing.T) {
		syncer := &fakeSyncer{
			apps: map[string][]string{"a": {"oci://gw/space/s"}, "b": {"oci://gw/space/s"}},
			err:  errors.New("boom"),
		}
		makeEventHandler(syncer, config{})(context.Background(), event(eventTypeReleasePublished, payload("s")))
		if want := []string{"a", "b"}; !slices.Equal(syncer.synced, want) {
			t.Fatalf("synced = %v, want %v", syncer.synced, want)
		}
	})

	t.Run("unresolvable event is skipped", func(t *testing.T) {
		syncer := &fakeSyncer{}
		handler := makeEventHandler(syncer, config{})
		handler(context.Background(), event(eventTypeApplyCompleted, nil))
		if len(syncer.synced) != 0 {
			t.Fatalf("synced = %v, want none", syncer.synced)
		}
	})

	t.Run("an event type argobot does not react to is skipped, even with ArgoApp set", func(t *testing.T) {
		var buf bytes.Buffer
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(os.Stderr) })
		syncer := &fakeSyncer{}
		makeEventHandler(syncer, config{ArgoApp: "pinned-app"})(context.Background(), event("unit.updated", payload("s")))
		if len(syncer.synced) != 0 {
			t.Fatalf("synced = %v, want none", syncer.synced)
		}
		if !strings.Contains(buf.String(), "unit.updated") {
			t.Fatalf("log = %q, want a WARN naming the event type", buf.String())
		}
	})

	t.Run("ArgoApp override is synced", func(t *testing.T) {
		syncer := &fakeSyncer{}
		handler := makeEventHandler(syncer, config{ArgoApp: "pinned-app"})
		handler(context.Background(), event(eventTypeApplyCompleted, payload("ignored-slug")))
		if got := syncer.synced; len(got) != 1 || got[0] != "pinned-app" {
			t.Fatalf("synced = %v, want [pinned-app]", got)
		}
	})
}
