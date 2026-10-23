// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"embed"
	"flag"
	"fmt"
	"io"
	"strings"
)

// manifests are the Kubernetes deployments of argobot, one per controller,
// built into the binary so `argobot install` prints the one matching its own
// version.
//
//go:embed manifests/argobot.yaml manifests/flux/argobot.yaml manifests/sveltos/argobot.yaml
var manifests embed.FS

// version is the release argobot was built as, set with
// -ldflags "-X main.version=vX.Y.Z". A development build is "dev".
var version = "dev"

// defaultImage is the argobot image the committed manifests reference, at tag
// latest. `argobot install` replaces that reference.
const defaultImage = "ghcr.io/confighub/argobot"

// installManifests maps each controller `argobot install` accepts to its
// manifest. "argo" is accepted beside GITOPS_CONTROLLER's "argocd".
var installManifests = map[string]string{
	ControllerArgoCD:  "manifests/argobot.yaml",
	"argo":            "manifests/argobot.yaml",
	ControllerSveltos: "manifests/sveltos/argobot.yaml",
	ControllerFlux:    "manifests/flux/argobot.yaml",
}

// runInstall implements `argobot install`: it writes the deployment for the
// chosen controller to out, with the image pinned to --image and --tag, for
// `kubectl apply -f -` or `cub unit create`. The tag defaults to argobot's own
// version, so a released binary installs the image it was released with; a
// development build installs latest.
func runInstall(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	controller := fs.String("controller", ControllerArgoCD, "GitOps controller to install argobot for: argo, argocd, flux or sveltos")
	image := fs.String("image", defaultImage, "argobot image repository")
	tag := fs.String("tag", defaultTag(), "argobot image tag")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("install takes no arguments, got %q", fs.Args())
	}

	path, ok := installManifests[*controller]
	if !ok {
		return fmt.Errorf("invalid --controller %q: must be argo, %q, %q or %q",
			*controller, ControllerArgoCD, ControllerFlux, ControllerSveltos)
	}
	manifest, err := manifests.ReadFile(path)
	if err != nil {
		return err
	}

	const committed = "image: " + defaultImage + ":latest"
	if !strings.Contains(string(manifest), committed) {
		// Only a manifest edited without updating this is missing it.
		return fmt.Errorf("%s does not reference %s:latest", path, defaultImage)
	}
	pinned := strings.ReplaceAll(string(manifest), committed, "image: "+*image+":"+*tag)
	_, err = io.WriteString(out, pinned)
	return err
}

// defaultTag is the image tag `argobot install` pins by default.
func defaultTag() string {
	if version == "" || version == "dev" {
		return "latest"
	}
	return version
}
