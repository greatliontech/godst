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

// sync_runtime_dstSyncAcquire and sync_runtime_dstSyncObserve are the bridges for
// package sync's RWMutex, Once, WaitGroup, and Cond decision hooks.
//
//go:linkname sync_runtime_dstSyncAcquire sync.runtime_dstSyncAcquire
func sync_runtime_dstSyncAcquire(id unsafe.Pointer) {
	dstSyncAcquire(id)
}

//go:linkname sync_runtime_dstSyncObserve sync.runtime_dstSyncObserve
func sync_runtime_dstSyncObserve(id unsafe.Pointer) {
	dstSyncObserve(id)
}

//go:linkname sync_runtime_dstRecordSyncRelease sync.runtime_dstRecordSyncRelease
func sync_runtime_dstRecordSyncRelease(id unsafe.Pointer) {
	dstRecordSyncRelease(id)
}

//go:linkname sync_runtime_dstRecordSyncAcquire sync.runtime_dstRecordSyncAcquire
func sync_runtime_dstRecordSyncAcquire(id unsafe.Pointer) {
	dstRecordSyncAcquire(id)
}

//go:linkname __tsan_thread_ignore_begin _ZN6__tsan17ThreadIgnoreBeginEPNS_11ThreadStateEm
var __tsan_thread_ignore_begin byte

//go:linkname __tsan_thread_ignore_end _ZN6__tsan15ThreadIgnoreEndEPNS_11ThreadStateE
var __tsan_thread_ignore_end byte

//go:cgo_import_static _ZN6__tsan17ThreadIgnoreBeginEPNS_11ThreadStateEm
//go:cgo_import_static _ZN6__tsan15ThreadIgnoreEndEPNS_11ThreadStateE

// dstKernelRaceEnter and dstKernelRaceExit suppress both TSan synchronization
// events (RaceDisable/RaceEnable) and per-thread memory access instrumentation
// (ThreadIgnoreBegin/ThreadIgnoreEnd) inside modeled kernel critical sections.
// Unlike acquiring/releasing a shared Go mutex, ignoring modeled-kernel memory
// and sync operations neither reports false races on internal kernel tables nor
// publishes universe-global happens-before edges between simulated processes or
// unrelated goroutines.
//
//go:nosplit
func dstKernelRaceEnter() {
	gp := getg()
	if gp == nil || gp.racectx == 0 {
		return
	}
	RaceDisable()
	racecall(&__tsan_thread_ignore_begin, gp.racectx, 0, 0, 0)
}

//go:nosplit
func dstKernelRaceExit() {
	gp := getg()
	if gp == nil || gp.racectx == 0 {
		return
	}
	racecall(&__tsan_thread_ignore_end, gp.racectx, 0, 0, 0)
	RaceEnable()
}

//go:nosplit
func dstKernelLiftGoStartIgnoreSync(spawng *g) bool {
	return spawng.dstKernelDepth > 0 && int32(spawng.raceignore) == spawng.dstKernelDepth
}

// dstKernelRaceReleaseMerge and dstKernelRaceAcquire record an intentional
// user-visible happens-before edge (modeled file, pipe, socket, flock, or
// futex synchronization) atomically inside a KernelMutex critical section in
// both the DPOR HB shadow and TSan.
//
// KernelMutex suppresses internal mutex/channel sync and ready edges via
// dstKernelDepth > 0 and RaceDisable() (__tsan_go_ignore_sync_begin). Recording
// user-level HB only before Lock() or after Unlock() would leave a schedulable
// Level-2 yield window between committing/observing kernel state and updating
// vector clocks. When no non-kernel RaceDisable is active
// (int32(gp.raceignore) == gp.dstKernelDepth), these helpers record the DPOR
// user sync event and temporarily lift TSan's ignore_sync flag across
// __tsan_release_merge / __tsan_acquire in lockstep.
//
//go:linkname dstKernelRaceReleaseMerge
//go:nosplit
func dstKernelRaceReleaseMerge(addr unsafe.Pointer) {
	if addr == nil || !isvalidaddr(addr) {
		return
	}
	gp := getg()
	if gp == nil || gp != gp.m.curg || gp.racectx == 0 {
		return
	}
	if int32(gp.raceignore) != gp.dstKernelDepth {
		return
	}
	dstRecordUserSyncEvent(dstSyncEventRelease, uintptr(addr), dstSyncCoarseAux)
	if gp.dstKernelDepth == 0 {
		racereleasemergeg(gp, addr)
		return
	}
	racecall(&__tsan_go_ignore_sync_end, gp.racectx, 0, 0, 0)
	racecall(&__tsan_release_merge, gp.racectx, uintptr(addr), 0, 0)
	racecall(&__tsan_go_ignore_sync_begin, gp.racectx, 0, 0, 0)
}

//go:linkname dstKernelRaceAcquire
//go:nosplit
func dstKernelRaceAcquire(addr unsafe.Pointer) {
	if addr == nil || !isvalidaddr(addr) {
		return
	}
	gp := getg()
	if gp == nil || gp != gp.m.curg || gp.racectx == 0 {
		return
	}
	if int32(gp.raceignore) != gp.dstKernelDepth {
		return
	}
	dstRecordUserSyncEvent(dstSyncEventAcquire, uintptr(addr), dstSyncCoarseAux)
	if gp.dstKernelDepth == 0 {
		raceacquireg(gp, addr)
		return
	}
	racecall(&__tsan_go_ignore_sync_end, gp.racectx, 0, 0, 0)
	racecall(&__tsan_acquire, gp.racectx, uintptr(addr), 0, 0)
	racecall(&__tsan_go_ignore_sync_begin, gp.racectx, 0, 0, 0)
}
