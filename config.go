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

	// ArgoSyncMode selects how argobot triggers a sync: "kubernetes" (default)
	// patches the Application's refresh annotation via the Kubernetes API;
	// "argocd" calls the Argo CD REST /sync endpoint.
	ArgoSyncMode string

	ArgoServer   string
	ArgoToken    string
	ArgoInsecure bool

	// Kubernetes-mode settings. ArgoNamespace is where the Application resources
	// live (defaults to "argocd"). ArgoRefreshType is "hard" or "normal".
	ArgoNamespace   string
	ArgoRefreshType string

	// Event subscription scope. SubscriptionName keys argobot's server-stored
	// delivery cursor and must be stable across restarts. SpaceID is an optional
	// filter; empty means every Space. TargetID is an override: when empty,
	// argobot discovers the Targets its worker is the bridge for and subscribes
	// to those; when set, it scopes to that single Target instead.
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

	// ReportLiveStatus enables the reporter that watches Argo CD Applications and
	// writes their status back to the deployment Space as the
	// confighub.com/live-status annotation. On by default; set
	// CONFIGHUB_REPORT_LIVE_STATUS=false to disable. It needs Kubernetes access
	// (in-cluster or a kubeconfig); when it cannot reach the cluster it is skipped
	// with a warning rather than failing the bot.
	ReportLiveStatus bool
}

func loadConfig() (config, error) {
	cfg := config{
		ConfigHubURL:     os.Getenv("CONFIGHUB_URL"),
		WorkerID:         os.Getenv("CONFIGHUB_WORKER_ID"),
		WorkerSecret:     os.Getenv("CONFIGHUB_WORKER_SECRET"),
		ArgoSyncMode:     os.Getenv("ARGO_SYNC_MODE"),
		ArgoServer:       strings.TrimRight(os.Getenv("ARGOCD_SERVER"), "/"),
		ArgoToken:        os.Getenv("ARGOCD_AUTH_TOKEN"),
		ArgoInsecure:     os.Getenv("ARGOCD_INSECURE") == "true",
		ArgoNamespace:    os.Getenv("ARGO_NAMESPACE"),
		ArgoRefreshType:  os.Getenv("ARGO_REFRESH_TYPE"),
		SubscriptionName: os.Getenv("CONFIGHUB_SUBSCRIPTION_NAME"),
		EventSpaceID:     os.Getenv("CONFIGHUB_EVENT_SPACE_ID"),
		EventTargetID:    os.Getenv("CONFIGHUB_EVENT_TARGET_ID"),
		ArgoApp:          os.Getenv("ARGO_APP"),
		ArgoAppNamespace: os.Getenv("ARGO_APP_NAMESPACE"),
		ArgoPrune:        os.Getenv("ARGO_PRUNE") == "true",
		ArgoForce:        os.Getenv("ARGO_FORCE") == "true",
		ReportLiveStatus: os.Getenv("CONFIGHUB_REPORT_LIVE_STATUS") != "false",
	}
	if cfg.SubscriptionName == "" {
		cfg.SubscriptionName = "argobot"
	}
	if cfg.ArgoSyncMode == "" {
		cfg.ArgoSyncMode = SyncModeKubernetes
	}
	if cfg.ArgoNamespace == "" {
		cfg.ArgoNamespace = "argocd"
	}
	if cfg.ArgoRefreshType == "" {
		cfg.ArgoRefreshType = "hard"
	}

	required := map[string]string{
		"CONFIGHUB_URL":           cfg.ConfigHubURL,
		"CONFIGHUB_WORKER_ID":     cfg.WorkerID,
		"CONFIGHUB_WORKER_SECRET": cfg.WorkerSecret,
	}
	switch cfg.ArgoSyncMode {
	case SyncModeKubernetes:
		// No extra required vars: in-cluster credentials come from the
		// ServiceAccount, and ARGO_NAMESPACE has a default.
	case SyncModeArgoCD:
		required["ARGOCD_SERVER"] = cfg.ArgoServer
		required["ARGOCD_AUTH_TOKEN"] = cfg.ArgoToken
	default:
		return config{}, fmt.Errorf("invalid ARGO_SYNC_MODE %q: must be %q or %q",
			cfg.ArgoSyncMode, SyncModeKubernetes, SyncModeArgoCD)
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
