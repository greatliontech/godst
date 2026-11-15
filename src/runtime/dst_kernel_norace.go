// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && !race

package runtime

import "unsafe"

//go:nosplit
func dstKernelRaceEnter() {}

//go:nosplit
func dstKernelRaceExit() {}

//go:linkname dstKernelRaceReleaseMerge
//go:nosplit
func dstKernelRaceReleaseMerge(addr unsafe.Pointer) {
	if addr == nil {
		return
	}
	dstRecordUserSyncEvent(dstSyncEventRelease, uintptr(addr), dstSyncCoarseAux)
}

//go:linkname dstKernelRaceAcquire
//go:nosplit
func dstKernelRaceAcquire(addr unsafe.Pointer) {
	if addr == nil {
		return
	}
	dstRecordUserSyncEvent(dstSyncEventAcquire, uintptr(addr), dstSyncCoarseAux)
}
