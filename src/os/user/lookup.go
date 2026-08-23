// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package user

import (
	"errors"
	"strconv"
	"sync"
)

const (
	userFile  = "/etc/passwd"
	groupFile = "/etc/group"
)

var colon = []byte{':'}

// Current returns the current user.
//
// The first call will cache the current user information.
// Subsequent calls will return the cached value and will not reflect
// changes to the current user.
func Current() (*User, error) {
	if dstSimUserEnabled {
		if u, ok := dstCurrentUser(); ok {
			// Deterministic simulation: return a synthetic current user before the
			// cache, but never populate the real-user cache with it.
			return u, nil
		}
	}
	cache.Do(func() { cache.u, cache.err = current() })
	if cache.err != nil {
		return nil, cache.err
	}
	u := *cache.u // copy
	return &u, nil
}

// cache of the current user
var cache struct {
	sync.Once
	u   *User
	err error
}

// Lookup looks up a user by username. If the user cannot be found, the
// returned error is of type [UnknownUserError].
func Lookup(username string) (*User, error) {
	if dstSimUserEnabled { // zero-cost guard: the stock path below stays inlinable
		if u, ok := dstLookupUser(); ok {
			// Deterministic simulation: the user database contains exactly the
			// simulated user; any other name is deterministically unknown rather
			// than a host-database lookup. See os/user/dst.go.
			if u.Username == username {
				return u, nil
			}
			return nil, UnknownUserError(username)
		}
	}
	if u, err := Current(); err == nil && u.Username == username {
		return u, err
	}
	return lookupUser(username)
}

// LookupId looks up a user by userid. If the user cannot be found, the
// returned error is of type [UnknownUserIdError].
func LookupId(uid string) (*User, error) {
	if dstSimUserEnabled { // zero-cost guard: the stock path below stays inlinable
		if u, ok := dstLookupUser(); ok {
			if u.Uid == uid {
				return u, nil
			}
			i, e := strconv.Atoi(uid)
			if e != nil {
				// The osusergo flavor (the in-simulation analog; no cgo database).
				return nil, errors.New("user: invalid userid " + uid)
			}
			return nil, UnknownUserIdError(i)
		}
	}
	if u, err := Current(); err == nil && u.Uid == uid {
		return u, err
	}
	return lookupUserId(uid)
}

// LookupGroup looks up a group by name. If the group cannot be found, the
// returned error is of type [UnknownGroupError].
func LookupGroup(name string) (*Group, error) {
	if dstSimUserEnabled { // zero-cost guard: the stock path below stays inlinable
		if g, ok := dstLookupGroup(); ok {
			if g.Name == name {
				return g, nil
			}
			return nil, UnknownGroupError(name)
		}
	}
	return lookupGroup(name)
}

// LookupGroupId looks up a group by groupid. If the group cannot be found, the
// returned error is of type [UnknownGroupIdError].
func LookupGroupId(gid string) (*Group, error) {
	if dstSimUserEnabled { // zero-cost guard: the stock path below stays inlinable
		if g, ok := dstLookupGroup(); ok {
			if g.Gid == gid {
				return g, nil
			}
			return nil, UnknownGroupIdError(gid)
		}
	}
	return lookupGroupId(gid)
}

// GroupIds returns the list of group IDs that the user is a member of.
func (u *User) GroupIds() ([]string, error) {
	if dstSimUserEnabled { // zero-cost guard: the stock path below stays inlinable
		if su, ok := dstLookupUser(); ok {
			if u.Uid == su.Uid || u.Username == su.Username {
				return []string{su.Gid}, nil
			}
			// Production (osusergo) returns the user's primary gid plus any
			// /etc/group memberships; the simulated database has no other group
			// entries, so a non-simulated user resolves to just its primary gid.
			if u.Gid != "" {
				return []string{u.Gid}, nil
			}
			return nil, UnknownUserError(u.Username)
		}
	}
	return listGroups(u)
}
