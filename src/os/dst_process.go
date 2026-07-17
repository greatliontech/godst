// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import _ "unsafe" // for go:linkname

//go:linkname dstSetProcessTeardownHook runtime.dstSetProcessTeardownHook
func dstSetProcessTeardownHook(fn func(proc uint32))

//go:linkname dstSetProcessStateTeardownHook runtime.dstSetProcessStateTeardownHook
func dstSetProcessStateTeardownHook(fn func(proc uint32))

//go:linkname dstEnvProcessTeardown syscall.dstEnvProcessTeardown
func dstEnvProcessTeardown(proc uint32)

func init() {
	dstSetProcessTeardownHook(dstApplyProcessTeardown)
	dstSetProcessStateTeardownHook(dstApplyProcessStateTeardown)
}

func dstApplyProcessTeardown(proc uint32) {
	if !dstFSActive() {
		return
	}
	dstCloseProcFiles(proc)
	dstReleaseProcFDs(proc)
	dstMMapReleaseProc(proc)
	dstFutexTeardownProc(proc)
	dstApplyProcessStateTeardown(proc)
}

func dstApplyProcessStateTeardown(proc uint32) {
	dstFS.mu.Lock()
	dstFSRoll()
	for key := range dstFS.cwds {
		if key[1] == proc {
			delete(dstFS.cwds, key)
		}
	}
	dstFS.mu.Unlock()
	dstEnvProcessTeardown(proc)
}
