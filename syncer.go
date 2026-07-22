// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"

	"github.com/confighub/argobot/argo"
	"github.com/confighub/argobot/kube"
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

// Syncer triggers a sync of a single Argo CD Application. Both the Kubernetes
// and the Argo CD REST backends implement it; the event handler is agnostic to
// which is in use. Per-backend tunables are bound at construction, so only the
// Application name varies per event.
type Syncer interface {
	Sync(ctx context.Context, appName string) error
}

// newSyncer builds the Syncer for the configured mode.
func newSyncer(cfg config) (Syncer, error) {
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
