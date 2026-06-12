// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package user

import (
	"strconv"
	_ "unsafe" // for go:linkname
)

const dstSimUserEnabled = true

// dstSimUser reports the simulated current user under deterministic simulation
// testing (testing/simulation). The runtime holds the per-run simulated identity;
// ok is false outside a run, so Current falls through to the real lookup then.
// See runtime.dstSimUser.
//
//go:linkname dstSimUser runtime.dstSimUser
func dstSimUser() (uid, gid int, username, name, home string, ok bool)

// dstLookupUser returns the one entry of the simulated user database (the
// simulated current user) when a run is active. The database contains exactly
// this user and its group: lookups for anything else are deterministically
// unknown instead of host-database reads.
func dstLookupUser() (*User, bool) {
	return dstCurrentUser()
}

// dstLookupGroup returns the simulated user's group when a run is active.
func dstLookupGroup() (*Group, bool) {
	_, gid, username, _, _, ok := dstSimUser()
	if !ok {
		return nil, false
	}
	return &Group{Gid: strconv.Itoa(gid), Name: username}, true
}

func dstCurrentUser() (*User, bool) {
	uid, gid, username, name, home, ok := dstSimUser()
	if !ok {
		return nil, false
	}
	// Return a synthetic current user, uncached, so the real user is still
	// resolved outside the run. uid/gid are formatted from the runtime's single
	// int source of truth.
	return &User{
		Uid:      strconv.Itoa(uid),
		Gid:      strconv.Itoa(gid),
		Username: username,
		Name:     name,
		HomeDir:  home,
	}, true
}
