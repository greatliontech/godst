// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && (unix || (js && wasm) || wasip1)

package os

import (
	"runtime"
	"sync"
	"sync/atomic"
	"weak"

	_ "unsafe" // for go:linkname
)

// The DST per-object state tables hold os.file's simulated backing and
// virtual-fd map (and, in dst_root.go, os.root's simulated root) OUT OF
// LINE, so both types keep upstream's exact shape in every build mode
// (design.md, "Untagged footprint (contract)", the type-shape clause).
// The row index rides the object's existing descriptor slot — a value,
// never a field: dstNewFile and dstNewRoot already park the sentinel -1
// in pfd.Sysfd and root.fd (a simulated object has no host descriptor),
// and every index encodes below it as -2-idx, still negative, so every
// not-yet-gated path that would have failed EBADF on -1 fails EBADF
// identically. The lookup is one field load and compare (host objects:
// nothing else) plus a spine load and two dependent loads (simulated
// objects) — the out-of-line price on the simulation's per-operation
// path, measured against the in-record field by the SimFile benchmarks
// beside the suite.
//
// Row lifetime equals what the fields' was, exactly: a row is created
// before the object escapes its constructor, survives Close (a closed
// simulated handle still answers through its backend, so
// read-after-close stays production-shaped, and a leaked handle from a
// dead run keeps its backend so the dead-run gates refuse it by name
// instead of the handle decaying into a host EBADF), and is reclaimed
// only after the OBJECT is collected: each row carries a weak backref
// to its object, and the sweep releases exactly the rows whose backref
// died. No callback rides the run-scoped cleanup/finalizer channel —
// that channel discards a dead simulated process's callbacks by
// design, which would leak every row (and its pinned backend graph)
// a simulated process registered. The sweep runs at RUN TEARDOWN only
// (dstFSRunTeardown — host context, after the bubble has exited):
// weak.Value can park its goroutine on an in-flight GC's mark
// termination, which inside a bubble would be a wait on a
// nondeterministic event under the table mutex, so no in-bubble path
// (register, the epoch roll) ever sweeps. The recorded bound: in-run
// growth is at most the run's registrations — negligible beyond the
// run's own footprint, since a row is two words plus state whose
// backend graph the run's tree pins until teardown regardless — and a
// row is reclaimed at the first teardown after its object's
// collection, so growth across runs comes only from objects still
// referenced or not yet collected.
//
// Free-list index reuse is sound because a row is released only when
// its object has been COLLECTED — no reachable object can carry the
// slot — and every in-flight operation pins its object across the
// derived-index window (runtime.KeepAlive at the accessors and across
// dstFD): a derived index alone must never outlive its object's
// liveness, or a recycled slot would read another object's row.
//
// Storage is an immutable spine of fixed chunks: the spine grows
// copy-on-write under the mutex and chunks never move, so a reader
// needs no lock — an atomic spine load, a chunk index, an atomic cell
// load. Writers (register, sweep) serialize on the mutex.
const dstStateChunk = 512

type dstStateRow[T any, S any] struct {
	obj   weak.Pointer[T]
	state S
}

type dstStateChunkArr[T any, S any] [dstStateChunk]atomic.Pointer[dstStateRow[T, S]]

type dstStateTable[T any, S any] struct {
	mu    sync.Mutex
	spine atomic.Pointer[[]*dstStateChunkArr[T, S]]
	next  int
	free  []int
}

// register stores state in a fresh row backref'd to obj and returns the
// row's index.
func (t *dstStateTable[T, S]) register(obj *T, state S) int {
	row := &dstStateRow[T, S]{obj: weak.Make(obj), state: state}
	t.mu.Lock()
	var idx int
	if n := len(t.free); n > 0 {
		idx = t.free[n-1]
		t.free = t.free[:n-1]
	} else {
		idx = t.next
		t.next++
		if spine := t.spine.Load(); spine == nil || idx/dstStateChunk >= len(*spine) {
			var old []*dstStateChunkArr[T, S]
			if spine != nil {
				old = *spine
			}
			grown := make([]*dstStateChunkArr[T, S], len(old)+1)
			copy(grown, old)
			grown[len(old)] = new(dstStateChunkArr[T, S])
			t.spine.Store(&grown)
		}
	}
	(*t.spine.Load())[idx/dstStateChunk][idx%dstStateChunk].Store(row)
	t.mu.Unlock()
	return idx
}

