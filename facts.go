// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"log"

	"github.com/confighub/argobot/kube"
	"github.com/confighub/sdk/core/cubapi"
	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/confighub/sdk/k8sutil/collect"
)

// collectTargetFacts collects facts about the cluster argobot runs in (its
// Kubernetes version, the resource types it serves, its storage and ingress
// classes) and stores them on the Target cfg.Target names, under the reserved
// "Cluster." prefix, as `cub k8s collect` does. argobot is authorized by Edit
// on the Target.
//
// It runs once, at startup, so a Target's facts follow its cluster across an
// upgrade as soon as argobot restarts.
func collectTargetFacts(ctx context.Context, cfg config, hub *cubapi.Client) error {
	restConfig, err := kube.RestConfig()
	if err != nil {
		return err
	}
	facts, err := collect.Facts(ctx, restConfig)
	if err != nil {
		return err
	}
	target, err := resolveTarget(ctx, hub, cubapi.ParseRef(cfg.Target))
	if err != nil {
		return err
	}
	if _, err := collect.UpdateTarget(ctx, hub.API, target, facts); err != nil {
		return fmt.Errorf("update the facts of target %s: %w", cfg.Target, err)
	}
	log.Printf("[INFO] argobot: stored %d cluster facts on target %s (Kubernetes %s)",
		len(facts), cfg.Target, facts[collect.FactPrefix+"KubernetesVersion"])
	return nil
}

// resolveTarget finds the Target ref names. argobot's grants are on the Target,
// not on its Space, so a space/slug reference is not resolved through the
// Space: the Targets of that slug are listed across the organization, which
// shows the ones argobot may view, and the Space narrows them where it can be
// seen.
func resolveTarget(ctx context.Context, hub *cubapi.Client, ref cubapi.Ref) (*goclientnew.Target, error) {
	if ref.IsID() {
		resolved, err := cubapi.ResolveTarget(ctx, hub, ref, cubapi.ResolveOpts{})
		if err != nil {
			return nil, fmt.Errorf("resolve target %s: %w", ref, err)
		}
		return resolved.Target, nil
	}
	candidates, err := cubapi.ListTargets(ctx, hub, cubapi.NewWhere("").Slug(ref.Name), cubapi.ListOpts{Include: "SpaceID"})
	if err != nil {
		return nil, fmt.Errorf("resolve target %s: %w", ref, err)
	}
	var matches []*goclientnew.Target
	for _, candidate := range candidates {
		if candidate.Target == nil {
			continue
		}
		if ref.Space == "" || ref.Space == "*" || candidate.Target.SpaceID.String() == ref.Space ||
			(candidate.Space != nil && candidate.Space.Slug == ref.Space) {
			matches = append(matches, candidate.Target)
		}
	}
	// The one Target of that slug, in a Space argobot cannot see to confirm.
	if len(matches) == 0 && len(candidates) == 1 && candidates[0].Target != nil && candidates[0].Space == nil {
		matches = append(matches, candidates[0].Target)
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("resolve target %s: no such target is visible to this identity; it needs View on the target", ref)
	case 1:
		return matches[0], nil
	}
	return nil, fmt.Errorf("resolve target %s: %d targets have that slug; name the space as space/slug, or the target by its ID", ref, len(matches))
}
