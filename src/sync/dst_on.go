// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && race

package sync

import "unsafe"

// dstHookEnabled compile-time gates sync-package DST hooks. True only in a
// -tags dst -race build; false in dst_off.go, so non-dst, plain -race, and
// dst-without-race builds compile these hooks away (DST-L2-4).
const dstHookEnabled = true

//go:linkname runtime_dstSyncAcquire
func runtime_dstSyncAcquire(id unsafe.Pointer)

func dstSyncAcquireRWMutex(rw *RWMutex) {
	// Use the writer mutex's internal identity so reader-vs-writer acquisition order
	// conflicts with the existing rw.w.Lock/TryLock hook.
	runtime_dstSyncAcquire(unsafe.Pointer(&rw.w.mu))
}
