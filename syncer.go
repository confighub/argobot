// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"

	"github.com/confighub/argobot/flux"
	"github.com/confighub/argobot/kube"
	"github.com/confighub/argobot/sveltos"
)

// Controllers selectable via GITOPS_CONTROLLER.
const (
	// ControllerArgoCD drives Argo CD Applications: it forces a sync by patching
	// their argocd.argoproj.io/refresh annotation, which deploys when the
	// Application has auto-sync enabled. This is the default.
	ControllerArgoCD = "argocd"
	// ControllerSveltos drives Sveltos ClusterProfiles and Profiles: it forces a
	// redeploy by patching their profile.projectsveltos.io/redeploy annotation,
	// and reports live status from Sveltos HealthCheckReports.
	ControllerSveltos = "sveltos"
	// ControllerFlux drives Flux OCIRepositories: it forces a reconciliation by
	// patching their reconcile.fluxcd.io/requestedAt annotation, and reports
	// live status from the Kustomizations that apply them.
	ControllerFlux = "flux"
)

// Syncer triggers a sync of a single Argo CD Application, Sveltos profile or
// Flux OCIRepository. The kube backend implements it for Argo CD, the sveltos
// backend for Sveltos and the flux backend for Flux; the
// event handler is agnostic to which is in use. Per-backend tunables are bound
// at construction, so only the Application (or profile, or repository) name
// varies per event.
type Syncer interface {
	Sync(ctx context.Context, appName string) error
	// RepoURLs lists the Applications the backend can see, mapping each name to
	// the repoURLs of its source(s). It is how an event's Space is matched to the
	// Applications that deploy from it.
	RepoURLs(ctx context.Context) (map[string][]string, error)
}

// newSyncer builds the Syncer for the configured controller.
func newSyncer(cfg config) (Syncer, error) {
	switch cfg.Controller {
	case ControllerSveltos:
		return sveltos.NewSyncer()
	case ControllerFlux:
		return flux.NewSyncer(flux.Config{Namespace: cfg.FluxNamespace})
	}
	return kube.NewSyncer(kube.Config{
		Namespace:   cfg.ArgoNamespace,
		RefreshType: cfg.ArgoRefreshType,
	})
}
