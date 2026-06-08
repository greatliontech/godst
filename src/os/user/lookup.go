// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package user

import (
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
	if uid, gid, username, name, home, ok := dstSimUser(); ok {
		// Deterministic simulation: return a synthetic current user, uncached, so
		// the real user is still resolved outside the run (the dstSimUser check
		// runs before the cache, and we never populate the cache here). uid/gid are
		// formatted from the runtime's single int source of truth. See os/user/dst.go.
		return &User{
			Uid:      strconv.Itoa(uid),
			Gid:      strconv.Itoa(gid),
			Username: username,
			Name:     name,
			HomeDir:  home,
		}, nil
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
	if u, err := Current(); err == nil && u.Username == username {
		return u, err
	}
	return lookupUser(username)
}

// LookupId looks up a user by userid. If the user cannot be found, the
// returned error is of type [UnknownUserIdError].
func LookupId(uid string) (*User, error) {
	if u, err := Current(); err == nil && u.Uid == uid {
		return u, err
	}
	return lookupUserId(uid)
}

// LookupGroup looks up a group by name. If the group cannot be found, the
// returned error is of type [UnknownGroupError].
func LookupGroup(name string) (*Group, error) {
	return lookupGroup(name)
}

// LookupGroupId looks up a group by groupid. If the group cannot be found, the
// returned error is of type [UnknownGroupIdError].
func LookupGroupId(gid string) (*Group, error) {
	return lookupGroupId(gid)
}

// GroupIds returns the list of group IDs that the user is a member of.
func (u *User) GroupIds() ([]string, error) {
	return listGroups(u)
}
