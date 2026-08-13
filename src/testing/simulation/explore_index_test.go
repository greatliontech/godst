// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

// The indexed conflict passes (explore_index.go) claim EXACT equivalence with
// the all-pairs scans they replaced: identical concurrent-conflicting pair
// sequence, identical trace clocks. These tests pin that claim against
// reference copies of the replaced scans, over randomized traces that exercise
// the index's structural branches (page straddling, large-span entries,
// full-scan queries, zero-address accesses, HB-ordered pairs) and over
// targeted anchor cases for each branch. They are pure offline-function tests:
// no simulation bubble, no build-tag requirement.

import (
	"math/rand"
	"reflect"
	"testing"
)

// accPair is one enumerated pair, for comparing sequences.
type accPair struct {
	i, j int32
}

// collectConflictPairs materializes forEachConcurrentConflictingPair's
// streamed sequence for comparison.
func collectConflictPairs(tr exploreTrace, clk [][]uint32, pidx map[uint64]int) []accPair {
	var pairs []accPair
	forEachConcurrentConflictingPair(tr, clk, pidx, func(i, j int) {
		pairs = append(pairs, accPair{i: int32(i), j: int32(j)})
	})
	return pairs
}

// referenceConflictPairs is the all-pairs scan the streaming enumeration
// replaced: j ascending, i descending, accessConflict && dporConcurrent.
func referenceConflictPairs(tr exploreTrace, clk [][]uint32, pidx map[uint64]int) []accPair {
	var pairs []accPair
	for j := 0; j < len(tr.accSeq); j++ {
		for i := j - 1; i >= 0; i-- {
			if accessConflict(tr, i, j) && dporConcurrent(clk, pidx, tr, i, j) {
				pairs = append(pairs, accPair{i: int32(i), j: int32(j)})
			}
		}
	}
	return pairs
}

// referenceTraceClocks is dporTraceClocks with the all-pairs conflict-edge
// scan it carried before the index (merge every earlier conflicting access).
func referenceTraceClocks(tr exploreTrace) (clk [][]uint32, pidx map[uint64]int) {
	pidx = make(map[uint64]int)
	addProc := func(p uint64) {
		if _, ok := pidx[p]; !ok {
			pidx[p] = len(pidx)
		}
	}
	for _, p := range tr.accSeq {
		addProc(p)
	}
	for i := range tr.edgeFrom {
		addProc(tr.edgeFrom[i])
		addProc(tr.edgeTo[i])
	}
	for _, p := range tr.syncSeq {
		addProc(p)
	}
	P := len(pidx)
	cur := make([][]uint32, P)
	for i := range cur {
		cur[i] = make([]uint32, P)
	}
	mergeInto := func(dst, src []uint32) {
		for k := 0; k < P; k++ {
			if src[k] > dst[k] {
				dst[k] = src[k]
			}
		}
	}
	events := orderedHBEvents(tr)
	eventIdx := 0
	objClk := map[syncObjectKey][]uint32{}
	objectClock := func(obj syncObjectKey) []uint32 {
		clk := objClk[obj]
		if clk == nil {
			clk = make([]uint32, P)
			objClk[obj] = clk
		}
		return clk
	}
	applyEvents := func(step, accLimit int) {
		for eventIdx < len(events) && (events[eventIdx].step < step || events[eventIdx].step == step && events[eventIdx].acc <= accLimit) {
			ev := events[eventIdx]
			switch ev.kind {
			case hbEventReady:
				mergeInto(cur[pidx[ev.to]], cur[pidx[ev.from]])
			case hbEventSyncRelease:
				mergeInto(objectClock(ev.obj), cur[pidx[ev.seq]])
			case hbEventSyncAcquire:
				mergeInto(cur[pidx[ev.seq]], objectClock(ev.obj))
			}
			eventIdx++
		}
	}
	maxStep := maxHBStep(tr)
	nLog := len(tr.accSeq)
	clk = make([][]uint32, nLog)
	li := 0
	for s := 0; s <= maxStep; s++ {
		applyEvents(s, li)
		for li < nLog && tr.accStep[li] == s {
			applyEvents(s, li)
			pi := pidx[tr.accSeq[li]]
			for m := 0; m < li; m++ {
				if accessConflict(tr, m, li) {
					mergeInto(cur[pi], clk[m])
				}
			}
			cur[pi][pi]++
			clk[li] = append([]uint32(nil), cur[pi]...)
			li++
			applyEvents(s, li)
		}
		applyEvents(s, li)
	}
	return clk, pidx
}

