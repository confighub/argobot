// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import "testing"

// setBaseEnv sets the always-required ConfigHub vars and clears the mode/Argo
// vars so each case starts from a known state.
func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CONFIGHUB_URL", "https://hub.example.com")
	t.Setenv("CONFIGHUB_WORKER_ID", "worker")
	t.Setenv("CONFIGHUB_WORKER_SECRET", "secret")
	for _, k := range []string{
		"ARGO_SYNC_MODE", "ARGOCD_SERVER", "ARGOCD_AUTH_TOKEN",
		"ARGO_NAMESPACE", "ARGO_REFRESH_TYPE",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadConfigModes(t *testing.T) {
	t.Run("default mode is kubernetes with defaults", func(t *testing.T) {
		setBaseEnv(t)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error = %v", err)
		}
		if cfg.ArgoSyncMode != SyncModeKubernetes {
			t.Errorf("ArgoSyncMode = %q, want %q", cfg.ArgoSyncMode, SyncModeKubernetes)
		}
		if cfg.ArgoNamespace != "argocd" {
			t.Errorf("ArgoNamespace = %q, want %q", cfg.ArgoNamespace, "argocd")
		}
		if cfg.ArgoRefreshType != "hard" {
			t.Errorf("ArgoRefreshType = %q, want %q", cfg.ArgoRefreshType, "hard")
		}
	})

	t.Run("kubernetes mode needs no Argo CD credentials", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("ARGO_SYNC_MODE", "kubernetes")
		if _, err := loadConfig(); err != nil {
			t.Fatalf("loadConfig() error = %v", err)
		}
	})

	t.Run("argocd mode requires server and token", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("ARGO_SYNC_MODE", "argocd")
		if _, err := loadConfig(); err == nil {
			t.Fatal("loadConfig() error = nil, want missing ARGOCD_SERVER/ARGOCD_AUTH_TOKEN")
		}

		t.Setenv("ARGOCD_SERVER", "https://argocd.example.com")
		t.Setenv("ARGOCD_AUTH_TOKEN", "token")
		if _, err := loadConfig(); err != nil {
			t.Fatalf("loadConfig() error = %v", err)
		}
	})

	t.Run("unknown mode is rejected", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("ARGO_SYNC_MODE", "bogus")
		if _, err := loadConfig(); err == nil {
			t.Fatal("loadConfig() error = nil, want invalid mode error")
		}
	})
}
