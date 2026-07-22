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

	consumer := worker.NewEventConsumer(
		cfg.ConfigHubURL,
		cfg.WorkerID,
		cfg.WorkerSecret,
		eventSubscription(cfg),
		makeEventHandler(syncer, cfg),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("[INFO] argobot starting; consuming events from %s (subscription %q, space=%q target=%q)",
		cfg.ConfigHubURL, cfg.SubscriptionName, cfg.EventSpaceID, cfg.EventTargetID)
	if err := consumer.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("[FATAL] event consumer stopped: %v", err)
	}
	log.Printf("[INFO] argobot shutting down")
}
