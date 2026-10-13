// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

// Command argobot is a ConfigHub bot that force-syncs Argo CD applications.
//
// It listens on a ConfigHub Channel: it claims the Channel as the service
// account whose key it holds and polls it, force-syncing the corresponding Argo CD Application
// whenever ConfigHub delivers an apply or a release the Channel subscribes to.
// This makes a deploy feel immediate instead of waiting for Argo's next
// reconciliation.
//
// argobot handles no commands yet, so a Channel it listens on declares none.
// Its reaction (force-sync) is its own; the event only says the desired state
// for a (space, target) changed.
//
// With --print-subscriptions it prints the Subscriptions its Channel should
// have, for `cub channel create ... --subscriptions -`.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"syscall"

	"github.com/confighub/sdk/core/channel"
	"github.com/confighub/sdk/core/cubapi"
	"golang.org/x/sync/errgroup"
)

func main() {
	printSubscriptions := flag.Bool("print-subscriptions", false, "print the Channel subscriptions argobot expects and exit")
	flag.Parse()
	if *printSubscriptions {
		encoded, _ := json.MarshalIndent(subscriptions(), "", "  ")
		fmt.Println(string(encoded))
		return
	}

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

	hub, err := newHubClient(ctx, cfg)
	if err != nil {
		log.Fatalf("[FATAL] %v", err)
	}

	// Concurrency one: argobot runs no commands, and events are handled in
	// delivery order on the poll goroutine regardless.
	listener := channel.NewListener(hub, cubapi.ParseRef(cfg.Channel), channel.Options{Concurrency: 1}).
		OnEvent(makeEventHandler(syncer, cfg))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		log.Printf("[INFO] argobot starting; listening on channel %s at %s", cfg.Channel, cfg.ConfigHubURL)
		return listener.Run(gctx)
	})

	// Alongside the channel, optionally watch Argo CD Applications and report
	// their live status back to ConfigHub. It needs Kubernetes access; if the
	// client cannot be built (e.g. argocd sync mode with no cluster access), skip
	// it with a warning rather than failing the bot.
	if cfg.ReportLiveStatus {
		if rep, err := newReporter(cfg, hub); err != nil {
			log.Printf("[WARN] live-status reporting disabled: %v", err)
		} else {
			log.Printf("[INFO] argobot: live-status reporting enabled")
			g.Go(func() error { return rep.Run(gctx) })
		}
	}

	if err := g.Wait(); err != nil && ctx.Err() == nil {
		log.Fatalf("[FATAL] %v", err)
	}
	log.Printf("[INFO] argobot shutting down")
}

// newHubClient builds the one ConfigHub client argobot uses, as the service
// account whose key it holds. Each session is bought with a short-lived
// assertion signed by that key, so nothing replayable crosses the wire. The
// client authenticates once here, so a bad key fails the start, and renews its
// session on a 401 for as long as the process runs.
func newHubClient(ctx context.Context, cfg config) (*cubapi.Client, error) {
	signer, err := cubapi.NewAssertionSigner([]byte(cfg.PrivateKey), "", cfg.AssertionAudience)
	if err != nil {
		return nil, fmt.Errorf("CONFIGHUB_AUTH_PRIVATE_KEY: %w", err)
	}
	authenticate := cubapi.AssertionAuthenticator(cfg.ConfigHubURL, signer)
	session, err := authenticate(ctx)
	if err != nil {
		return nil, fmt.Errorf("authenticating to %s with key %s: %w", cfg.ConfigHubURL, signer.Kid(), err)
	}
	return cubapi.NewClient(cubapi.ClientOptions{
		ServerURL:     cfg.ConfigHubURL,
		Session:       session,
		Authenticator: authenticate,
		UserAgent:     "argobot",
	})
}
