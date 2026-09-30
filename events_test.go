// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/confighub/sdk/core/worker/api"
)

func TestResolveAppNames(t *testing.T) {
	payload := func(v map[string]any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

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
		entry api.EventLogEntry
		want  []string
	}{
		{
			name:  "release payload resolves via SpaceSlug",
			entry: api.EventLogEntry{EventType: eventTypeReleasePublished, Payload: payload(map[string]any{"BundleBaseName": "e9786ad6-uuid", "SpaceSlug": "nonprod-argobot", "ReleaseNum": 5})},
			want:  []string{"nonprod-argobot"},
		},
		{
			name:  "apply payload resolves via SpaceSlug",
			entry: api.EventLogEntry{EventType: eventTypeApplyCompleted, Payload: payload(map[string]any{"SpaceSlug": "nonprod-argocd"})},
			want:  []string{"nonprod-argocd"},
		},
		{
			name:  "ArgoApp override wins over payload",
			cfg:   config{ArgoApp: "pinned-app"},
			entry: api.EventLogEntry{EventType: eventTypeReleasePublished, Payload: payload(map[string]any{"SpaceSlug": "nonprod-argobot"})},
			want:  []string{"pinned-app"},
		},
		{
			name:  "Application named differently is found by its source",
			entry: api.EventLogEntry{EventType: eventTypeReleasePublished, Payload: payload(map[string]any{"SpaceSlug": "argo-apptique-dev-1"})},
			want:  []string{"dev-1-apptique"},
		},
		{
			name:  "multi-source Application is found by any of its sources",
			entry: api.EventLogEntry{EventType: eventTypeReleasePublished, Payload: payload(map[string]any{"SpaceSlug": "argo-multi"})},
			want:  []string{"multi"},
		},
		{
			name:  "Application matching by name and by source is listed once, with every other match",
			entry: api.EventLogEntry{EventType: eventTypeReleasePublished, Payload: payload(map[string]any{"SpaceSlug": "argo-both"})},
			want:  []string{"argo-both", "both-renamed"},
		},
		{
			name:  "a shared prefix is not a match",
			entry: api.EventLogEntry{EventType: eventTypeReleasePublished, Payload: payload(map[string]any{"SpaceSlug": "app-dev"})},
			want:  []string{"app-dev"},
		},
		{
			name:  "no Application for the Space resolves to none",
			entry: api.EventLogEntry{EventType: eventTypeReleasePublished, Payload: payload(map[string]any{"SpaceSlug": "unknown-space"})},
			want:  nil,
		},
		{
			name:  "BundleBaseName alone resolves to none",
			entry: api.EventLogEntry{EventType: eventTypeReleasePublished, Payload: payload(map[string]any{"BundleBaseName": "e9786ad6-uuid"})},
			want:  nil,
		},
		{
			name:  "nil payload resolves to none",
			entry: api.EventLogEntry{EventType: eventTypeApplyCompleted},
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
	entry := api.EventLogEntry{EventType: eventTypeReleasePublished, Payload: json.RawMessage(`{"SpaceSlug":"nonprod-argobot"}`)}
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
	}
	if got, want := appNamesForSpace(apps, "app-dev"), []string{"a"}; !slices.Equal(got, want) {
		t.Errorf("appNamesForSpace(app-dev) = %v, want %v", got, want)
	}
	if got, want := appNamesForSpace(apps, "app-dev-2"), []string{"b"}; !slices.Equal(got, want) {
		t.Errorf("appNamesForSpace(app-dev-2) = %v, want %v", got, want)
	}
}

func TestSubscriptionsForTargets(t *testing.T) {
	base := config{SubscriptionName: "argobot"}

	t.Run("EventTargetID override wins and ignores discovered targets", func(t *testing.T) {
		cfg := base
		cfg.EventTargetID = "tgt-override"
		subs := subscriptionsForTargets(cfg, []string{"tgt-a", "tgt-b"})
		if len(subs) != 1 {
			t.Fatalf("len = %d, want 1", len(subs))
		}
		if subs[0].Name != "argobot" || subs[0].TargetID != "tgt-override" {
			t.Fatalf("got name=%q target=%q, want name=argobot target=tgt-override", subs[0].Name, subs[0].TargetID)
		}
	})

	t.Run("discovered targets yield one scoped subscription each", func(t *testing.T) {
		subs := subscriptionsForTargets(base, []string{"tgt-a", "tgt-b"})
		if len(subs) != 2 {
			t.Fatalf("len = %d, want 2", len(subs))
		}
		want := map[string]string{"argobot-tgt-a": "tgt-a", "argobot-tgt-b": "tgt-b"}
		for _, s := range subs {
			if want[s.Name] != s.TargetID {
				t.Fatalf("subscription %q scoped to %q, want %q", s.Name, s.TargetID, want[s.Name])
			}
		}
	})

	t.Run("no override and no discovered targets falls back to unscoped", func(t *testing.T) {
		subs := subscriptionsForTargets(base, nil)
		if len(subs) != 1 {
			t.Fatalf("len = %d, want 1", len(subs))
		}
		if subs[0].Name != "argobot" || subs[0].TargetID != "" {
			t.Fatalf("got name=%q target=%q, want name=argobot target=\"\"", subs[0].Name, subs[0].TargetID)
		}
	})

	t.Run("EventSpaceID narrows every subscription", func(t *testing.T) {
		cfg := base
		cfg.EventSpaceID = "space-x"
		subs := subscriptionsForTargets(cfg, []string{"tgt-a", "tgt-b"})
		for _, s := range subs {
			if s.SpaceID != "space-x" {
				t.Fatalf("subscription %q SpaceID = %q, want space-x", s.Name, s.SpaceID)
			}
		}
	})

	t.Run("every subscription carries the apply and release event types", func(t *testing.T) {
		subs := subscriptionsForTargets(base, []string{"tgt-a"})
		got := subs[0].EventTypes
		if len(got) != 2 || got[0] != eventTypeApplyCompleted || got[1] != eventTypeReleasePublished {
			t.Fatalf("EventTypes = %v, want [%s %s]", got, eventTypeApplyCompleted, eventTypeReleasePublished)
		}
	})
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
	payload := func(slug string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"SpaceSlug": slug})
		return b
	}

	// handle delivers a release for slug to a handler over apps and returns what
	// was synced and what was logged.
	handle := func(t *testing.T, apps map[string][]string, slug string) (synced []string, logged string) {
		t.Helper()
		var buf bytes.Buffer
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(os.Stderr) })

		syncer := &fakeSyncer{apps: apps}
		makeEventHandler(syncer, config{})(context.Background(), api.EventLogEntry{
			EventType: eventTypeReleasePublished,
			Payload:   payload(slug),
		})
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
		makeEventHandler(syncer, config{})(context.Background(), api.EventLogEntry{
			EventType: eventTypeReleasePublished,
			Payload:   payload("s"),
		})
		if want := []string{"a", "b"}; !slices.Equal(syncer.synced, want) {
			t.Fatalf("synced = %v, want %v", syncer.synced, want)
		}
	})

	t.Run("unresolvable event is skipped", func(t *testing.T) {
		syncer := &fakeSyncer{}
		handler := makeEventHandler(syncer, config{})
		handler(context.Background(), api.EventLogEntry{EventType: eventTypeApplyCompleted})
		if len(syncer.synced) != 0 {
			t.Fatalf("synced = %v, want none", syncer.synced)
		}
	})

	t.Run("ArgoApp override is synced", func(t *testing.T) {
		syncer := &fakeSyncer{}
		handler := makeEventHandler(syncer, config{ArgoApp: "pinned-app"})
		handler(context.Background(), api.EventLogEntry{
			EventType: eventTypeApplyCompleted,
			Payload:   payload("ignored-slug"),
		})
		if got := syncer.synced; len(got) != 1 || got[0] != "pinned-app" {
			t.Fatalf("synced = %v, want [pinned-app]", got)
		}
	})
}