// genTrace builds a random well-formed trace: accStep non-decreasing, ready
// edges and sync events in a shared monotone HB order, addresses drawn to hit
// the index's branches (hot exact addresses, page-straddling ranges,
// large-span entries, full-scan-width queries, zero addresses).
func genTrace(rng *rand.Rand, n int) exploreTrace {
	var tr exploreTrace
	procs := 1 + rng.Intn(4)
	const base = uintptr(0x10000)
	hot := make([]uintptr, 1+rng.Intn(4))
	for i := range hot {
		hot[i] = base + uintptr(rng.Intn(1<<12))
	}
	step := 0
	hbOrder := 0
	accs := 0
	for len(tr.accSeq) < n {
		switch rng.Intn(10) {
		case 0: // ready edge
			tr.edgeFrom = append(tr.edgeFrom, uint64(1+rng.Intn(procs)))
			tr.edgeTo = append(tr.edgeTo, uint64(1+rng.Intn(procs)))
			tr.edgeStep = append(tr.edgeStep, step)
			tr.edgeAcc = append(tr.edgeAcc, accs)
			tr.edgeOrder = append(tr.edgeOrder, hbOrder)
			hbOrder++
		case 1: // sync release/acquire on a small object pool
			kind := uint8(syncEventRelease)
			if rng.Intn(2) == 0 {
				kind = syncEventAcquire
			}
			tr.syncKind = append(tr.syncKind, kind)
			tr.syncID = append(tr.syncID, base+uintptr(rng.Intn(3))*64)
			tr.syncAux = append(tr.syncAux, uintptr(rng.Intn(2)))
			tr.syncSeq = append(tr.syncSeq, uint64(1+rng.Intn(procs)))
			tr.syncStep = append(tr.syncStep, step)
			tr.syncAcc = append(tr.syncAcc, accs)
			tr.syncOrd = append(tr.syncOrd, hbOrder)
			hbOrder++
		default: // access
			var addr, size uintptr
			switch rng.Intn(12) {
			case 0:
				addr, size = 0, 0 // no conflict identity
			case 1: // page-straddling range
				addr = base + uintptr(rng.Intn(8))*256 + 250
				size = uintptr(8 + rng.Intn(16))
			case 2: // large-span entry (> accIdxMaxSpan pages)
				addr = base + uintptr(rng.Intn(4))*4096
				size = uintptr((accIdxMaxSpan + 1 + rng.Intn(4)) * 256)
			case 3: // full-scan-width query (> accIdxMaxQuery pages)
				addr = base
				size = uintptr((accIdxMaxQuery + 1) * 256)
			case 4, 5: // cold exact address
				addr = base + uintptr(rng.Intn(1<<14))
				size = uintptr(1 + rng.Intn(8))
			case 6: // size 0 with a real address: normalized to 1 byte
				addr = base + uintptr(rng.Intn(1<<10))
				size = 0
			default: // hot exact address
				addr = hot[rng.Intn(len(hot))]
				size = uintptr(1 << rng.Intn(4))
			}
			tr.accSeq = append(tr.accSeq, uint64(1+rng.Intn(procs)))
			tr.accAddr = append(tr.accAddr, addr)
			tr.accSize = append(tr.accSize, size)
			tr.accPC = append(tr.accPC, uintptr(rng.Intn(64)))
			tr.accCount = append(tr.accCount, uint64(rng.Intn(4)))
			tr.accWrite = append(tr.accWrite, rng.Intn(2) == 0)
			tr.accStep = append(tr.accStep, step)
			accs++
		}
		if rng.Intn(3) == 0 {
			step++
		}
	}
	return tr
}

