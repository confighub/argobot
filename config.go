// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// config holds argobot's runtime configuration, all sourced from the environment.
//
// The ConfigHub key identifies argobot to ConfigHub; the Channel is the mailbox
// it listens on. The Argo CD credentials are argobot's own authority against
// Argo CD — held out of band, never brokered through ConfigHub.
//
// argobot reacts to the ConfigHub events its Channel subscribes to: release
// facts, scoped on the Channel to whatever Spaces or Targets the
// operator chose. It force-syncs the corresponding Argo CD Application or
// Sveltos profile, which comes from ArgoApp when set (Argo CD only), otherwise
// from the event itself (its Space slug names every Application or profile
// whose OCI source is that Space, and the one named after it).
type config struct {
	ConfigHubURL string

	// PrivateKey is the service account key argobot authenticates with, as a
	// private JWK. It signs a short-lived assertion per session, so ConfigHub
	// holds only the public half. AssertionAudience is needed only on a server
	// that sets CONFIGHUB_ASSERTION_AUDIENCE to something other than its default.
	PrivateKey        string
	AssertionAudience string

	// Channel names the ConfigHub Channel argobot claims and polls, as
	// <space>/<slug> or a UUID. What it delivers — which event types, for which
	// Spaces and Targets — is the Channel's Subscriptions, set with cub, not here.
	Channel string

	// Controller selects the GitOps controller argobot drives: "argocd"
	// (default) or "sveltos". The Argo* settings below apply to Argo CD only.
	Controller string

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

	// Force-sync behaviour. ArgoApp, when set, is the single Application every
	// matching event force-syncs (the simplest deployment). AppNamespace, Prune,
	// and Force are passed through to the sync request.
	ArgoApp          string
	ArgoAppNamespace string
	ArgoPrune        bool
	ArgoForce        bool

	// ReportLiveStatus enables the reporter that watches Argo CD Applications and
	// writes their status back to the Release each one is running. On by default;
	// set CONFIGHUB_REPORT_LIVE_STATUS=false to disable. It needs Kubernetes
	// access (in-cluster or a kubeconfig); when it cannot reach the cluster it is
	// skipped with a warning rather than failing the bot.
	ReportLiveStatus bool
}

// retiredEnv names the variables argobot read when it connected as a worker, each
// with why it no longer does. One that is still set is ignored with a warning
// rather than silently: a deployment that sets it expects it to do something.
var retiredEnv = []struct{ name, reason string }{
	{"CONFIGHUB_WORKER_ID", "argobot authenticates with the service account key in CONFIGHUB_AUTH_PRIVATE_KEY"},
	{"CONFIGHUB_WORKER_SECRET", "argobot authenticates with the service account key in CONFIGHUB_AUTH_PRIVATE_KEY"},
	{"CONFIGHUB_SUBSCRIPTION_NAME", "the events argobot receives are the Subscriptions of its channel"},
	{"CONFIGHUB_EVENT_SPACE_ID", "the events argobot receives are the Subscriptions of its channel"},
	{"CONFIGHUB_EVENT_TARGET_ID", "the events argobot receives are the Subscriptions of its channel"},
}

func loadConfig() (config, error) {
	cfg := config{
		ConfigHubURL:      os.Getenv("CONFIGHUB_URL"),
		PrivateKey:        os.Getenv("CONFIGHUB_AUTH_PRIVATE_KEY"),
		AssertionAudience: os.Getenv("CONFIGHUB_ASSERTION_AUDIENCE"),
		Channel:           os.Getenv("CONFIGHUB_CHANNEL"),
		Controller:        os.Getenv("GITOPS_CONTROLLER"),
		ArgoSyncMode:      os.Getenv("ARGO_SYNC_MODE"),
		ArgoServer:        strings.TrimRight(os.Getenv("ARGOCD_SERVER"), "/"),
		ArgoToken:         os.Getenv("ARGOCD_AUTH_TOKEN"),
		ArgoInsecure:      os.Getenv("ARGOCD_INSECURE") == "true",
		ArgoNamespace:     os.Getenv("ARGO_NAMESPACE"),
		ArgoRefreshType:   os.Getenv("ARGO_REFRESH_TYPE"),
		ArgoApp:           os.Getenv("ARGO_APP"),
		ArgoAppNamespace:  os.Getenv("ARGO_APP_NAMESPACE"),
		ArgoPrune:         os.Getenv("ARGO_PRUNE") == "true",
		ArgoForce:         os.Getenv("ARGO_FORCE") == "true",
		ReportLiveStatus:  os.Getenv("CONFIGHUB_REPORT_LIVE_STATUS") != "false",
	}
	if cfg.Controller == "" {
		cfg.Controller = ControllerArgoCD
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
		"CONFIGHUB_URL":              cfg.ConfigHubURL,
		"CONFIGHUB_AUTH_PRIVATE_KEY": cfg.PrivateKey,
		"CONFIGHUB_CHANNEL":          cfg.Channel,
	}
	switch cfg.Controller {
	case ControllerArgoCD:
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
	case ControllerSveltos:
		// No extra required vars: in-cluster credentials come from the
		// ServiceAccount. The ARGO_*/ARGOCD_* settings are Argo-only; clear them so
		// none of them, ARGO_APP above all, changes what a Sveltos deployment syncs.
		cfg.ArgoSyncMode, cfg.ArgoApp, cfg.ArgoAppNamespace = "", "", ""
		cfg.ArgoServer, cfg.ArgoToken, cfg.ArgoNamespace, cfg.ArgoRefreshType = "", "", "", ""
		cfg.ArgoInsecure, cfg.ArgoPrune, cfg.ArgoForce = false, false, false
	default:
		return config{}, fmt.Errorf("invalid GITOPS_CONTROLLER %q: must be %q or %q",
			cfg.Controller, ControllerArgoCD, ControllerSveltos)
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

	for _, retired := range retiredEnv {
		if os.Getenv(retired.name) != "" {
			log.Printf("[WARN] %s is ignored: %s", retired.name, retired.reason)
		}
	}
	return cfg, nil
}
