// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// setBaseEnv sets the always-required ConfigHub vars and clears the mode/Argo
// vars so each case starts from a known state.
func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CONFIGHUB_URL", "https://hub.example.com")
	t.Setenv("CONFIGHUB_AUTH_PRIVATE_KEY", `{"kty":"OKP","crv":"Ed25519","x":"x","d":"d"}`)
	t.Setenv("CONFIGHUB_CHANNEL", "my-cluster/argobot")
	for _, k := range []string{
		"GITOPS_CONTROLLER", "ARGO_SYNC_MODE", "ARGOCD_SERVER", "ARGOCD_AUTH_TOKEN",
		"ARGO_NAMESPACE", "ARGO_REFRESH_TYPE", "ARGO_APP", "FLUX_NAMESPACE",
		"ARGOCD_INSECURE", "ARGO_APP_NAMESPACE", "ARGO_PRUNE", "ARGO_FORCE",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadConfigModes(t *testing.T) {
	t.Run("default controller is argocd with defaults", func(t *testing.T) {
		setBaseEnv(t)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error = %v", err)
		}
		if cfg.Controller != ControllerArgoCD {
			t.Errorf("Controller = %q, want %q", cfg.Controller, ControllerArgoCD)
		}
		if cfg.ArgoNamespace != "argocd" {
			t.Errorf("ArgoNamespace = %q, want %q", cfg.ArgoNamespace, "argocd")
		}
		if cfg.ArgoRefreshType != "hard" {
			t.Errorf("ArgoRefreshType = %q, want %q", cfg.ArgoRefreshType, "hard")
		}
	})

	t.Run("argocd sync mode is retired and warns", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("ARGO_SYNC_MODE", "argocd")
		var logs bytes.Buffer
		prev := log.Writer()
		log.SetOutput(&logs)
		t.Cleanup(func() { log.SetOutput(prev) })
		if _, err := loadConfig(); err != nil {
			t.Fatalf("loadConfig() error = %v, want ARGO_SYNC_MODE ignored", err)
		}
		if !strings.Contains(logs.String(), "[WARN] ARGO_SYNC_MODE is ignored") {
			t.Errorf("log = %q, want a warning that ARGO_SYNC_MODE is ignored", logs.String())
		}
	})

	t.Run("the key is required", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("CONFIGHUB_AUTH_PRIVATE_KEY", "")
		if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "CONFIGHUB_AUTH_PRIVATE_KEY") {
			t.Fatalf("loadConfig() error = %v, want missing CONFIGHUB_AUTH_PRIVATE_KEY", err)
		}
	})

	t.Run("the channel is required", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("CONFIGHUB_CHANNEL", "")
		if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "CONFIGHUB_CHANNEL") {
			t.Fatalf("loadConfig() error = %v, want missing CONFIGHUB_CHANNEL", err)
		}
	})

	t.Run("sveltos needs no Argo settings and ignores them", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("GITOPS_CONTROLLER", "sveltos")
		// A pinned Argo app and namespace must not affect Sveltos.
		t.Setenv("ARGO_NAMESPACE", "argo")
		t.Setenv("ARGO_APP", "pinned-app")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error = %v", err)
		}
		if cfg.Controller != ControllerSveltos {
			t.Errorf("Controller = %q, want %q", cfg.Controller, ControllerSveltos)
		}
		if cfg.ArgoApp != "" || cfg.ArgoNamespace != "" || cfg.ArgoRefreshType != "" {
			t.Errorf("Argo settings kept under Sveltos: %+v", cfg)
		}
	})

	t.Run("flux needs no Argo settings and ignores them", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("GITOPS_CONTROLLER", "flux")
		// A pinned Argo app and namespace must not affect Flux.
		t.Setenv("ARGO_NAMESPACE", "argo")
		t.Setenv("ARGO_APP", "pinned-app")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error = %v", err)
		}
		if cfg.Controller != ControllerFlux {
			t.Errorf("Controller = %q, want %q", cfg.Controller, ControllerFlux)
		}
		if cfg.ArgoApp != "" || cfg.ArgoNamespace != "" || cfg.ArgoRefreshType != "" {
			t.Errorf("Argo settings kept under Flux: %+v", cfg)
		}
		if cfg.FluxNamespace != "flux-system" {
			t.Errorf("FluxNamespace = %q, want %q", cfg.FluxNamespace, "flux-system")
		}

		t.Setenv("FLUX_NAMESPACE", "gitops")
		if cfg, err := loadConfig(); err != nil || cfg.FluxNamespace != "gitops" {
			t.Errorf("FLUX_NAMESPACE=gitops: FluxNamespace = %q, err = %v", cfg.FluxNamespace, err)
		}
	})

	t.Run("unknown controller is rejected", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("GITOPS_CONTROLLER", "bogus")
		if _, err := loadConfig(); err == nil {
			t.Fatal("loadConfig() error = nil, want invalid controller error")
		}
	})
}
