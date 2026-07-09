// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package syscall

import _ "unsafe" // for go:linkname

//go:linkname dstPidAlive runtime.dstPidAlive
func dstPidAlive(pid int32) bool

func init() {
	dstSetKillHook(dstKill)
}

func dstKill(pid int, sig Signal) (Errno, bool) {
	if sig != 0 {
		panic("syscall: Kill unsupported under deterministic simulation")
	}
	pid32 := int32(pid)
	if int(pid32) != pid {
		return ESRCH, true
	}
	if dstPidAlive(pid32) {
		return 0, true
	}
	return ESRCH, true
}
