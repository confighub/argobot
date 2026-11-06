// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/google/uuid"
)

// fakeHub is the part of the ConfigHub API releaseLookup reads: the Space each
// slug names, and each Space's Releases. Tests change them between lookups.
type fakeHub struct {
	spaces   map[string]uuid.UUID
	releases map[uuid.UUID][]goclientnew.Release
}

// serve starts the fake and returns a releaseLookup that reads from it.
func (h *fakeHub) serve(t *testing.T) *releaseLookup {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		where := req.URL.Query().Get("where")
		var body any
		switch {
		case req.URL.Path == "/space":
			list := []goclientnew.ExtendedSpace{}
			for slug, id := range h.spaces {
				if where == "Slug = '"+slug+"'" {
					list = append(list, goclientnew.ExtendedSpace{Space: &goclientnew.Space{Slug: slug, SpaceID: id}})
				}
			}
			body = list
		case strings.HasPrefix(req.URL.Path, "/space/") && strings.HasSuffix(req.URL.Path, "/release"):
			spaceID, err := uuid.Parse(strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/space/"), "/release"))
			if err != nil {
				http.NotFound(w, req)
				return
			}
			releases, ok := h.releases[spaceID]
			if !ok {
				http.NotFound(w, req)
				return
			}
			list := []goclientnew.ExtendedRelease{}
			for i := range releases {
				if where == "ManifestDigest = '"+releases[i].ManifestDigest+"'" {
					list = append(list, goclientnew.ExtendedRelease{Release: &releases[i]})
				}
			}
			body = list
		default:
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	api, err := goclientnew.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &releaseLookup{liveStatusWriter{api: api}}
}

// A Space deleted and created again with the same slug has a new id, and the
// reporter has to report on the new Space rather than the deleted one.
func TestGetSpaceIDFollowsRecreatedSpace(t *testing.T) {
	ctx := context.Background()
	first, second := uuid.New(), uuid.New()
	hub := &fakeHub{spaces: map[string]uuid.UUID{"demo": first}}
	lookup := hub.serve(t)

	id, known, err := lookup.getSpaceID(ctx, "demo")
	if err != nil || !known || id != first {
		t.Fatalf("getSpaceID = %s, %v, %v; want %s, true, nil", id, known, err, first)
	}

	delete(hub.spaces, "demo")
	if id, known, err := lookup.getSpaceID(ctx, "demo"); err != nil || known {
		t.Fatalf("getSpaceID of a deleted Space = %s, %v, %v; want not known", id, known, err)
	}

	hub.spaces["demo"] = second
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
	lookup := &releaseLookup{liveStatusWriter{api: api}}
	if _, _, err := lookup.getSpaceID(context.Background(), "demo"); err == nil {
		t.Fatal("getSpaceID on HTTP 503: want an error")
	}
}

// Publishing from an existing Tag, as a rollback does, makes a new Release with
// the manifest digest of the one the Tag was first published as. The status of
// what Argo runs belongs on the new Release, the newest with that digest.
func TestGetReleaseIDFollowsRollback(t *testing.T) {
	ctx := context.Background()
	spaceID := uuid.New()
	release := func(num int, digest string) goclientnew.Release {
		return goclientnew.Release{ReleaseID: uuid.New(), ReleaseNum: int64(num), ManifestDigest: digest}
	}
	r2, r3, r4 := release(2, "sha256:aaa"), release(3, "sha256:bbb"), release(4, "sha256:aaa")
	hub := &fakeHub{releases: map[uuid.UUID][]goclientnew.Release{spaceID: {r2}}}
	lookup := hub.serve(t)

	id, known, err := lookup.getReleaseID(ctx, spaceID, "sha256:aaa")
	if err != nil || !known || id != r2.ReleaseID {
		t.Fatalf("getReleaseID = %s, %v, %v; want release 2 (%s)", id, known, err, r2.ReleaseID)
	}

	hub.releases[spaceID] = []goclientnew.Release{r2, r3, r4}
	id, known, err = lookup.getReleaseID(ctx, spaceID, "sha256:aaa")
	if err != nil || !known || id != r4.ReleaseID {
		t.Fatalf("getReleaseID after the rollback = %s, %v, %v; want release 4 (%s)", id, known, err, r4.ReleaseID)
	}

	// A Release deleted while Argo still runs it has nothing to record on.
	hub.releases[spaceID] = []goclientnew.Release{r3}
	if id, known, err := lookup.getReleaseID(ctx, spaceID, "sha256:aaa"); err != nil || known {
		t.Fatalf("getReleaseID of deleted Releases = %s, %v, %v; want not known", id, known, err)
	}
}