func TestExploreIndexPairsMatchAllPairs(t *testing.T) {
	for seed := int64(0); seed < 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		tr := genTrace(rng, 20+rng.Intn(280))
		clk, pidx := dporClocks(tr)
		want := referenceConflictPairs(tr, clk, pidx)
		got := collectConflictPairs(tr, clk, pidx)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: indexed pair sequence diverges from the all-pairs scan\n got %v\nwant %v", seed, got, want)
		}
	}
}

func TestExploreIndexTraceClocksMatchAllPairs(t *testing.T) {
	for seed := int64(0); seed < 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		tr := genTrace(rng, 20+rng.Intn(280))
		wantClk, wantPidx := referenceTraceClocks(tr)
		gotClk, gotPidx := dporTraceClocks(tr)
		if !reflect.DeepEqual(gotPidx, wantPidx) {
			t.Fatalf("seed %d: pidx diverges", seed)
		}
		if !reflect.DeepEqual(gotClk, wantClk) {
			t.Fatalf("seed %d: indexed trace clocks diverge from the all-pairs scan", seed)
		}
	}
}

// anchorTrace lays out two goroutines with hand-placed intervals hitting the
// named structural branch, all in one step window (no HB events: every
// cross-goroutine pair is concurrent).
func anchorTrace(acc []struct {
	seq   uint64
	addr  uintptr
	size  uintptr
	write bool
}) exploreTrace {
	var tr exploreTrace
	for _, a := range acc {
		tr.accSeq = append(tr.accSeq, a.seq)
		tr.accAddr = append(tr.accAddr, a.addr)
		tr.accSize = append(tr.accSize, a.size)
		tr.accPC = append(tr.accPC, 1)
		tr.accCount = append(tr.accCount, 1)
		tr.accWrite = append(tr.accWrite, a.write)
		tr.accStep = append(tr.accStep, 0)
	}
	return tr
}

func TestExploreIndexAnchorCases(t *testing.T) {
	type acc = struct {
		seq   uint64
		addr  uintptr
		size  uintptr
		write bool
	}
	const base = uintptr(0x20000)
	cases := []struct {
		name string
		accs []acc
	}{
		{"cross-page overlap, differing start pages", []acc{
			{1, base + 250, 12, true}, // straddles into the next page
			{2, base + 258, 1, false}, // next page only
		}},
		{"adjacent intervals do not conflict", []acc{
			{1, base, 8, true},
			{2, base + 8, 8, true},
		}},
		{"multi-shared-page pair emitted once", []acc{
			{1, base + 100, 700, true}, // covers 3+ pages
			{2, base + 200, 700, true}, // shares at least 2 pages with it
		}},
		{"large-span entry found by later exact access", []acc{
			{1, base, (accIdxMaxSpan + 2) * 256, true},
			{2, base + 1024, 8, false},
		}},
		{"full-scan query finds indexed entries", []acc{
			{1, base + 512, 8, true},
			{2, base, (accIdxMaxQuery + 2) * 256, false},
		}},
		{"read-read pairs commute", []acc{
			{1, base, 8, false},
			{2, base, 8, false},
			{1, base, 8, true}, // the write pairs with the earlier foreign read
		}},
		{"same goroutine never pairs", []acc{
			{1, base, 8, true},
			{1, base, 8, true},
		}},
		{"zero address has no identity", []acc{
			{1, 0, 0, true},
			{2, 0, 0, true},
			{2, base, 8, true},
		}},
		{"size zero with a real address is a one-byte interval", []acc{
			{1, base, 0, true},
			{2, base, 1, true},
			{2, base + 1, 1, true}, // adjacent: no overlap with the 1-byte norm
		}},
	}
	for _, tc := range cases {
		tr := anchorTrace(tc.accs)
		clk, pidx := dporClocks(tr)
		want := referenceConflictPairs(tr, clk, pidx)
		got := collectConflictPairs(tr, clk, pidx)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: indexed pairs %v, all-pairs %v", tc.name, got, want)
		}
		wantClk, _ := referenceTraceClocks(tr)
		gotClk, _ := dporTraceClocks(tr)
		if !reflect.DeepEqual(gotClk, wantClk) {
			t.Errorf("%s: indexed trace clocks diverge", tc.name)
		}
	}
}

