// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
)

func TestRunInstall(t *testing.T) {
	for _, tc := range []struct {
		controller, wantController string
	}{
		{"argo", ControllerArgoCD},
		{ControllerArgoCD, ControllerArgoCD},
		{ControllerFlux, ControllerFlux},
		{ControllerSveltos, ControllerSveltos},
	} {
		t.Run(tc.controller, func(t *testing.T) {
			var out strings.Builder
			if err := runInstall([]string{"--controller", tc.controller, "--tag", "v1.2.3"}, &out); err != nil {
				t.Fatalf("runInstall() error = %v", err)
			}
			got := out.String()
			if !strings.Contains(got, "image: ghcr.io/confighub/argobot:v1.2.3") {
				t.Errorf("output does not pin the image to v1.2.3")
			}
			if strings.Contains(got, "argobot:latest") {
				t.Errorf("output still references argobot:latest")
			}
			if !strings.Contains(got, "name: GITOPS_CONTROLLER\n              value: \""+tc.wantController+"\"") {
				t.Errorf("output does not set GITOPS_CONTROLLER=%s", tc.wantController)
			}
		})
	}

	t.Run("default is argocd at the build's version", func(t *testing.T) {
		orig := version
		t.Cleanup(func() { version = orig })
		version = "v9.9.9"
		var out strings.Builder
		if err := runInstall(nil, &out); err != nil {
			t.Fatalf("runInstall() error = %v", err)
		}
		if !strings.Contains(out.String(), "image: ghcr.io/confighub/argobot:v9.9.9") {
			t.Errorf("output does not pin the image to the build's version")
		}
		if !strings.Contains(out.String(), "kind: RoleBinding") {
			t.Errorf("output is not the Argo CD manifest")
		}
	})

	t.Run("development build installs latest", func(t *testing.T) {
		orig := version
		t.Cleanup(func() { version = orig })
		version = "dev"
		var out strings.Builder
		if err := runInstall([]string{"--image", "registry.example.com/argobot"}, &out); err != nil {
			t.Fatalf("runInstall() error = %v", err)
		}
		if !strings.Contains(out.String(), "image: registry.example.com/argobot:latest") {
			t.Errorf("output does not use the --image repository at latest")
		}
	})

	t.Run("unknown controller", func(t *testing.T) {
		var out strings.Builder
		err := runInstall([]string{"--controller", "rancher"}, &out)
		if err == nil || !strings.Contains(err.Error(), "invalid --controller") {
			t.Fatalf("runInstall() error = %v, want invalid --controller", err)
		}
		if out.Len() != 0 {
			t.Errorf("wrote output on error")
		}
	})
}
