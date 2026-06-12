// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package user

const dstSimUserEnabled = false

func dstSimUser() (uid, gid int, username, name, home string, ok bool) { return }

func dstCurrentUser() (*User, bool) { return nil, false }

func dstLookupUser() (*User, bool) { return nil, false }

func dstLookupGroup() (*Group, bool) { return nil, false }
