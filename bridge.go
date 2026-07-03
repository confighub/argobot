// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"time"

	"github.com/confighub/argobot/argo"
	funcapi "github.com/confighub/sdk/core/function/api"
	"github.com/confighub/sdk/core/worker/api"
	"github.com/confighub/sdk/core/workerapi"
)

// ProviderArgo is the ConfigHub ProviderType that routes apply operations to
// argobot. Create Targets with this provider type and bind them to argobot's
// worker; applying a Unit on such a Target force-syncs the Argo CD Application.
const ProviderArgo = api.ProviderType("Argo")

// ArgoBridge force-syncs an Argo CD Application on apply. It routes by no
// toolchain (ToolchainAny) because it operates on the Target, not on Unit data.
type ArgoBridge struct {
	argo *argo.Client
}

// NewArgoBridge builds an ArgoBridge over the given Argo CD client.
func NewArgoBridge(client *argo.Client) *ArgoBridge {
	return &ArgoBridge{argo: client}
}

func (b *ArgoBridge) ID() api.BridgeWorkerID {
	return api.BridgeWorkerID{
		ProviderType:   ProviderArgo,
		ToolchainTypes: []workerapi.ToolchainType{workerapi.ToolchainAny},
	}
}

func (b *ArgoBridge) Info(_ api.InfoOptions) api.BridgeWorkerInfo {
	return api.BridgeWorkerInfo{
		SupportedConfigTypes: []*api.SupportedConfigType{
			{
				ConfigTypeSignature: api.ConfigTypeSignature{
					ConfigType: api.ConfigType{
						ProviderType:  ProviderArgo,
						ToolchainType: workerapi.ToolchainAny,
					},
					Options: []api.BridgeOption{
						{
							Name:        "AppNamespace",
							Description: "Namespace of the Argo CD Application, for apps-in-any-namespace. Optional.",
							Required:    false,
							DataType:    funcapi.DataTypeString,
							Example:     "argocd",
						},
						{
							Name:        "Prune",
							Description: "Prune resources during sync (true/false). Defaults to false.",
							Required:    false,
							DataType:    funcapi.DataTypeString,
							Example:     "false",
						},
						{
							Name:        "Force",
							Description: "Force sync using the apply force strategy (true/false). Defaults to true.",
							Required:    false,
							DataType:    funcapi.DataTypeString,
							Example:     "true",
						},
					},
				},
			},
		},
	}
}

// Apply force-syncs the Argo CD Application identified by the Target's
// BridgeHandle, falling back to the Unit slug when the handle is empty.
func (b *ArgoBridge) Apply(ctx api.BridgeWorkerContext, payload api.BridgeWorkerPayload) error {
	startedAt := time.Now()
	_ = ctx.SendStatus(&api.ActionResult{
		UnitID:            payload.UnitID,
		SpaceID:           payload.SpaceID,
		QueuedOperationID: payload.QueuedOperationID,
		ActionResultBaseMeta: api.ActionResultBaseMeta{
			Action:    api.ActionApply,
			Result:    api.ActionResultNone,
			Status:    api.ActionStatusProgressing,
			Message:   "Starting Argo CD sync",
			StartedAt: startedAt,
		},
	})

	appName := payload.BridgeHandle
	if appName == "" {
		appName = payload.UnitSlug
	}

	syncErr := b.argo.Sync(ctx.Context(), appName, argo.SyncOptions{
		AppNamespace: payload.TargetOptions["AppNamespace"],
		Prune:        payload.TargetOptions["Prune"] == "true",
		Force:        forceEnabled(payload.TargetOptions),
	})

	terminatedAt := time.Now()
	if syncErr != nil {
		return ctx.SendStatus(&api.ActionResult{
			UnitID:            payload.UnitID,
			SpaceID:           payload.SpaceID,
			QueuedOperationID: payload.QueuedOperationID,
			ActionResultBaseMeta: api.ActionResultBaseMeta{
				Action:       api.ActionApply,
				Result:       api.ActionResultApplyFailed,
				Status:       api.ActionStatusFailed,
				Message:      fmt.Sprintf("Argo CD sync of %q failed: %v", appName, syncErr),
				StartedAt:    startedAt,
				TerminatedAt: &terminatedAt,
			},
			ErrorMessages: []string{syncErr.Error()},
		})
	}

	return ctx.SendStatus(&api.ActionResult{
		UnitID:            payload.UnitID,
		SpaceID:           payload.SpaceID,
		QueuedOperationID: payload.QueuedOperationID,
		ActionResultBaseMeta: api.ActionResultBaseMeta{
			Action:       api.ActionApply,
			Result:       api.ActionResultApplyCompleted,
			Status:       api.ActionStatusCompleted,
			Message:      fmt.Sprintf("Triggered Argo CD sync of %q", appName),
			StartedAt:    startedAt,
			TerminatedAt: &terminatedAt,
		},
	})
}

// Destroy is a no-op: argobot does not manage Argo CD Application teardown.
func (b *ArgoBridge) Destroy(ctx api.BridgeWorkerContext, payload api.BridgeWorkerPayload) error {
	return completed(ctx, payload, api.ActionDestroy, api.ActionResultDestroyCompleted,
		"argobot does not manage teardown; nothing to destroy")
}

// Refresh is a no-op: argobot holds no desired state, so it reports no drift.
func (b *ArgoBridge) Refresh(ctx api.BridgeWorkerContext, payload api.BridgeWorkerPayload) error {
	return completed(ctx, payload, api.ActionRefresh, api.ActionResultRefreshAndNoDrift,
		"argobot holds no desired state; no drift")
}

// Import is unsupported by argobot.
func (b *ArgoBridge) Import(ctx api.BridgeWorkerContext, payload api.BridgeWorkerPayload) error {
	now := time.Now()
	return ctx.SendStatus(&api.ActionResult{
		UnitID:            payload.UnitID,
		SpaceID:           payload.SpaceID,
		QueuedOperationID: payload.QueuedOperationID,
		ActionResultBaseMeta: api.ActionResultBaseMeta{
			Action:       api.ActionImport,
			Result:       api.ActionResultImportFailed,
			Status:       api.ActionStatusFailed,
			Message:      "import is not supported by argobot",
			StartedAt:    now,
			TerminatedAt: &now,
		},
	})
}

// Finalize is a no-op.
func (b *ArgoBridge) Finalize(_ api.BridgeWorkerContext, _ api.BridgeWorkerPayload) error {
	return nil
}

// completed reports a terminal success for a no-op action.
func completed(ctx api.BridgeWorkerContext, payload api.BridgeWorkerPayload, action api.ActionType, result api.ActionResultType, message string) error {
	now := time.Now()
	return ctx.SendStatus(&api.ActionResult{
		UnitID:            payload.UnitID,
		SpaceID:           payload.SpaceID,
		QueuedOperationID: payload.QueuedOperationID,
		ActionResultBaseMeta: api.ActionResultBaseMeta{
			Action:       action,
			Result:       result,
			Status:       api.ActionStatusCompleted,
			Message:      message,
			StartedAt:    now,
			TerminatedAt: &now,
		},
	})
}

// forceEnabled defaults to true — argobot is a force-sync bot; only an explicit
// "false" disables the force strategy.
func forceEnabled(opts map[string]string) bool {
	return opts["Force"] != "false"
}
