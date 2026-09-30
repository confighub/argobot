// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"

	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/confighub/sdk/core/worker/lib"
)

// discoverWorkerTargets asks ConfigHub which Targets the connecting worker has
// been granted access to, so argobot can scope its event subscription to just
// its own blast radius without being told the Target IDs out of band.
//
// It authenticates with the worker credentials (the same identity the event
// consumer uses), learns the worker's bot user from /me, and keeps the Targets
// whose Permissions grant that user ViewChildren: the grant that lets a puller
// pull the Releases published for the Target, and so the one a worker Argo
// pulls as holds. The returned slice may be empty — a worker granted on no
// Target — which the caller handles.
func discoverWorkerTargets(ctx context.Context, cfg config) ([]string, error) {
	fc := lib.NewWorkerFrontdoorClient(cfg.ConfigHubURL, "", cfg.WorkerID, cfg.WorkerSecret)
	defer fc.Close()

	client := fc.GetClient()
	if client == nil {
		return nil, fmt.Errorf("worker frontdoor authentication failed")
	}

	me, err := client.GetMeWithResponse(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the worker's user: %w", err)
	}
	if me.JSON200 == nil {
		return nil, fmt.Errorf("reading the worker's user: unexpected HTTP %d: %s",
			me.HTTPResponse.StatusCode, string(me.Body))
	}

	resp, err := client.ListAllTargetsWithResponse(ctx, &goclientnew.ListAllTargetsParams{})
	if err != nil {
		return nil, fmt.Errorf("listing worker targets: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("listing worker targets: unexpected HTTP %d: %s",
			resp.HTTPResponse.StatusCode, string(resp.Body))
	}
	return targetsGrantedTo(*resp.JSON200, me.JSON200.UserID.String()), nil
}

// targetsGrantedTo returns the IDs of the Targets whose Permissions grant the
// user ViewChildren.
func targetsGrantedTo(targets []goclientnew.ExtendedTarget, userID string) []string {
	var ids []string
	for _, et := range targets {
		if et.Target == nil || et.Target.Permissions == nil {
			continue
		}
		if (*et.Target.Permissions)["ViewChildren"].UserIDs[userID] {
			ids = append(ids, et.Target.TargetID.String())
		}
	}
	return ids
}
