// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && race

// DST Level-2 sync-decision auto-hook bridges into sync packages. Built ONLY under
// -tags dst -race (this file's build tag) — the same gate as the memory-access
// auto-instrumentation — so a non-dst build, a plain -race build, and a -tags dst
// build WITHOUT -race carry none of this bridge and are hook-inert (DST-L2-4). The
// channel-side hook needs no such bridge: chan.go/select.go call
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

// HB-record suppression under runtime.RaceDisable is NOT handled in these
// bridges: dstRecordSyncEventForGID — the single choke point every HB recorder
// funnels through (these bridges, chan.go/select.go, dstAtomicYield) — checks
// the executing goroutine's raceignore, mirroring race.go's acquire/release
// variants. That is what suppresses the embedded writer mutex's HB events
// while RWMutex executes its internals under race.Disable() — the mutex's
// decision announce still fires there (the dstSyncAcquire bridges do not flow
// through the HB funnel), so DPOR exploration is unaffected. Public RWMutex HB
// hooks are placed before race.Disable()/after race.Enable() in
// sync/rwmutex.go, exactly where their race-annotation twins sit.

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

//go:linkname sync_runtime_dstRecordSyncRelease sync.runtime_dstRecordSyncRelease
func sync_runtime_dstRecordSyncRelease(id unsafe.Pointer) {
	dstRecordSyncRelease(id)
}

//go:linkname sync_runtime_dstRecordSyncAcquire sync.runtime_dstRecordSyncAcquire
func sync_runtime_dstRecordSyncAcquire(id unsafe.Pointer) {
	dstRecordSyncAcquire(id)
}
