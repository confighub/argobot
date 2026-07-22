// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

// Command argobot is a ConfigHub bot that force-syncs Argo CD applications.
//
// It is an event-log consumer: it authenticates to ConfigHub with a worker
// identity and long-polls the event log, force-syncing the corresponding Argo CD
// Application whenever ConfigHub records an apply or a release. This makes a
// deploy feel immediate instead of waiting for Argo's next reconciliation.
//
// argobot is a pure consumer — it holds no lease and dequeues no operations, so
// it never contends with any Target's work. Its reaction (force-sync) is its
// own; the event only says the desired state for a (space, target) changed.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/confighub/sdk/core/worker"
	"golang.org/x/sync/errgroup"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("[FATAL] %v", err)
	}

	syncer, err := newSyncer(cfg)
	if err != nil {
		log.Fatalf("[FATAL] %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Scope delivery to the worker's own Targets unless CONFIGHUB_EVENT_TARGET_ID
	// overrides. Discovery uses the same worker credentials as the event consumer.
	var discovered []string
	if cfg.EventTargetID == "" {
		discovered, err = discoverWorkerTargets(ctx, cfg)
		if err != nil {
			log.Fatalf("[FATAL] discovering worker targets: %v", err)
		}
		if len(discovered) == 0 {
			log.Printf("[WARN] argobot: worker is the bridge for no Targets; subscribing to every Target. " +
				"Set CONFIGHUB_EVENT_TARGET_ID to scope explicitly.")
		}
	}
	subs := subscriptionsForTargets(cfg, discovered)

	handler := makeEventHandler(syncer, cfg)
	log.Printf("[INFO] argobot starting; consuming events from %s (subscription %q, space=%q, %d subscription(s))",
		cfg.ConfigHubURL, cfg.SubscriptionName, cfg.EventSpaceID, len(subs))
	for _, sub := range subs {
		log.Printf("[INFO] argobot: subscription %q scoped to target=%q", sub.Name, sub.TargetID)
	}

	// One consumer per subscription; each holds its own long-poll connection and
	// cursor. If any stops with an error, cancel the rest and exit.
	g, gctx := errgroup.WithContext(ctx)
	for _, sub := range subs {
		consumer := worker.NewEventConsumer(cfg.ConfigHubURL, cfg.WorkerID, cfg.WorkerSecret, sub, handler)
		g.Go(func() error { return consumer.Run(gctx) })
	}
	if err := g.Wait(); err != nil && ctx.Err() == nil {
		log.Fatalf("[FATAL] event consumer stopped: %v", err)
	}
	log.Printf("[INFO] argobot shutting down")
}
