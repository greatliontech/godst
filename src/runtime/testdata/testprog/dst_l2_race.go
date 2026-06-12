// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build race

// White-box fixture for the DST HB shadow's raceignore mirror: every HB
// recorder funnels through dstRecordSyncEventForGID, which honors the
// executing goroutine's raceignore exactly as race.go's acquire/release
// variants do, so the recorded sync-event stream must contain the public
// sync-primitive events and ONLY those — no embedded writer-mutex events from
// inside RWMutex internals (suppressed by upstream's race.Disable brackets),
// and nothing at all from mutex, channel, or atomic ops the SUT itself
// brackets with runtime.RaceDisable. Outcome-based
// exploration tests cannot see this property (HB records only prune, never
// change outcome sets — and RWMutex's embedded HB pairs are subsumed by the
// public sem-identity pairs in every reachable shape), so this fixture asserts
// on the event stream directly. Needs a -race build for runtime.RaceDisable and
// the dst&&race sync hooks; meaningful only when driven by the -tags dst -race
// parent test.

package main

import (
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing/simulation"
	"unsafe"
)

func init() {
	register("DSTSyncHBSuppress", DSTSyncHBSuppress)
}

//go:linkname dstSyncEventLenFP runtime.dstSyncEventLenFP
func dstSyncEventLenFP() int

//go:linkname dstSyncEventAtFP runtime.dstSyncEventAtFP
func dstSyncEventAtFP(i int) (kind uint8, id, aux uintptr, seq uint64, step, acc, order int)

// Mirror runtime's dstSyncEvent kind constants.
const (
	hbRelease = 1
	hbAcquire = 2
)

type hbEv struct {
	kind uint8
	id   uintptr
}

func hbEventsSince(from int) []hbEv {
	n := dstSyncEventLenFP()
	evs := make([]hbEv, 0, n-from)
	for i := from; i < n; i++ {
		kind, id, _, _, _, _, _ := dstSyncEventAtFP(i)
		evs = append(evs, hbEv{kind, id})
	}
	return evs
}