// TestExploreIndexHBOrderedPairExcluded pins the concurrency gate: a ready
// edge ordering two conflicting accesses removes the pair for both the
// reference and the index (the pair set is empty, not merely equal).
func TestExploreIndexHBOrderedPairExcluded(t *testing.T) {
	var tr exploreTrace
	tr.accSeq = []uint64{1, 2}
	tr.accAddr = []uintptr{0x30000, 0x30000}
	tr.accSize = []uintptr{8, 8}
	tr.accPC = []uintptr{1, 1}
	tr.accCount = []uint64{1, 1}
	tr.accWrite = []bool{true, true}
	tr.accStep = []int{0, 1}
	tr.edgeFrom = []uint64{1}
	tr.edgeTo = []uint64{2}
	tr.edgeStep = []int{0}
	tr.edgeAcc = []int{1}
	tr.edgeOrder = []int{0}
	clk, pidx := dporClocks(tr)
	if got := collectConflictPairs(tr, clk, pidx); len(got) != 0 {
		t.Fatalf("HB-ordered conflicting pair not excluded: %v", got)
	}
	if want := referenceConflictPairs(tr, clk, pidx); len(want) != 0 {
		t.Fatalf("reference disagrees with the memory model: %v", want)
	}
}

// genDenseTrace is the race-dense worst case: every access a write to ONE
// address, no HB events — every cross-goroutine pair is concurrent and
// conflicting, so the pair set is quadratic in the log and each access's
// concurrency window is its whole prefix. The shape that stresses the
// streamed enumeration's per-window costs (sort, candidate buffer).
func genDenseTrace(n, procs int) exploreTrace {
	rng := rand.New(rand.NewSource(7))
	var tr exploreTrace
	step := 0
	for len(tr.accSeq) < n {
		tr.accSeq = append(tr.accSeq, uint64(1+rng.Intn(procs)))
		tr.accAddr = append(tr.accAddr, 0x40000)
		tr.accSize = append(tr.accSize, 8)
		tr.accPC = append(tr.accPC, 1)
		tr.accCount = append(tr.accCount, 1)
		tr.accWrite = append(tr.accWrite, true)
		tr.accStep = append(tr.accStep, step)
		if rng.Intn(2) == 0 {
			step++
		}
	}
	return tr
}

// TestExploreIndexDenseWindowMatches pins equivalence on the race-dense
// shape, where the concurrency window approaches the whole prefix (the
// randomized generator's sync events keep windows narrow, so this shape needs
// its own pin). Scope: the sort/window path. With no HB events every foreign
// ordinal bound is 0, so the binary search's ord==bound cut is degenerate
// here — that boundary is pinned by the randomized traces' sync chains and
// TestExploreIndexHBOrderedPairExcluded.
func TestExploreIndexDenseWindowMatches(t *testing.T) {
	tr := genDenseTrace(600, 3)
	clk, pidx := dporClocks(tr)
	want := referenceConflictPairs(tr, clk, pidx)
	got := collectConflictPairs(tr, clk, pidx)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("dense trace: indexed pair sequence diverges from the all-pairs scan")
	}
	wantClk, _ := referenceTraceClocks(tr)
	gotClk, _ := dporTraceClocks(tr)
	if !reflect.DeepEqual(gotClk, wantClk) {
		t.Fatal("dense trace: indexed trace clocks diverge from the all-pairs scan")
	}
}

