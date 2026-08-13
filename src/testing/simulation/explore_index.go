// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

// Indexed conflict detection for the offline DPOR access passes. The dependency
// relation pairs only accesses whose byte intervals overlap, so the passes that
// consume it — the concurrent-conflicting-pair enumeration (source-set race
// analysis, replay-force promotion) and dporTraceClocks' conflict edges — need
// only each access's PER-ADDRESS history, never the whole log. These indexes
// bucket the log by the 256-byte pages an interval covers (mirroring the
// runtime's live-filter page index, runtime/dst_explore.go) and consult
// same-page histories instead of scanning all prior accesses: the all-pairs
// scans are quadratic in the log and a store-sized SUT records ~65k accesses
// per schedule, which put minutes inside a single pass.
//
// Both indexes are EXACT, not approximate — the enumerated pair set, its
// processing order, and the computed clocks are identical to the all-pairs
// scans', so the completeness (DST-L2-3) and determinism (DST-L2-2) arguments
// carry over unchanged:
//
//   - Concurrency needs no per-pair clock test. In dporClocks' vector clocks an
//     access's own component is its per-goroutine ordinal (it ticks exactly
//     once per own access, and cross-goroutine merges cannot raise a
//     component above its owner's tick count), and for i < j in commit order
//     "j happens-before i" is impossible (j's tick does not exist at i's
//     snapshot). So for cross-goroutine i < j: concurrent(i, j) iff
//     ord(i) > clk[j][pidx(seq_i)] — a goroutine's accesses concurrent with j
//     are a contiguous TAIL of its history, located by one ordinal comparison
//     (binary search), never a per-pair HB test.
//
//   - dporTraceClocks' conflict edges need only the LAST conflicting access
//     per goroutine: same-goroutine clock snapshots are monotone (cur only
//     grows), so merging the last conflicting access's clock is merge-identical
//     to merging every earlier same-goroutine conflicting clock.
//
// Interval handling: an access spanning more than accIdxMaxSpan pages is held
// on a small always-consulted large list instead of being indexed under every
// page; a QUERY spanning more than accIdxMaxQuery pages falls back to the exact
// full scan of the prior log. Range accesses are few (the compiler emits them
// for composite copies), so both fallbacks stay off the hot path.

import "sort"

const (
	accIdxPageShift = 8  // 256-byte pages, as the runtime's live-filter index
	accIdxMaxSpan   = 8  // pages an access is indexed under before the large list
	accIdxMaxQuery  = 64 // pages a query walks before falling back to the full scan
)

func accIdxPages(addr, size uintptr) (start, end uintptr) {
	return addr >> accIdxPageShift, (accessRangeEnd(addr, size) - 1) >> accIdxPageShift
}

// accPairEntry is one indexed access: its log index and its goroutine's
// per-goroutine ordinal (own vector-clock component), ascending in both within
// a history. int32 log indices are sound because the access log is capped at
// exploreMaxAccesses (1<<16) — ExploreOptions.MaxSteps raises only the
// decision budget, never the access-log capacity.
type accPairEntry struct {
	log int32
	ord uint32
}

// accPairHist is one goroutine's access history on one page.
type accPairHist struct {
	seq     uint64
	entries []accPairEntry
}

// accPairPage holds the per-goroutine histories of one page, in first-touch
// order (deterministic: commit order is deterministic under a schedule).
type accPairPage struct {
	procs []*accPairHist
}

// accPairIndex indexes accesses for concurrent-conflicting-pair enumeration.
type accPairIndex struct {
	pages map[uintptr]*accPairPage
	large []int32 // log indices of accesses spanning > accIdxMaxSpan pages, ascending
}

func (ix *accPairIndex) add(tr exploreTrace, k int, ord uint32) {
	addr := tr.accAddr[k]
	if addr == 0 {
		return // no conflict identity; can never pair
	}
	start, end := accIdxPages(addr, accessSize(tr, k))
	if end-start >= accIdxMaxSpan {
		ix.large = append(ix.large, int32(k))
		return
	}
	seq := tr.accSeq[k]
	for p := start; p <= end; p++ {
		pg := ix.pages[p]
		if pg == nil {
			pg = &accPairPage{}
			ix.pages[p] = pg
		}
		var h *accPairHist
		for _, ph := range pg.procs {
			if ph.seq == seq {
				h = ph
				break
			}
		}
		if h == nil {
			h = &accPairHist{seq: seq}
			pg.procs = append(pg.procs, h)
		}
		h.entries = append(h.entries, accPairEntry{log: int32(k), ord: ord})
	}
}

