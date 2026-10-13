// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"

	"github.com/confighub/argobot/argo"
	"github.com/confighub/argobot/kube"
	"github.com/confighub/argobot/sveltos"
)

// Controllers selectable via GITOPS_CONTROLLER.
const (
	// ControllerArgoCD drives Argo CD Applications, by ARGO_SYNC_MODE. This is
	// the default.
	ControllerArgoCD = "argocd"
	// ControllerSveltos drives Sveltos ClusterProfiles and Profiles: it forces a
	// redeploy by patching their profile.projectsveltos.io/redeploy annotation,
	// and reports live status from Sveltos HealthCheckReports.
	ControllerSveltos = "sveltos"
)

// Sync modes selectable via ARGO_SYNC_MODE.
const (
	// SyncModeKubernetes patches the Application's argocd.argoproj.io/refresh
	// annotation via the Kubernetes API. It deploys only when the Application
	// has auto-sync enabled. This is the default.
	SyncModeKubernetes = "kubernetes"
	// SyncModeArgoCD calls the Argo CD REST /sync endpoint, which deploys
	// regardless of the Application's auto-sync setting.
	SyncModeArgoCD = "argocd"
)

// Syncer triggers a sync of a single Argo CD Application or Sveltos profile. The
// Kubernetes and Argo CD REST backends implement it for Argo CD, and the sveltos
// backend for Sveltos; the event handler is agnostic to which is in use.
// Per-backend tunables are bound at construction, so only the Application (or
// profile) name varies per event.
type Syncer interface {
	Sync(ctx context.Context, appName string) error
	// RepoURLs lists the Applications the backend can see, mapping each name to
	// the repoURLs of its source(s). It is how an event's Space is matched to the
	// Applications that deploy from it.
	RepoURLs(ctx context.Context) (map[string][]string, error)
}

// newSyncer builds the Syncer for the configured controller and mode.
func newSyncer(cfg config) (Syncer, error) {
	if cfg.Controller == ControllerSveltos {
		return sveltos.NewSyncer()
	}
	switch cfg.ArgoSyncMode {
	case SyncModeArgoCD:
		return argo.NewClient(argo.ClientConfig{
			Server:       cfg.ArgoServer,
			Token:        cfg.ArgoToken,
			Insecure:     cfg.ArgoInsecure,
			AppNamespace: cfg.ArgoAppNamespace,
			Prune:        cfg.ArgoPrune,
			Force:        cfg.ArgoForce,
		}), nil
	case SyncModeKubernetes:
		return kube.NewSyncer(kube.Config{
			Namespace:   cfg.ArgoNamespace,
			RefreshType: cfg.ArgoRefreshType,
		})
	default:
		// loadConfig validates the mode, so this is unreachable in practice.
		return nil, fmt.Errorf("unknown sync mode %q", cfg.ArgoSyncMode)
	}
}
