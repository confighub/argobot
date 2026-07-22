// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/confighub/sdk/core/worker/api"
)

// Event types argobot reacts to. ConfigHub's event vocabulary is open-ended and
// owned by the server, so — like the API spec and the SDK — argobot names the
// ones it cares about itself rather than relying on shared constants.
const (
	eventTypeApplyCompleted   = "apply.completed"
	eventTypeReleasePublished = "release.published"
)

// subscriptionsForTargets builds the event-log subscriptions argobot consumes:
// apply and release facts, scoped to the Targets argobot reacts to. Each
// subscription's Name is the cursor name that keys argobot's server-stored
// delivery cursor, so a restart resumes where it left off.
//
// Scoping precedence:
//   - EventTargetID set — an explicit override: a single subscription scoped to
//     that one Target, and discoveredTargetIDs is ignored. Its Name is the plain
//     SubscriptionName, preserving the cursor of a deployment that pinned a
//     Target before auto-discovery existed.
//   - discoveredTargetIDs non-empty — one subscription per Target the worker is
//     the BridgeWorker for, so argobot reacts to exactly its own targets. Each
//     Name is suffixed with the Target ID to give it an independent cursor.
//   - neither — a single unscoped subscription (every Target). This is the
//     fallback when a worker owns no Targets; argobot stays useful rather than
//     going silent, at the cost of reacting org-wide.
//
// EventSpaceID, when set, further narrows every subscription (AND semantics).
func subscriptionsForTargets(cfg config, discoveredTargetIDs []string) []api.EventSubscription {
	eventTypes := []string{eventTypeApplyCompleted, eventTypeReleasePublished}
	sub := func(name, targetID string) api.EventSubscription {
		return api.EventSubscription{
			Name:       name,
			EventTypes: eventTypes,
			SpaceID:    cfg.EventSpaceID,
			TargetID:   targetID,
		}
	}

	switch {
	case cfg.EventTargetID != "":
		return []api.EventSubscription{sub(cfg.SubscriptionName, cfg.EventTargetID)}
	case len(discoveredTargetIDs) == 0:
		return []api.EventSubscription{sub(cfg.SubscriptionName, "")}
	default:
		subs := make([]api.EventSubscription, 0, len(discoveredTargetIDs))
		for _, id := range discoveredTargetIDs {
			subs = append(subs, sub(cfg.SubscriptionName+"-"+id, id))
		}
		return subs
	}
}

// makeEventHandler returns the callback ConfigHub invokes for each delivered
// fact. argobot reacts by syncing the corresponding Argo CD Application — a
// reaction, not a command it was told to run. A fact it cannot map to an
// Application is logged and skipped.
func makeEventHandler(syncer Syncer, cfg config) func(context.Context, api.EventLogEntry) {
	return func(ctx context.Context, entry api.EventLogEntry) {
		appName := resolveAppName(cfg, entry)
		if appName == "" {
			log.Printf("[WARN] argobot: %s (space=%s target=%s cursor=%d) — no Argo app resolved; skipping.",
				entry.EventType, entry.SpaceID, entry.TargetID, entry.CursorID)
			return
		}

		log.Printf("[INFO] argobot: %s (space=%s target=%s cursor=%d) → syncing Argo app %q",
			entry.EventType, entry.SpaceID, entry.TargetID, entry.CursorID, appName)

		if err := syncer.Sync(ctx, appName); err != nil {
			log.Printf("[ERROR] argobot: sync of %q failed: %v", appName, err)
			return
		}
		log.Printf("[INFO] argobot: sync of %q triggered", appName)
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
