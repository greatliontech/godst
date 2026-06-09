// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && race

// DST Level-2 sync-acquisition auto-hook bridge into internal/sync. Built ONLY under
// -tags dst -race (this file's build tag) — the same gate as the memory-access
// auto-instrumentation — so a non-dst build, a plain -race build, and a -tags dst
// build WITHOUT -race carry none of this and are byte-identical to upstream/Seq-5
// (DST-L2-4). The channel-side hook needs no such bridge: chan.go's chansend/chanrecv
// call runtime.dstSyncAcquire directly, guarded inline by the `dstBuild && raceenabled`
// compile-time constants (const-folded to a no-op in every other build). Only the
// MUTEX bridge lives here, gated away entirely outside dst&&race, because internal/sync
// references it (via linkname) solely from its own dst&&race-tagged file.

package runtime

import "unsafe"

// internal_sync_runtime_dstSyncAcquire is the runtime-side push of the mutex-
// acquisition auto-hook into internal/sync (mirroring internal_sync_runtime_-
// SemacquireMutex in sema.go): internal/sync.Mutex.Lock calls it (as
// runtime_dstSyncAcquire) just before its fast-path CAS, so an unmodified mutex's
// acquisition order becomes a DPOR transition — the lock-order completeness the
// fast-path CAS would otherwise hide (a goroutine that wins the CAS records no memory
// access, so the order is an addr=0 decision DPOR cannot reverse; see design.md
// "Completeness boundary"). The identity is the mutex pointer — the same value
// internal/sync passes to race.Acquire(unsafe.Pointer(m)) — so all goroutines
// contending for one mutex announce the same conflict identity by construction.
//
//go:linkname internal_sync_runtime_dstSyncAcquire internal/sync.runtime_dstSyncAcquire
func internal_sync_runtime_dstSyncAcquire(id unsafe.Pointer) {
	dstSyncAcquire(id)
}
