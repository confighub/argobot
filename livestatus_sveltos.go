// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
)

// fromSveltos normalizes what Sveltos reports about a running Release: a
// ClusterSummary's featureSummaries[].status and a HealthCheckReport's
// healthStatus, keeping each value as Sveltos reported it. Reporter, DataSource,
// Message and ObservedAt are the caller's to fill in.
//
// It belongs next to livestatus.FromArgoCD in the SDK (public/core/livestatus in
// the confighub repo) and lives here until an SDK release carries it as
// livestatus.FromSveltos.
func fromSveltos(featureStatus, healthStatus string) goclientnew.ReleaseLiveStatus {
	return goclientnew.ReleaseLiveStatus{
		Sync:              sveltosSync(featureStatus),
		Health:            sveltosHealth(healthStatus),
		Operation:         sveltosOperation(featureStatus),
		ReporterSync:      featureStatus,
		ReporterHealth:    healthStatus,
		ReporterOperation: featureStatus,
	}
}

// sveltosSync reads a feature status as a sync status: Provisioned means the
// profile's content is deployed. Nothing else says whether it is.
func sveltosSync(status string) goclientnew.ReleaseLiveStatusSync {
	if status == "Provisioned" {
		return goclientnew.Synced
	}
	return goclientnew.Unknown
}

// sveltosHealth maps a HealthCheckReport's healthStatus, whose values are a
// subset of Argo CD's.
func sveltosHealth(status string) goclientnew.ReleaseLiveStatusHealth {
	switch status {
	case "Healthy":
		return goclientnew.ReleaseLiveStatusHealthHealthy
	case "Progressing":
		return goclientnew.ReleaseLiveStatusHealthProgressing
	case "Degraded":
		return goclientnew.ReleaseLiveStatusHealthDegraded
	case "Suspended":
		return goclientnew.ReleaseLiveStatusHealthSuspended
	}
	return goclientnew.ReleaseLiveStatusHealthUnknown
}

// sveltosOperation reads a feature status as an operation: Provisioning is a
// deploy under way, Provisioned one that completed, and Failed or
// FailedNonRetriable one that did not. Any other status (Removing, Blocked, ...)
// is no deploy operation.
func sveltosOperation(status string) goclientnew.ReleaseLiveStatusOperation {
	switch status {
	case "Provisioning":
		return goclientnew.ReleaseLiveStatusOperationRunning
	case "Provisioned":
		return goclientnew.ReleaseLiveStatusOperationSucceeded
	case "Failed", "FailedNonRetriable":
		return goclientnew.ReleaseLiveStatusOperationFailed
	}
	return ""
}
