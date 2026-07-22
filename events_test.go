// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/confighub/sdk/core/worker/api"
)

func TestResolveAppName(t *testing.T) {
	payload := func(v map[string]any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	tests := []struct {
		name  string
		cfg   config
		entry api.EventLogEntry
		want  string
	}{
		{
			name:  "release payload resolves via SpaceSlug",
			entry: api.EventLogEntry{EventType: eventTypeReleasePublished, Payload: payload(map[string]any{"BundleBaseName": "e9786ad6-uuid", "SpaceSlug": "nonprod-argobot", "ReleaseNum": 5})},
			want:  "nonprod-argobot",
		},
		{
			name:  "apply payload resolves via SpaceSlug",
			entry: api.EventLogEntry{EventType: eventTypeApplyCompleted, Payload: payload(map[string]any{"SpaceSlug": "nonprod-argocd"})},
			want:  "nonprod-argocd",
		},
		{
			name:  "ArgoApp override wins over payload",
			cfg:   config{ArgoApp: "pinned-app"},
			entry: api.EventLogEntry{EventType: eventTypeReleasePublished, Payload: payload(map[string]any{"SpaceSlug": "nonprod-argobot"})},
			want:  "pinned-app",
		},
		{
			name:  "BundleBaseName alone is not used as the app name",
			entry: api.EventLogEntry{EventType: eventTypeReleasePublished, Payload: payload(map[string]any{"BundleBaseName": "e9786ad6-uuid"})},
			want:  "",
		},
		{
			name:  "nil payload resolves to empty",
			entry: api.EventLogEntry{EventType: eventTypeApplyCompleted},
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveAppName(tt.cfg, tt.entry); got != tt.want {
				t.Fatalf("resolveAppName() = %q, want %q", got, tt.want)
			}
		})
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

// fakeSyncer records the app names it was asked to sync.
type fakeSyncer struct {
	synced []string
	err    error
}

func (f *fakeSyncer) Sync(_ context.Context, appName string) error {
	f.synced = append(f.synced, appName)
	return f.err
}

func TestMakeEventHandler(t *testing.T) {
	payload := func(slug string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"SpaceSlug": slug})
		return b
	}

	t.Run("resolved app is synced", func(t *testing.T) {
		syncer := &fakeSyncer{}
		handler := makeEventHandler(syncer, config{})
		handler(context.Background(), api.EventLogEntry{
			EventType: eventTypeReleasePublished,
			Payload:   payload("nonprod-argobot"),
		})
		if got := syncer.synced; len(got) != 1 || got[0] != "nonprod-argobot" {
			t.Fatalf("synced = %v, want [nonprod-argobot]", got)
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
