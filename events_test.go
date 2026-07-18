// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
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
