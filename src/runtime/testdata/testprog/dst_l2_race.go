// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build race

// White-box fixture for the DST HB shadow's raceignore mirror: the HB-record
// bridges honor g.raceignore exactly as raceacquireg/racereleaseg do, so the
// recorded sync-event stream must contain the public sync-primitive events and
// ONLY those — no embedded writer-mutex events from inside RWMutex internals
// (suppressed by upstream's race.Disable brackets), and nothing at all from a
// mutex pair the SUT itself brackets with runtime.RaceDisable. Outcome-based
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

		return true
	}
	res := simulation.Explore(dstSeedEnv(), simulation.DPOR, sut)
	os.Stdout.WriteString("synchbsuppress\n")
	os.Stdout.WriteString(report + "exhausted=" +
		strconv.FormatBool(res.Exhausted && !res.Overflow) + "\n")
}
