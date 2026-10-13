// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"log"
	"slices"
	"sort"
	"strconv"

	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
)

// Event types argobot reacts to. ConfigHub's event vocabulary is open-ended and
// owned by the server, so — like the API spec and the SDK — argobot names the
// ones it cares about itself rather than relying on shared constants.
const eventTypeReleasePublished = "release.published"

// subscriptions is what argobot's Channel should subscribe to: the release
// facts, for every Space and Target. It is the default an operator
// creates the Channel with (`argobot --print-subscriptions`), and narrows on
// the Channel by adding a SpaceID or TargetID, or more entries, with cub.
func subscriptions() []goclientnew.EventSubscription {
	return []goclientnew.EventSubscription{
		{EventTypes: []string{eventTypeReleasePublished}},
	}
}

// makeEventHandler returns the callback the Channel listener invokes for each
// delivered event. argobot reacts by syncing the corresponding Argo CD
// Applications — a reaction, not a command it was told to run. An event of a
// type argobot does not react to, which a Channel subscribed more widely than
// subscriptions() would deliver, is logged and skipped, as is a fact it cannot
// map to an Application.
func makeEventHandler(syncer Syncer, cfg config) func(context.Context, *goclientnew.EventLog) {
	return func(ctx context.Context, event *goclientnew.EventLog) {
		where := eventWhere(event)
		switch event.EventType {
		case eventTypeReleasePublished:
		default:
			log.Printf("[WARN] argobot: %s (%s) — not an event argobot reacts to; skipping. Narrow the channel's subscriptions.",
				event.EventType, where)
			return
		}

		appNames := resolveAppNames(ctx, syncer, cfg, event)
		if len(appNames) == 0 {
			if slug := eventSpaceSlug(event); slug != "" {
				log.Printf("[WARN] argobot: %s (%s) — no Argo app found for space %q; skipping.",
					event.EventType, where, slug)
				return
			}
			log.Printf("[WARN] argobot: %s (%s) — no Argo app resolved; skipping.", event.EventType, where)
			return
		}

		for _, appName := range appNames {
			log.Printf("[INFO] argobot: %s (%s) → syncing Argo app %q", event.EventType, where, appName)

			if err := syncer.Sync(ctx, appName); err != nil {
				log.Printf("[ERROR] argobot: sync of %q failed: %v", appName, err)
				continue
			}
			log.Printf("[INFO] argobot: sync of %q triggered", appName)
		}
	}
}

// eventWhere describes an event's position and scope for the log.
func eventWhere(event *goclientnew.EventLog) string {
	id := func(u *goclientnew.UUID) string {
		if u == nil {
			return ""
		}
		return u.String()
	}
	return "space=" + id(event.SpaceID) + " target=" + id(event.TargetID) + " cursor=" + strconv.FormatInt(event.CursorID, 10)
}

// resolveAppNames maps a delivered event to the Argo CD Applications to sync.
// ArgoApp, when set, is the single Application every event targets. Otherwise the
// event payload's SpaceSlug — the slug of the Space the fact is about — is matched
// to every Application whose OCI source names that Space (.../space/<slug>), plus
// the Application named after the slug, by the app-name == space-slug convention.
// The source match finds Applications that kept the name Argo CD gave them when
// they were moved onto ConfigHub. Each Application appears once, in name order.
// release.published carries SpaceSlug. (The payload's
// BundleBaseName is the Release bundle's filename, which defaults to the Space ID,
// so it must not be used as the app name.) An event that resolves to nothing is
// left unhandled.
func resolveAppNames(ctx context.Context, syncer Syncer, cfg config, event *goclientnew.EventLog) []string {
	if cfg.ArgoApp != "" {
		return []string{cfg.ArgoApp}
	}
	slug := eventSpaceSlug(event)
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
// the payload is absent or is not an object with one. The generated client
// decodes the payload as a generic JSON value, so it is read as a map.
func eventSpaceSlug(event *goclientnew.EventLog) string {
	payload, ok := event.Payload.(map[string]any)
	if !ok {
		return ""
	}
	slug, _ := payload["SpaceSlug"].(string)
	return slug
}
