// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package user

import _ "unsafe" // for go:linkname

// dstSimUser reports the simulated current user under deterministic simulation
// testing (testing/simulation). The runtime holds the per-run simulated identity;
// ok is false outside a run, so Current falls through to the real lookup then.
// See runtime.dstSimUser.
//
//go:linkname dstSimUser runtime.dstSimUser
func dstSimUser() (uid, gid int, username, name, home string, ok bool)
