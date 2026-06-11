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

//go:linkname runtime_dstRecordSyncRelease
func runtime_dstRecordSyncRelease(id unsafe.Pointer)

//go:linkname runtime_dstRecordSyncAcquire
func runtime_dstRecordSyncAcquire(id unsafe.Pointer)

func dstSyncAcquireRWMutex(rw *RWMutex) {
	// Use the writer mutex's internal identity so reader-vs-writer decisions conflict
	// with the existing rw.w Lock/TryLock/Unlock hooks.
	runtime_dstSyncAcquire(unsafe.Pointer(&rw.w.mu))
}

func dstSyncReleaseRWMutexUnlock(rw *RWMutex) {
	runtime_dstRecordSyncRelease(unsafe.Pointer(&rw.readerSem))
}

func dstSyncReleaseRWMutexRUnlock(rw *RWMutex) {
	runtime_dstRecordSyncRelease(unsafe.Pointer(&rw.writerSem))
}

func dstSyncAcquireHBRWMutexLock(rw *RWMutex) {
	runtime_dstRecordSyncAcquire(unsafe.Pointer(&rw.readerSem))
	runtime_dstRecordSyncAcquire(unsafe.Pointer(&rw.writerSem))
}

func dstSyncAcquireHBRWMutexRLock(rw *RWMutex) {
	runtime_dstRecordSyncAcquire(unsafe.Pointer(&rw.readerSem))
}
