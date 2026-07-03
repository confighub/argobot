// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

// Command argobot is a ConfigHub bot that force-syncs Argo CD applications.
//
// It connects to ConfigHub over the HTTP long-poll worker protocol, registers an
// "Argo" provider bridge, and force-syncs the corresponding Argo CD Application
// whenever a Unit bound to an Argo target is applied. This makes an apply feel
// like an immediate imperative deploy instead of waiting for Argo's next
// reconciliation.
package main

import (
	"log"

	"github.com/confighub/argobot/argo"
	"github.com/confighub/sdk/core/worker"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("[FATAL] %v", err)
	}

	argoClient := argo.NewClient(argo.ClientConfig{
		Server:   cfg.ArgoServer,
		Token:    cfg.ArgoToken,
		Insecure: cfg.ArgoInsecure,
	})

	dispatcher := worker.NewBridgeDispatcher()
	dispatcher.RegisterBridge(NewArgoBridge(argoClient))

	connector, err := worker.NewConnector(worker.ConnectorOptions{
		WorkerID:         cfg.WorkerID,
		WorkerSecret:     cfg.WorkerSecret,
		ConfigHubURL:     cfg.ConfigHubURL,
		BridgeDispatcher: &dispatcher,
		// Hardcoded to HTTP long-polling: argobot talks to the main API port and
		// does not need the separate h2c worker port. Later, when ConfigHub grows
		// an event-subscription channel, argobot will react to apply events over
		// this same transport instead of being invoked as a bridge Apply.
		Transport: worker.TransportLongPoll,
	})
	if err != nil {
		log.Fatalf("[FATAL] failed to create connector: %v", err)
	}

	log.Printf("[INFO] argobot starting; connecting to %s via long-poll", cfg.ConfigHubURL)
	if err := connector.Start(); err != nil {
		log.Fatalf("[FATAL] connector stopped: %v", err)
	}
}
