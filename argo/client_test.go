// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package argo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestRepoURLs(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"single"},"spec":{"source":{"repoURL":"oci://gw/space/argo-single"}}},
			{"metadata":{"name":"multi"},"spec":{"sources":[{"repoURL":"https://charts.example.com/stable"},{"repoURL":"oci://gw/space/argo-multi"}]}},
			{"metadata":{"name":"sourceless"},"spec":{}}
		]}`))
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{Server: srv.URL, Token: "tok", AppNamespace: "team-a"})
	got, err := c.RepoURLs(context.Background())
	if err != nil {
		t.Fatalf("RepoURLs() error = %v", err)
	}

	if gotPath != "/api/v1/applications" || gotQuery != "appNamespace=team-a" || gotAuth != "Bearer tok" {
		t.Errorf("request = %s?%s auth=%q", gotPath, gotQuery, gotAuth)
	}
	want := map[string][]string{
		"single":     {"oci://gw/space/argo-single"},
		"multi":      {"https://charts.example.com/stable", "oci://gw/space/argo-multi"},
		"sourceless": nil,
	}
	if len(got) != len(want) {
		t.Fatalf("RepoURLs() = %v, want %v", got, want)
	}
	for name, urls := range want {
		if !slices.Equal(got[name], urls) {
			t.Errorf("RepoURLs()[%q] = %v, want %v", name, got[name], urls)
		}
	}
}

func TestRepoURLsEmptyAndErrors(t *testing.T) {
	// Argo CD returns "items": null when there are no Applications.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":null}`))
	}))
	defer empty.Close()
	got, err := NewClient(ClientConfig{Server: empty.URL}).RepoURLs(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("RepoURLs() = %v, %v; want empty, nil", got, err)
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "permission denied", http.StatusForbidden)
	}))
	defer denied.Close()
	if _, err := NewClient(ClientConfig{Server: denied.URL}).RepoURLs(context.Background()); err == nil {
		t.Fatal("RepoURLs() error = nil, want an error on HTTP 403")
	}
}
