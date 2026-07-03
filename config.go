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
// through ConfigHub. Which Application to sync comes from the applied Unit/Target,
// not from here.
type config struct {
	ConfigHubURL string
	WorkerID     string
	WorkerSecret string

	ArgoServer   string
	ArgoToken    string
	ArgoInsecure bool
}

func loadConfig() (config, error) {
	cfg := config{
		ConfigHubURL: os.Getenv("CONFIGHUB_URL"),
		WorkerID:     os.Getenv("CONFIGHUB_WORKER_ID"),
		WorkerSecret: os.Getenv("CONFIGHUB_WORKER_SECRET"),
		ArgoServer:   strings.TrimRight(os.Getenv("ARGOCD_SERVER"), "/"),
		ArgoToken:    os.Getenv("ARGOCD_AUTH_TOKEN"),
		ArgoInsecure: os.Getenv("ARGOCD_INSECURE") == "true",
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
