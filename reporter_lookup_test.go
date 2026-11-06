// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/google/uuid"
)

// fakeSpaces serves GET /space, listing the one Space each slug currently names.
func fakeSpaces(t *testing.T, spaces map[string]uuid.UUID) *releaseLookup {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/space" {
			http.NotFound(w, req)
			return
		}
		var list []goclientnew.ExtendedSpace
		for slug, id := range spaces {
			if req.URL.Query().Get("where") == "Slug = '"+slug+"'" {
				list = append(list, goclientnew.ExtendedSpace{Space: &goclientnew.Space{Slug: slug, SpaceID: id}})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(list); err != nil {
			t.Errorf("encode spaces: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	api, err := goclientnew.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	lookup := newReleaseLookup(liveStatusWriter{api: api})
	return &lookup
}

// A Space deleted and created again with the same slug has a new id, and the
// reporter has to report on the new Space rather than the deleted one.
func TestGetSpaceIDFollowsRecreatedSpace(t *testing.T) {
	ctx := context.Background()
	first, second := uuid.New(), uuid.New()
	spaces := map[string]uuid.UUID{"demo": first}
	lookup := fakeSpaces(t, spaces)

	id, known, err := lookup.getSpaceID(ctx, "demo")
	if err != nil || !known || id != first {
		t.Fatalf("getSpaceID = %s, %v, %v; want %s, true, nil", id, known, err, first)
	}

	delete(spaces, "demo")
	if id, known, err := lookup.getSpaceID(ctx, "demo"); err != nil || known {
		t.Fatalf("getSpaceID of a deleted Space = %s, %v, %v; want not known", id, known, err)
	}

	spaces["demo"] = second
	id, known, err = lookup.getSpaceID(ctx, "demo")
	if err != nil || !known || id != second {
		t.Fatalf("getSpaceID of the recreated Space = %s, %v, %v; want %s, true, nil", id, known, err, second)
	}
}

// A failure to ask ConfigHub is an error, so the report is retried rather than
// the Application being taken for one argobot is not responsible for.
func TestGetSpaceIDServerErrorIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	api, err := goclientnew.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	lookup := newReleaseLookup(liveStatusWriter{api: api})
	if _, _, err := lookup.getSpaceID(context.Background(), "demo"); err == nil {
		t.Fatal("getSpaceID on HTTP 503: want an error")
	}
}
