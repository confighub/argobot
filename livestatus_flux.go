// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
)

// Flux condition types and the reasons the mapping reads (fluxcd/pkg/apis/meta).
const (
	fluxConditionReady       = "Ready"
	fluxConditionHealthy     = "Healthy"
	fluxConditionReconciling = "Reconciling"
	fluxConditionStalled     = "Stalled"

	fluxReasonHealthCheckFailed = "HealthCheckFailed"
)

// fluxConditions is what the mapping reads of a Kustomization: the status and
// reason of its conditions, and whether it checks health.
type fluxConditions struct {
	// Ready, Healthy, Reconciling and Stalled are each condition's status
	// ("True", "False", "Unknown"), or "" when the condition is absent.
	Ready, Healthy, Reconciling, Stalled string
	// ReadyReason and HealthyReason are the Ready and Healthy conditions'
	// reasons, kept as Flux's own words.
	ReadyReason, HealthyReason string
	// HealthChecked says the Kustomization judges the health of what it applies
	// (spec.wait or spec.healthChecks). Without it, Ready means only "applied".
	HealthChecked bool
}

// The functions below normalize what Flux reports about a Release, keeping
// Flux's reasons as the reporter's words. Reporter, DataSource, Message and
// ObservedAt are the caller's to fill in. A Kustomization speaks about up to two
// Releases: the one the cluster runs, and a newer one it attempted but has not
// applied.
//
// They belong next to livestatus.FromArgoCD in the SDK (public/core/livestatus
// in the confighub repo) and live here until an SDK release carries them.

// fluxApplied says the Kustomization's last attempted revision is on the
// cluster: it is Ready, or its objects were applied and their health check is
// under way or failed. kustomize-controller marks Healthy Unknown only once it
// has applied a revision and starts checking it.
func fluxApplied(c fluxConditions) bool {
	return c.Ready == "True" ||
		c.ReadyReason == fluxReasonHealthCheckFailed ||
		(c.Healthy == "Unknown" && c.Reconciling == "True")
}

// fromFluxRunning is the status of the Release the cluster runs, the one the
// Kustomization last attempted.
func fromFluxRunning(c fluxConditions) goclientnew.ReleaseLiveStatus {
	sync := goclientnew.Unknown
	if fluxApplied(c) {
		sync = goclientnew.Synced
	}
	return goclientnew.ReleaseLiveStatus{
		Sync:              sync,
		Health:            fluxHealth(c),
		Operation:         fluxOperation(c),
		ReporterSync:      c.ReadyReason,
		ReporterHealth:    c.HealthyReason,
		ReporterOperation: c.ReadyReason,
	}
}

// fromFluxSuperseded is the status of the Release the cluster runs while the
// Kustomization attempts a newer one it has not applied. The running Release is
// still in sync and its apply succeeded; its health is the Healthy condition,
// which still describes it, since Flux judges a revision's health only once it
// has applied it.
func fromFluxSuperseded(c fluxConditions) goclientnew.ReleaseLiveStatus {
	health := goclientnew.ReleaseLiveStatusHealthUnknown
	switch {
	case !c.HealthChecked:
	case c.Healthy == "False":
		health = goclientnew.ReleaseLiveStatusHealthDegraded
	case c.Healthy == "True", c.Healthy == "":
		// A revision becomes lastAppliedRevision only once its check passed.
		health = goclientnew.ReleaseLiveStatusHealthHealthy
	}
	return goclientnew.ReleaseLiveStatus{
		Sync:           goclientnew.Synced,
		Health:         health,
		Operation:      goclientnew.ReleaseLiveStatusOperationSucceeded,
		ReporterHealth: c.HealthyReason,
	}
}

// fromFluxAttempted is the status of a Release the Kustomization attempted but
// has not applied: not on the cluster, so out of sync with nothing to judge the
// health of, and its operation still running or failed.
func fromFluxAttempted(c fluxConditions) goclientnew.ReleaseLiveStatus {
	var operation goclientnew.ReleaseLiveStatusOperation
	switch {
	case c.Stalled == "True", c.Ready == "False":
		operation = goclientnew.ReleaseLiveStatusOperationFailed
	case c.Reconciling == "True", c.Ready == "Unknown":
		operation = goclientnew.ReleaseLiveStatusOperationRunning
	}
	return goclientnew.ReleaseLiveStatus{
		Sync:              goclientnew.OutOfSync,
		Health:            goclientnew.ReleaseLiveStatusHealthUnknown,
		Operation:         operation,
		ReporterSync:      c.ReadyReason,
		ReporterOperation: c.ReadyReason,
	}
}

// fromFluxSourceFailed is the status of a Release an OCIRepository failed to
// pull, with the reason of the repository's Ready condition.
func fromFluxSourceFailed(reason string) goclientnew.ReleaseLiveStatus {
	return goclientnew.ReleaseLiveStatus{
		Sync:              goclientnew.OutOfSync,
		Health:            goclientnew.ReleaseLiveStatusHealthUnknown,
		Operation:         goclientnew.ReleaseLiveStatusOperationFailed,
		ReporterSync:      reason,
		ReporterOperation: reason,
	}
}

// fromFluxReplaced is the status of a Release the cluster no longer runs or
// attempts: another has replaced it.
func fromFluxReplaced() goclientnew.ReleaseLiveStatus {
	return goclientnew.ReleaseLiveStatus{
		Sync:   goclientnew.OutOfSync,
		Health: goclientnew.ReleaseLiveStatusHealthUnknown,
	}
}

// fluxHealth reads a Kustomization's health check. A Kustomization with none
// never looks at the health of what it applied, so its health is unknown. One
// with a health check is Ready only once the check passed, and Flux does not
// always record a Healthy condition beside it, so Ready alone reads as Healthy.
func fluxHealth(c fluxConditions) goclientnew.ReleaseLiveStatusHealth {
	switch {
	case !c.HealthChecked:
		return goclientnew.ReleaseLiveStatusHealthUnknown
	case c.Healthy == "True", c.Healthy == "" && c.Ready == "True":
		return goclientnew.ReleaseLiveStatusHealthHealthy
	case c.Healthy == "False", c.ReadyReason == fluxReasonHealthCheckFailed:
		return goclientnew.ReleaseLiveStatusHealthDegraded
	case c.Reconciling == "True", c.Healthy == "Unknown":
		return goclientnew.ReleaseLiveStatusHealthProgressing
	}
	return goclientnew.ReleaseLiveStatusHealthUnknown
}

// fluxOperation reads a Kustomization as an operation: a stalled or not-Ready one
// Failed, a reconciliation under way Running, and a Ready one Succeeded. Failure
// is checked first: Flux keeps Reconciling True (ProgressingWithRetry) while it
// retries a failed reconciliation.
func fluxOperation(c fluxConditions) goclientnew.ReleaseLiveStatusOperation {
	switch {
	case c.Stalled == "True", c.Ready == "False":
		return goclientnew.ReleaseLiveStatusOperationFailed
	case c.Reconciling == "True":
		return goclientnew.ReleaseLiveStatusOperationRunning
	case c.Ready == "True":
		return goclientnew.ReleaseLiveStatusOperationSucceeded
	}
	return ""
}
