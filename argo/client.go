// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

// Package argo is a thin client for the Argo CD REST API. It intentionally
// depends only on the standard library rather than the (large) upstream Argo CD
// module, since argobot only needs to trigger syncs.
package argo

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ClientConfig configures a Client. Server is the Argo CD API base URL
// (e.g. https://argocd.example.com); Token is an Argo CD API bearer token. The
// sync tunables (AppNamespace, Prune, Force) come from static configuration, so
// they are bound here rather than passed per call.
type ClientConfig struct {
	Server   string
	Token    string
	Insecure bool

	// AppNamespace is the namespace of the Application resource itself, for
	// Argo CD's apps-in-any-namespace mode. Empty means the default.
	AppNamespace string
	// Prune deletes resources that are no longer in the desired state.
	Prune bool
	// Force uses the apply force strategy, equivalent to a force sync in the UI.
	Force bool
}

// Client calls the Argo CD REST API.
type Client struct {
	server       string
	token        string
	http         *http.Client
	appNamespace string
	prune        bool
	force        bool
}

// NewClient builds a Client. When cfg.Insecure is set, TLS verification is
// skipped, which is common for Argo CD servers with self-signed certificates.
func NewClient(cfg ClientConfig) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in via ARGOCD_INSECURE
	}
	return &Client{
		server:       cfg.Server,
		token:        cfg.Token,
		http:         &http.Client{Timeout: 30 * time.Second, Transport: transport},
		appNamespace: cfg.AppNamespace,
		prune:        cfg.Prune,
		force:        cfg.Force,
	}
}

// Sync triggers a sync of the named Argo CD Application. Unlike a Kubernetes
// refresh, this deploys regardless of the Application's auto-sync setting. A
// non-2xx response is returned as an error including the response body.
func (c *Client) Sync(ctx context.Context, appName string) error {
	reqBody := map[string]any{"name": appName}
	if c.appNamespace != "" {
		reqBody["appNamespace"] = c.appNamespace
	}
	if c.prune {
		reqBody["prune"] = true
	}
	if c.force {
		reqBody["strategy"] = map[string]any{"apply": map[string]any{"force": true}}
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal sync request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/api/v1/applications/%s/sync", c.server, url.PathEscape(appName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build sync request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call argo cd: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("argo cd sync of %q returned %s: %s", appName, resp.Status, string(body))
	}
	return nil
}