// appendConcurrentConflicting appends to dst the log indices i < j that
// conflict with j (overlapping intervals, >=1 write, different goroutine) and
// are concurrent with it under clk (dporClocks' sync HB) — exactly the pairs
// the all-pairs scan `accessConflict && dporConcurrent` admits. Unsorted;
// each index appears once (a pair spanning several shared pages is emitted
// only at the first page both intervals cover).
func (ix *accPairIndex) appendConcurrentConflicting(dst []int, tr exploreTrace, clk [][]uint32, pidx map[uint64]int, j int) []int {
	jAddr := tr.accAddr[j]
	if jAddr == 0 {
		return dst
	}
	jSize := accessSize(tr, j)
	jSeq := tr.accSeq[j]
	start, end := accIdxPages(jAddr, jSize)
	if end-start >= accIdxMaxQuery {
		// A range so large that walking its pages costs more than the exact
		// full scan of the prior log: same semantics, rare.
		for i := 0; i < j; i++ {
			if accessConflict(tr, i, j) && dporConcurrent(clk, pidx, tr, i, j) {
				dst = append(dst, i)
			}
		}
		return dst
	}
	for p := start; p <= end; p++ {
		pg := ix.pages[p]
		if pg == nil {
			continue
		}
		for _, h := range pg.procs {
			if h.seq == jSeq {
				continue
			}
			bound := clk[j][pidx[h.seq]]
			// First entry with ord > bound: everything at or below the bound
			// happens-before j (ordinal argument above); the rest is the
			// concurrent tail.
			lo, hi := 0, len(h.entries)
			for lo < hi {
				mid := int(uint(lo+hi) >> 1)
				if h.entries[mid].ord > bound {
					hi = mid
				} else {
					lo = mid + 1
				}
			}
			for _, e := range h.entries[lo:] {
				i := int(e.log)
				if !accessConflict(tr, i, j) {
					continue
				}
				// Emit a multi-shared-page pair only at the FIRST common page,
				// so each pair appears exactly once (as in the all-pairs scan).
				if fp, _ := accIdxPages(tr.accAddr[i], accessSize(tr, i)); max(fp, start) != p {
					continue
				}
				dst = append(dst, i)
			}
		}
	}
	for _, l := range ix.large {
		i := int(l)
		if !accessConflict(tr, i, j) {
			continue
		}
		pi := pidx[tr.accSeq[i]]
		if clk[j][pi] >= clk[i][pi] {
			continue // i happens-before j
		}
		dst = append(dst, i)
	}
	return dst
}

// forEachConcurrentConflictingPair streams every concurrent conflicting pair
// of access-log entries to visit, in the all-pairs scan's order: j ascending,
// i descending within each j. clk/pidx are dporClocks' sync-HB clocks for tr.
// Streaming, never materialized: a race-dense trace's pair set is quadratic in
// the log, and only the per-j candidate buffer (bounded by j's concurrency
// window) is held.
func forEachConcurrentConflictingPair(tr exploreTrace, clk [][]uint32, pidx map[uint64]int, visit func(i, j int)) {
	ix := &accPairIndex{pages: map[uintptr]*accPairPage{}}
	var cand []int
	for j := range tr.accSeq {
		cand = ix.appendConcurrentConflicting(cand[:0], tr, clk, pidx, j)
		// Descending i, matching the scan order. O(w log w) in the window
		// size, not O(w²): on a race-dense trace the window approaches j and
		// a quadratic per-j sort would make the streamed enumeration cubic
		// overall — worse than the scan it replaced. Candidates are unique
		// ints, so sortedness alone fixes the order deterministically.
		sort.Sort(sort.Reverse(sort.IntSlice(cand)))
		for _, i := range cand {
			visit(i, j)
		}
		ix.add(tr, j, clk[j][pidx[tr.accSeq[j]]])
	}
}

// accLastGroup summarizes one goroutine's accesses to one exact byte interval
// on one page: the last access of any kind and the last write, as log index+1
// (0 = none).
type accLastGroup struct {
	seq        uint64
	addr, size uintptr
	lastAny    int32
	lastWrite  int32
}

// accLastIndex indexes accesses for dporTraceClocks' conflict edges: per page,
// per (goroutine, exact interval), the last read/write log indices. Merging
// only these is merge-identical to merging every conflicting prior access
// (same-goroutine snapshots are monotone; see the file comment).
type accLastIndex struct {
	pages map[uintptr][]accLastGroup
	large []int32
}

func (ix *accLastIndex) add(tr exploreTrace, k int) {
	addr := tr.accAddr[k]
	if addr == 0 {
		return
	}
	size := accessSize(tr, k)
	start, end := accIdxPages(addr, size)
	if end-start >= accIdxMaxSpan {
		ix.large = append(ix.large, int32(k))
		return
	}
	seq := tr.accSeq[k]
	for p := start; p <= end; p++ {
		groups := ix.pages[p]
		gi := -1
		for i := range groups {
			if groups[i].seq == seq && groups[i].addr == addr && groups[i].size == size {
				gi = i
				break
			}
		}
		if gi < 0 {
			groups = append(groups, accLastGroup{seq: seq, addr: addr, size: size})
			gi = len(groups) - 1
		}
		groups[gi].lastAny = int32(k) + 1
		if tr.accWrite[k] {
			groups[gi].lastWrite = int32(k) + 1
		}
		ix.pages[p] = groups
	}
}

// appendConflictDominators appends to dst, for access li, a set of earlier
// conflicting accesses whose clocks dominate every earlier conflicting
// access's clock — the per-(page, goroutine, interval) last matching access
// plus every conflicting large-list access (or, for a very wide li, the exact
// set). Deterministic order; an index may repeat (multi-page groups) — the
// consumer's merge is idempotent.
func (ix *accLastIndex) appendConflictDominators(dst []int, tr exploreTrace, li int) []int {
	addr := tr.accAddr[li]
	if addr == 0 {
		return dst
	}
	size := accessSize(tr, li)
	seq := tr.accSeq[li]
	write := tr.accWrite[li]
	start, end := accIdxPages(addr, size)
	if end-start >= accIdxMaxQuery {
		for m := 0; m < li; m++ {
			if accessConflict(tr, m, li) {
				dst = append(dst, m)
			}
		}
		return dst
	}
	for p := start; p <= end; p++ {
		for _, g := range ix.pages[p] {
			if g.seq == seq || !accessOverlap(addr, size, g.addr, g.size) {
				continue
			}
			// li read: only prior writes conflict. li write: any prior access
			// does, and lastAny dominates lastWrite.
			last := g.lastWrite
			if write {
				last = g.lastAny
			}
			if last != 0 {
				dst = append(dst, int(last)-1)
			}
		}
	}
	for _, l := range ix.large {
		if m := int(l); accessConflict(tr, m, li) {
			dst = append(dst, m)
		}
	}
	return dst
}
