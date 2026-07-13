// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"
	"strings"
)

// config holds argobot's runtime configuration, all sourced from the environment.
//
// The ConfigHub credentials identify argobot as a worker. The Argo CD credentials
// are argobot's own authority against Argo CD — held out of band, never brokered
// through ConfigHub.
//
// argobot reacts to ConfigHub event-log facts: it subscribes to apply and
// release events (optionally scoped to one Space or Target) and force-syncs the
// corresponding Argo CD Application. Which Application that is comes from ArgoApp
// when set, otherwise from the event itself (a release carries its Space slug,
// which by convention names the Application).
type config struct {
	ConfigHubURL string
	WorkerID     string
	WorkerSecret string

	ArgoServer   string
	ArgoToken    string
	ArgoInsecure bool

	// Event subscription scope. SubscriptionName keys argobot's server-stored
	// delivery cursor and must be stable across restarts. SpaceID and TargetID
	// are optional filters; empty means every Space / every Target.
	SubscriptionName string
	EventSpaceID     string
	EventTargetID    string

	// Force-sync behaviour. ArgoApp, when set, is the single Application every
	// matching event force-syncs (the simplest deployment). AppNamespace, Prune,
	// and Force are passed through to the sync request.
	ArgoApp          string
	ArgoAppNamespace string
	ArgoPrune        bool
	ArgoForce        bool
}

func loadConfig() (config, error) {
	cfg := config{
		ConfigHubURL:     os.Getenv("CONFIGHUB_URL"),
		WorkerID:         os.Getenv("CONFIGHUB_WORKER_ID"),
		WorkerSecret:     os.Getenv("CONFIGHUB_WORKER_SECRET"),
		ArgoServer:       strings.TrimRight(os.Getenv("ARGOCD_SERVER"), "/"),
		ArgoToken:        os.Getenv("ARGOCD_AUTH_TOKEN"),
		ArgoInsecure:     os.Getenv("ARGOCD_INSECURE") == "true",
		SubscriptionName: os.Getenv("CONFIGHUB_SUBSCRIPTION_NAME"),
		EventSpaceID:     os.Getenv("CONFIGHUB_EVENT_SPACE_ID"),
		EventTargetID:    os.Getenv("CONFIGHUB_EVENT_TARGET_ID"),
		ArgoApp:          os.Getenv("ARGO_APP"),
		ArgoAppNamespace: os.Getenv("ARGO_APP_NAMESPACE"),
		ArgoPrune:        os.Getenv("ARGO_PRUNE") == "true",
		ArgoForce:        os.Getenv("ARGO_FORCE") == "true",
	}
	if cfg.SubscriptionName == "" {
		cfg.SubscriptionName = "argobot"
	}

	required := map[string]string{
		"CONFIGHUB_URL":           cfg.ConfigHubURL,
		"CONFIGHUB_WORKER_ID":     cfg.WorkerID,
		"CONFIGHUB_WORKER_SECRET": cfg.WorkerSecret,
		"ARGOCD_SERVER":           cfg.ArgoServer,
		"ARGOCD_AUTH_TOKEN":       cfg.ArgoToken,
	}
	var missing []string
	for name, value := range required {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}
