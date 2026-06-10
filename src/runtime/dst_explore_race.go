// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && race

// DST Level-2 sync-decision auto-hook bridges into sync packages. Built ONLY under
// -tags dst -race (this file's build tag) — the same gate as the memory-access
// auto-instrumentation — so a non-dst build, a plain -race build, and a -tags dst
// build WITHOUT -race carry none of this and are byte-identical to upstream/Seq-5
// (DST-L2-4). The channel-side hook needs no such bridge: chan.go/select.go call
// runtime.dstSyncAcquire directly, guarded inline by the `dstBuild && raceenabled`
// compile-time constants (const-folded to a no-op in every other build).

package runtime

import "unsafe"

// internal_sync_runtime_dstSyncAcquire is the runtime-side push of the mutex sync
// decision hook into internal/sync (mirroring internal_sync_runtime_SemacquireMutex
// in sema.go): internal/sync.Mutex Lock/TryLock/Unlock calls it (as
// runtime_dstSyncAcquire) around state transitions that decide sync-object outcomes.
// The identity is the mutex pointer — the same value internal/sync passes to
// race.Acquire(unsafe.Pointer(m)) — so all goroutines contending for one mutex
// announce the same conflict identity by construction.
//
//go:linkname internal_sync_runtime_dstSyncAcquire internal/sync.runtime_dstSyncAcquire
func internal_sync_runtime_dstSyncAcquire(id unsafe.Pointer) {
	dstSyncAcquire(id)
}

//go:linkname internal_sync_runtime_dstRecordSyncRelease internal/sync.runtime_dstRecordSyncRelease
func internal_sync_runtime_dstRecordSyncRelease(id unsafe.Pointer) {
	dstRecordSyncRelease(id)
}

//go:linkname internal_sync_runtime_dstRecordSyncAcquire internal/sync.runtime_dstRecordSyncAcquire
func internal_sync_runtime_dstRecordSyncAcquire(id unsafe.Pointer) {
	dstRecordSyncAcquire(id)
}

// sync_runtime_dstSyncAcquire is the same bridge for package sync's RWMutex reader,
// writer-release, and reader-release paths, which must conflict on the RWMutex's
// embedded writer mutex identity.
//
//go:linkname sync_runtime_dstSyncAcquire sync.runtime_dstSyncAcquire
func sync_runtime_dstSyncAcquire(id unsafe.Pointer) {
	dstSyncAcquire(id)
}