func DSTSyncHBSuppress() {
	var report string
	sut := func() bool {
		report = ""
		emit := func(name string, ok bool) {
			report += name + "=" + strconv.FormatBool(ok) + " "
		}

		// Plain Mutex: Lock then Unlock record exactly one acquire and one
		// release on the mutex identity (the announce is not an HB event).
		var mu sync.Mutex
		muID := uintptr(unsafe.Pointer(&mu))
		b := dstSyncEventLenFP()
		mu.Lock()
		mu.Unlock()
		evs := hbEventsSince(b)
		emit("mutexPair", len(evs) == 2 &&
			evs[0].kind == hbAcquire && evs[0].id == muID &&
			evs[1].kind == hbRelease && evs[1].id == muID)

		// A RaceDisable-bracketed pair records NOTHING: the HB shadow honors
		// raceignore exactly as the race detector does.
		var mu2 sync.Mutex
		b = dstSyncEventLenFP()
		runtime.RaceDisable()
		mu2.Lock()
		mu2.Unlock()
		runtime.RaceEnable()
		emit("ignoredPair", dstSyncEventLenFP() == b)

		// RWMutex: only the PUBLIC events on the readerSem/writerSem
		// identities (fields of rw) appear; the embedded writer mutex's
		// acquire/release inside Lock/Unlock are suppressed via upstream's
		// race.Disable brackets. The embedded mutex is rw's first field, so
		// its identity equals &rw — distinguishable from the sem fields.
		var rw sync.RWMutex
		rwLo := uintptr(unsafe.Pointer(&rw))
		rwHi := rwLo + unsafe.Sizeof(rw)
		inRW := func(e hbEv) bool { return e.id > rwLo && e.id < rwHi }

		b = dstSyncEventLenFP()
		rw.Lock()
		evs = hbEventsSince(b)
		emit("rwLock", len(evs) == 2 &&
			evs[0].kind == hbAcquire && inRW(evs[0]) &&
			evs[1].kind == hbAcquire && inRW(evs[1]) &&
			evs[0].id != evs[1].id)

		b = dstSyncEventLenFP()
		rw.Unlock()
		evs = hbEventsSince(b)
		emit("rwUnlock", len(evs) == 1 && evs[0].kind == hbRelease && inRW(evs[0]))

		b = dstSyncEventLenFP()
		rw.RLock()
		evs = hbEventsSince(b)
		emit("rwRLock", len(evs) == 1 && evs[0].kind == hbAcquire && inRW(evs[0]))

		b = dstSyncEventLenFP()
		rw.RUnlock()
		evs = hbEventsSince(b)
		emit("rwRUnlock", len(evs) == 1 && evs[0].kind == hbRelease && inRW(evs[0]))

		// TryLock success records an acquire at its own call site (a different
		// line than Lock's fast path — covered separately so dropping either
		// record fails a named field); a failed TryLock records nothing.
		var mu3 sync.Mutex
		mu3ID := uintptr(unsafe.Pointer(&mu3))
		b = dstSyncEventLenFP()
		ok := mu3.TryLock()
		evs = hbEventsSince(b)
		emit("tryLockPair", ok && len(evs) == 1 &&
			evs[0].kind == hbAcquire && evs[0].id == mu3ID)

		// Channel ops record on the hchan identity (positive control for the
		// ignoredChan case below): a buffered send then receive record an
		// acquire+release pair each, all on &ch.
		ch := make(chan int, 1)
		chID := *(*uintptr)(unsafe.Pointer(&ch))
		b = dstSyncEventLenFP()
		ch <- 1
		<-ch
		evs = hbEventsSince(b)
		chOK := len(evs) == 4
		for _, e := range evs {
			chOK = chOK && e.id == chID
		}
		emit("chanPair", chOK)

		// RaceDisable suppresses channel and atomic HB records too: the
		// raceignore check sits at the dstRecordSyncEventForGID choke point,
		// not per-bridge, so every recorder path mirrors TSan's ignore state.
		ch2 := make(chan int, 1)
		b = dstSyncEventLenFP()
		runtime.RaceDisable()
		ch2 <- 1
		<-ch2
		runtime.RaceEnable()
		emit("ignoredChan", dstSyncEventLenFP() == b)

		var a32 int32
		b = dstSyncEventLenFP()
		runtime.RaceDisable()
		atomic.AddInt32(&a32, 1)
		runtime.RaceEnable()
		emit("ignoredAtomic", dstSyncEventLenFP() == b)

		return true
	}
	res := simulation.Explore(dstSeedEnv(), simulation.DPOR, sut)

	// Contended scenario, asserted across EVERY schedule: a child Lock that
	// DPOR orders before the root's Unlock blocks and records its acquire in
	// lockSlow's tail — a different call site than the fast path, invisible to
	// outcome-based tests (HB records only prune). Exactly 2 acquires and 2
	// releases on the mutex identity must appear in every schedule, whichever
	// path the child took.
	contendedOK := true
	csut := func() bool {
		var mu sync.Mutex
		muID := uintptr(unsafe.Pointer(&mu))
		done := make(chan struct{})
		b := dstSyncEventLenFP()
		mu.Lock()
		go func() {
			mu.Lock()
			mu.Unlock()
			close(done)
		}()
		mu.Unlock()
		<-done
		acq, rel := 0, 0
		for _, e := range hbEventsSince(b) {
			if e.id != muID {
				continue // channel events on done
			}
			if e.kind == hbAcquire {
				acq++
			} else if e.kind == hbRelease {
				rel++
			}
		}
		if acq != 2 || rel != 2 {
			contendedOK = false
		}
		return true
	}
	cres := simulation.Explore(dstSeedEnv(), simulation.DPOR, csut)

	// >= 2 schedules guards the scenario against GLOBAL exploration vacuity
	// (DPOR collapsing to a single schedule). It is a proxy, not a proof that
	// the blocking path ran: the done channel's close/recv announces are also
	// a reorderable pair, so a mutex-announce-specific regression could
	// sustain >=2 schedules on chan reorders alone while every mutex Lock
	// takes the fast path. No cheap stream-level discriminator exists (fast
	// and woken paths record the identical acquire), so the lockSlow-tail
	// coverage ultimately rests on mutex announces staying live — which
	// TestDSTExploreSyncAutoInstrument enforces independently.
	os.Stdout.WriteString("synchbsuppress\n")
	os.Stdout.WriteString(report +
		"contended=" + strconv.FormatBool(contendedOK && cres.Schedules >= 2) +
		" exhausted=" + strconv.FormatBool(res.Exhausted && !res.Overflow &&
		cres.Exhausted && !cres.Overflow) + "\n")
}