// get returns the state at idx, or nil for a released row or an index
// the table never issued.
func (t *dstStateTable[T, S]) get(idx int) *S {
	spine := t.spine.Load()
	if spine == nil || idx < 0 || idx/dstStateChunk >= len(*spine) {
		return nil
	}
	row := (*spine)[idx/dstStateChunk][idx%dstStateChunk].Load()
	if row == nil {
		return nil
	}
	return &row.state
}

// sweep releases every row whose object has been collected. Host
// context only: weak.Value can park on an in-flight GC (see the
// package comment above).
func (t *dstStateTable[T, S]) sweep() {
	t.mu.Lock()
	t.sweepLocked()
	t.mu.Unlock()
}

func (t *dstStateTable[T, S]) sweepLocked() {
	spine := t.spine.Load()
	if spine == nil {
		return
	}
	for idx := 0; idx < t.next; idx++ {
		cell := &(*spine)[idx/dstStateChunk][idx%dstStateChunk]
		row := cell.Load()
		if row == nil || row.obj.Value() != nil {
			continue
		}
		cell.Store(nil)
		t.free = append(t.free, idx)
	}
}

// dstStateIndex converts between a descriptor-slot value and a table
// index: simulated objects park -2-idx below the -1 sentinel.
func dstStateIndex(slot int) (int, bool) {
	if slot > -2 {
		return 0, false
	}
	return -2 - slot, true
}

func dstStateSlot(idx int) int { return -2 - idx }

type dstFileState struct {
	backend dstFileBackend
	// fds is the file's virtual-descriptor registration map. Both the
	// map field and its entries are mutated only under
	// dstFDRegistry.mu, exactly as the former struct field was; the
	// state pointer itself is stable from creation.
	fds map[dstFDKey]int
}

var dstFileStates dstStateTable[file, dstFileState]

// dstRootStates is the root twin (dst_root.go); declared here so the
// teardown sweep below covers every platform this file builds on.
var dstRootStates dstStateTable[root, dstRoot]

// dstStateTablesSweep reclaims every collected object's row, files and
// roots alike. Called by dstFSRunTeardown — host context, bubble dead.
func dstStateTablesSweep() {
	dstFileStates.sweep()
	dstRootStates.sweep()
}

// dstFileStateStats reports the file table's high-water index and
// free-list length. Test-only linkname: anti-vacuity teeth for the
// teardown-sweep test in testing/simulation.
//
//go:linkname dstFileStateStats
func dstFileStateStats() (next, free int) {
	dstFileStates.mu.Lock()
	defer dstFileStates.mu.Unlock()
	return dstFileStates.next, len(dstFileStates.free)
}

// dstSetFileBackend attaches a simulated backing to a newly built
// file; dstNewFile calls it before the *File escapes. The row index
// lands in pfd.Sysfd, below the -1 sentinel.
func dstSetFileBackend(f *file, b dstFileBackend) {
	f.pfd.Sysfd = dstStateSlot(dstFileStates.register(f, dstFileState{backend: b}))
}

func dstFileStateOf(f *file) *dstFileState {
	idx, ok := dstStateIndex(f.pfd.Sysfd)
	if !ok {
		return nil
	}
	s := dstFileStates.get(idx)
	// The object, not the derived index, is the row's lifetime anchor:
	// keep it reachable past the lookup so the row cannot be swept and
	// its slot recycled mid-derivation.
	runtime.KeepAlive(f)
	return s
}

// dstBackendOf is a free function, deliberately not a method: a method
// on file would enter the type's — and, promoted, os.File's — declared
// method set, exactly the analyzer-observable shape change the
// type-shape clause forbids (design.md, INV-TYPESHAPE).
func dstBackendOf(f *file) dstFileBackend {
	if s := dstFileStateOf(f); s != nil {
		return s.backend
	}
	return nil
}
