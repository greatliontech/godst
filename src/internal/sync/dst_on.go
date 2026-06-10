// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && race

package sync

import "unsafe"

// dstHookEnabled compile-time gates the Mutex sync-decision announces: true only in
// this -tags dst -race build, false in dst_off.go. Mutex methods guard dstSyncAcquire
// calls with `if dstHookEnabled`, so in any other build the calls are dead-code-
// eliminated and the methods are byte-identical to upstream (DST-L2-4) — a guaranteed
// const fold, not a dependence on the inliner erasing an empty stub.
const dstHookEnabled = true

// runtime_dstSyncAcquire is the runtime's DST Level-2 sync-decision transition
// hook, pushed from package runtime as internal/sync.runtime_dstSyncAcquire (see
// runtime.internal_sync_runtime_dstSyncAcquire, mirroring the runtime_SemacquireMutex
// bridge above). It announces a mutex state decision on the mutex identity to the DST
// scheduler. Present only under a -tags dst -race build (this file's build
// tag); in any other build dstSyncAcquire is the empty stub in dst_off.go.
//
//go:linkname runtime_dstSyncAcquire
func runtime_dstSyncAcquire(id unsafe.Pointer)

//go:linkname runtime_dstRecordSyncRelease
func runtime_dstRecordSyncRelease(id unsafe.Pointer)

//go:linkname runtime_dstRecordSyncAcquire
func runtime_dstRecordSyncAcquire(id unsafe.Pointer)

// dstSyncAcquire announces mutex m's state decision to the DST scheduler. Lock calls it
// before the fast-path CAS, TryLock before failed-state rejection, and Unlock before
// clearing the lock bit, so contending goroutines record conflicts on the mutex
// identity and DPOR explores BOTH decision orders. Announcing only in the contended
// slow path (semacquire) would be too late: the uncontended winner of the CAS never
// enters it, so only one acquirer would record a conflict and the alternative order
// would be silently dropped (a DST-L2-3 completeness gap). The identity is the mutex
// pointer — the same value internal/sync
// passes to race.Acquire(unsafe.Pointer(m)) — so all contenders announce one identity
// by construction. The runtime guards the yield (bubble goroutine under the scheduled
// strategy, no runtime lock held) and skips it otherwise; a pre-acquire yield never
// runs a blocked goroutine (DST-L2-1) and skipping is always sound.
func dstSyncAcquire(m *Mutex) {
	runtime_dstSyncAcquire(unsafe.Pointer(m))
}

func dstSyncRelease(m *Mutex) {
	runtime_dstRecordSyncRelease(unsafe.Pointer(m))
}

func dstSyncAcquireHB(m *Mutex) {
	runtime_dstRecordSyncAcquire(unsafe.Pointer(m))
}
