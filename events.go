// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"log"
	"slices"
	"sort"

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
//     granted ViewChildren on, so argobot reacts to exactly its own targets. Each
//     Name is suffixed with the Target ID to give it an independent cursor.
//   - neither — a single unscoped subscription (every Target). This is the
//     fallback when a worker is granted on no Targets; argobot stays useful
//     rather than going silent, at the cost of reacting org-wide.
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
// fact. argobot reacts by syncing the corresponding Argo CD Applications — a
// reaction, not a command it was told to run. A fact it cannot map to an
// Application is logged and skipped.
func makeEventHandler(syncer Syncer, cfg config) func(context.Context, api.EventLogEntry) {
	return func(ctx context.Context, entry api.EventLogEntry) {
		appNames := resolveAppNames(ctx, syncer, cfg, entry)
		if len(appNames) == 0 {
			if slug := eventSpaceSlug(entry); slug != "" {
				log.Printf("[WARN] argobot: %s (space=%s target=%s cursor=%d) — no Argo app found for space %q; skipping.",
					entry.EventType, entry.SpaceID, entry.TargetID, entry.CursorID, slug)
				return
			}
			log.Printf("[WARN] argobot: %s (space=%s target=%s cursor=%d) — no Argo app resolved; skipping.",
				entry.EventType, entry.SpaceID, entry.TargetID, entry.CursorID)
			return
		}

		for _, appName := range appNames {
			log.Printf("[INFO] argobot: %s (space=%s target=%s cursor=%d) → syncing Argo app %q",
				entry.EventType, entry.SpaceID, entry.TargetID, entry.CursorID, appName)

			if err := syncer.Sync(ctx, appName); err != nil {
				log.Printf("[ERROR] argobot: sync of %q failed: %v", appName, err)
				continue
			}
			log.Printf("[INFO] argobot: sync of %q triggered", appName)
		}
	}
}

// resolveAppNames maps a delivered event to the Argo CD Applications to sync.
// ArgoApp, when set, is the single Application every event targets. Otherwise the
// event payload's SpaceSlug — the slug of the Space the fact is about — is matched
// to every Application whose OCI source names that Space (.../space/<slug>), plus
// the Application named after the slug, by the app-name == space-slug convention.
// The source match finds Applications that kept the name Argo CD gave them when
// they were moved onto ConfigHub. Each Application appears once, in name order.
// Both apply.completed and release.published carry SpaceSlug. (The payload's
// BundleBaseName is the Release bundle's filename, which defaults to the Space ID,
// so it must not be used as the app name.) An event that resolves to nothing is
// left unhandled.
func resolveAppNames(ctx context.Context, syncer Syncer, cfg config, entry api.EventLogEntry) []string {
	if cfg.ArgoApp != "" {
		return []string{cfg.ArgoApp}
	}
	slug := eventSpaceSlug(entry)
	if slug == "" {
		return nil
	}
	apps, err := syncer.RepoURLs(ctx)
	if err != nil {
		// Without the list, fall back to the convention rather than drop the event.
		log.Printf("[WARN] argobot: listing Argo apps for space %q failed: %v; trying the app named %q", slug, err, slug)
		return []string{slug}
	}
	return appNamesForSpace(apps, slug)
}

// appNamesForSpace returns, in name order, the Applications in apps (name ->
// source repoURLs) that are named slug or whose source repoURL names the Space slug.
func appNamesForSpace(apps map[string][]string, slug string) []string {
	var names []string
	for name, repoURLs := range apps {
		if name == slug || slices.ContainsFunc(repoURLs, func(u string) bool { return spaceSlugFromRepoURL(u) == slug }) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// eventSpaceSlug returns the SpaceSlug carried by the event payload, or "" when
// the payload is absent or does not decode.
func eventSpaceSlug(entry api.EventLogEntry) string {
	var payload struct {
		SpaceSlug string
	}
	if err := json.Unmarshal(entry.Payload, &payload); err == nil {
		return payload.SpaceSlug
	}
	return ""
}