// genStoreShapedTrace approximates a store-sized SUT's log: many goroutines,
// accesses spread over thousands of addresses with a mutex-serialized shared
// core (sync release/acquire chains keep concurrency windows narrow), sized at
// the ~65k accesses the field defect recorded.
func genStoreShapedTrace(n int) exploreTrace {
	rng := rand.New(rand.NewSource(42))
	var tr exploreTrace
	const procs = 8
	const base = uintptr(0x100000)
	step := 0
	hbOrder := 0
	releaseSeen := false
	for len(tr.accSeq) < n {
		seq := uint64(1 + rng.Intn(procs))
		// Release/acquire chains on one shared object keep most
		// cross-goroutine history HB-ordered — the store shape (narrow
		// concurrency windows over a mutex-serialized core).
		if rng.Intn(16) == 0 {
			kind := uint8(syncEventAcquire)
			if !releaseSeen || rng.Intn(2) == 0 {
				kind = syncEventRelease
				releaseSeen = true
			}
			tr.syncKind = append(tr.syncKind, kind)
			tr.syncID = append(tr.syncID, base)
			tr.syncAux = append(tr.syncAux, 0)
			tr.syncSeq = append(tr.syncSeq, seq)
			tr.syncStep = append(tr.syncStep, step)
			tr.syncAcc = append(tr.syncAcc, len(tr.accSeq))
			tr.syncOrd = append(tr.syncOrd, hbOrder)
			hbOrder++
			continue
		}
		var addr uintptr
		if rng.Intn(20) == 0 {
			addr = base + uintptr(rng.Intn(16))*8 // shared core
		} else {
			addr = base + 0x10000 + uintptr(rng.Intn(1<<14))*8 // spread
		}
		tr.accSeq = append(tr.accSeq, seq)
		tr.accAddr = append(tr.accAddr, addr)
		tr.accSize = append(tr.accSize, 8)
		tr.accPC = append(tr.accPC, uintptr(rng.Intn(256)))
		tr.accCount = append(tr.accCount, uint64(rng.Intn(8)))
		tr.accWrite = append(tr.accWrite, rng.Intn(3) == 0)
		tr.accStep = append(tr.accStep, step)
		if rng.Intn(4) == 0 {
			step++
		}
	}
	return tr
}

// The *AllPairs reference arms ARE the measured defect: quadratic in the 65k
// log, minutes per iteration. They exist for the before/after record; run
// them deliberately (-bench with -benchtime=1x), not casually.

func BenchmarkConflictPairsIndexed(b *testing.B) {
	tr := genStoreShapedTrace(65000)
	clk, pidx := dporClocks(tr)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		collectConflictPairs(tr, clk, pidx)
	}
}

func BenchmarkConflictPairsAllPairs(b *testing.B) {
	tr := genStoreShapedTrace(65000)
	clk, pidx := dporClocks(tr)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		referenceConflictPairs(tr, clk, pidx)
	}
}

func BenchmarkTraceClocksIndexed(b *testing.B) {
	tr := genStoreShapedTrace(65000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dporTraceClocks(tr)
	}
}

func BenchmarkTraceClocksAllPairs(b *testing.B) {
	tr := genStoreShapedTrace(65000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		referenceTraceClocks(tr)
	}
}

// Dense arms: the output itself is quadratic (every cross-goroutine pair
// emitted), so this compares the SHIPPED streamed shape against the REPLACED
// materializing scan on the output-bound floor — the reference arm's cost
// includes its pair-slice churn, which the streamed design exists to avoid.

func BenchmarkConflictPairsDenseIndexed(b *testing.B) {
	tr := genDenseTrace(8192, 3)
	clk, pidx := dporClocks(tr)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		forEachConcurrentConflictingPair(tr, clk, pidx, func(i, j int) {})
	}
}

func BenchmarkConflictPairsDenseAllPairs(b *testing.B) {
	tr := genDenseTrace(8192, 3)
	clk, pidx := dporClocks(tr)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		referenceConflictPairs(tr, clk, pidx)
	}
}
