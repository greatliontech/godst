// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import _ "unsafe" // for go:linkname

//go:linkname dstSetProcessTeardownHook runtime.dstSetProcessTeardownHook
func dstSetProcessTeardownHook(fn func(proc uint32))

func init() { dstSetProcessTeardownHook(dstApplyProcessTeardown) }

func dstApplyProcessTeardown(proc uint32) {
	if !dstFSActive() {
		return
	}
	dstCloseProcFiles(proc)
	dstReleaseProcFDs(proc)
	dstMMapReleaseProc(proc)
}
