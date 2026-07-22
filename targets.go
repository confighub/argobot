// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"

	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/confighub/sdk/core/worker/lib"
)

// discoverWorkerTargets asks ConfigHub which Targets the connecting worker is
// the BridgeWorker for, so argobot can scope its event subscription to just its
// own blast radius without being told the Target IDs out of band.
//
// It authenticates with the worker credentials (the same identity the event
// consumer uses) and lists Targets filtered by BridgeWorkerID. A worker is
// permitted to see its own Targets this way even though it cannot read Spaces.
// The returned slice may be empty — a worker that is the BridgeWorker for no
// Target — which the caller handles.
func discoverWorkerTargets(ctx context.Context, cfg config) ([]string, error) {
	fc := lib.NewWorkerFrontdoorClient(cfg.ConfigHubURL, "", cfg.WorkerID, cfg.WorkerSecret)
	defer fc.Close()

	client := fc.GetClient()
	if client == nil {
		return nil, fmt.Errorf("worker frontdoor authentication failed")
	}

	where := fmt.Sprintf("BridgeWorkerID = '%s'", cfg.WorkerID)
	resp, err := client.ListAllTargetsWithResponse(ctx, &goclientnew.ListAllTargetsParams{Where: &where})
	if err != nil {
		return nil, fmt.Errorf("listing worker targets: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("listing worker targets: unexpected HTTP %d: %s",
			resp.HTTPResponse.StatusCode, string(resp.Body))
	}

	var ids []string
	for _, et := range *resp.JSON200 {
		if et.Target == nil {
			continue
		}
		ids = append(ids, et.Target.TargetID.String())
	}
	return ids, nil
}
