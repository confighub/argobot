// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"reflect"
	"testing"

	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/google/uuid"
)

func TestTargetsGrantedTo(t *testing.T) {
	me := uuid.New().String()
	other := uuid.New().String()
	target := func(id uuid.UUID, permissions *goclientnew.Permissions) goclientnew.ExtendedTarget {
		return goclientnew.ExtendedTarget{Target: &goclientnew.Target{TargetID: id, Permissions: permissions}}
	}
	granted := uuid.New()
	viewOnly := uuid.New()
	someoneElse := uuid.New()
	noPermissions := uuid.New()
	targets := []goclientnew.ExtendedTarget{
		target(granted, &goclientnew.Permissions{
			"View":         {UserIDs: map[string]bool{me: true}},
			"ViewChildren": {UserIDs: map[string]bool{me: true, other: true}},
		}),
		target(viewOnly, &goclientnew.Permissions{"View": {UserIDs: map[string]bool{me: true}}}),
		target(someoneElse, &goclientnew.Permissions{"ViewChildren": {UserIDs: map[string]bool{other: true}}}),
		target(noPermissions, nil),
		{},
	}

	got := targetsGrantedTo(targets, me)
	want := []string{granted.String()}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targetsGrantedTo = %v, want %v", got, want)
	}
}
