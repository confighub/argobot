// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/confighub/argobot/argo"
	"github.com/confighub/sdk/core/worker/api"
)

// Event types argobot reacts to. ConfigHub's event vocabulary is open-ended and
// owned by the server, so — like the API spec and the SDK — argobot names the
// ones it cares about itself rather than relying on shared constants.
const (
	eventTypeApplyCompleted   = "apply.completed"
	eventTypeReleasePublished = "release.published"
)

// eventSubscription builds the event-log subscription argobot consumes: apply
// and release facts, optionally narrowed to one Space or Target. Its Name is the
// cursor name (in the request path), which keys argobot's server-stored delivery
// cursor, so a restart resumes where it left off.
func eventSubscription(cfg config) api.EventSubscription {
	return api.EventSubscription{
		Name: cfg.SubscriptionName,
		EventTypes: []string{
			eventTypeApplyCompleted,
			eventTypeReleasePublished,
		},
		SpaceID:  cfg.EventSpaceID,
		TargetID: cfg.EventTargetID,
	}
}

// makeEventHandler returns the callback ConfigHub invokes for each delivered
// fact. argobot reacts by force-syncing the corresponding Argo CD Application —
// a reaction, not a command it was told to run. A fact it cannot map to an
// Application is logged and skipped.
func makeEventHandler(argoClient *argo.Client, cfg config) func(context.Context, api.EventLogEntry) {
	return func(ctx context.Context, entry api.EventLogEntry) {
		appName := resolveAppName(cfg, entry)
		if appName == "" {
			log.Printf("[WARN] argobot: %s (space=%s target=%s cursor=%d) — no Argo app resolved; skipping.",
				entry.EventType, entry.SpaceID, entry.TargetID, entry.CursorID)
			return
		}

		log.Printf("[INFO] argobot: %s (space=%s target=%s cursor=%d) → force-syncing Argo app %q",
			entry.EventType, entry.SpaceID, entry.TargetID, entry.CursorID, appName)

		if err := argoClient.Sync(ctx, appName, argo.SyncOptions{
			AppNamespace: cfg.ArgoAppNamespace,
			Prune:        cfg.ArgoPrune,
			Force:        cfg.ArgoForce,
		}); err != nil {
			log.Printf("[ERROR] argobot: force-sync of %q failed: %v", appName, err)
			return
		}
		log.Printf("[INFO] argobot: force-sync of %q triggered", appName)
	}
}

// resolveAppName maps a delivered event to the Argo CD Application to sync.
// ArgoApp, when set, is the single Application every event targets. Otherwise the
// event payload's SpaceSlug — the slug of the Space the fact is about — names the
// Application, by the app-name == space-slug convention. Both apply.completed and
// release.published carry SpaceSlug. (The payload's BundleBaseName is the Release
// bundle's filename, which defaults to the Space ID, so it must not be used as
// the app name.) An event that resolves to neither is left unhandled.
func resolveAppName(cfg config, entry api.EventLogEntry) string {
	if cfg.ArgoApp != "" {
		return cfg.ArgoApp
	}
	var payload struct {
		SpaceSlug string
	}
	if err := json.Unmarshal(entry.Payload, &payload); err == nil {
		return payload.SpaceSlug
	}
	return ""
}
