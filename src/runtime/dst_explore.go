// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Deterministic simulation testing (DST) Level-2 exploration substrate: the
// scheduled scheduling strategy (dstSchedScheduled). Under it the runtime is a
// pure schedule-FOLLOWER and trace-RECORDER — it follows an explicit prefix of
// bubble-goroutine choices at the dstSchedSelect seam and records, per decision,
// the enabled bubble-goroutine set and the chosen one. The exploration brain
// (offline, between Runs: exhaustive enumeration, then DPOR) consumes the trace
// and emits the next prefix. See docs/dst/design.md "Level 2 — access-granularity
// interleaving + DPOR", increments 2–4 (build order (b): the brain is proven on
// the manual access-yield hook before auto-instrumentation).
//
// CONSTRAINT (load-bearing): dstScheduledSelect runs on g0 while holding
// sched.lock (it is called from dstFindRunnable). It must NOT allocate. The trace
// buffers are therefore pre-sized by the brain via dstExploreInit (on a normal
// goroutine, off-lock) and written by index here; an over-budget run sets
// dstTraceOverflow rather than growing (the brain reports it — no silent cap).

package runtime

import "unsafe" // for go:linkname and the access-yield hook

// dstAccessYieldPoints counts guarded access yields taken since the last reset —
// the per-run yield magnitude, read via dstAccessYieldFP.
var dstAccessYieldPoints uint64

// dstYieldAccess is the shared core of every Level-2 transition-boundary hook: it
// records the pending transition (addr, write) on the current goroutine for DPOR's
// dependency relation and yields, subject to the safe-point guard. The guard
// (DST-L2-1) lives here, in ONE place, so it cannot drift across the hooks: yield
// only on a bubble (SUT) goroutine running on its own stack with no runtime lock
// held; otherwise return without yielding — skipping a yield is always sound (it
// only forgoes an interleaving). goyield requeues the current G and reschedules
// through dstFindRunnable, so the seam never runs a blocked G; soundness is
// inherited from Seq 5 unchanged.
func dstYieldAccess(addr uintptr, write bool) {
	gp := getg()
	// Access-granularity yielding is a Level-2 (scheduled-strategy) mechanism. Under
	// the Seq-5 Random/PCT strategies the interleaving atoms are the coarse cooperative
	// points, and an extra yield at every access would perturb those strategies — which
	// matters now that the dst-race compiler mode auto-inserts this at EVERY memory
	// access under -race (so any -tags dst -race run, not just Explore, reaches here).
	// Gating to dstSchedScheduled confines access yields to Explore; non-scheduled
	// strategies are byte-for-byte unaffected.
	if !dstActive() || dstSchedKind != dstSchedScheduled || gp.bubble == nil || gp != gp.m.curg || gp.m.locks != 0 {
		return
	}
	gp.dstAccAddr = addr
	gp.dstAccWrite = write
	dstAccessYieldPoints++
	goyield()
}

// dstAccessYield is the access-granularity cooperative yield + access record: the
// Level-2 transition boundary (D1) at a MEMORY access — manually for the
// build-order-(b) validation phase, by the dst-race compiler mode later. It records
// the pending access (addr, isWrite) and lets the deterministic scheduler switch at
// this access.
//
//go:linkname dstAccessYield
func dstAccessYield(addr unsafe.Pointer, write bool) {
	dstYieldAccess(uintptr(addr), write)
}

// dstSyncAcquire is the Level-2 transition boundary for a synchronization
// ACQUISITION (mutex Lock, channel send/recv rendezvous, ...): it announces the
// sync object's identity as a write-conflict and yields BEFORE the blocking op, so
// the order in which contending goroutines acquire the object is itself a DPOR
// transition. Two acquisitions of the same object by different goroutines are then a
// co-enabled, concurrent, conflicting pair whose BOTH orderings DPOR explores —
// without which a program whose outcome depends on acquisition order silently loses
// Mazurkiewicz classes (DST-L2-3; see TestDSTExploreSweep, which fails 23/289 with
// this hook neutered). Modeled as a write-conflict because acquisitions do not
// commute. Same guard/soundness as dstAccessYield (a pre-acquire yield is sound: the
// real scheduler can switch before any goroutine acquires the object). Placed
// manually for the validation phase; the auto-instrumentation phase wires the
// runtime sync primitives (chan ops; a dst hook in sync.Mutex/the sema layer) to it.
//
//go:linkname dstSyncAcquire
func dstSyncAcquire(id unsafe.Pointer) {
	dstYieldAccess(uintptr(id), true)
}

// dstYieldPoint is a cooperative yield with no specific memory access recorded — a
// pure scheduling point (used by soundness probes). See dstAccessYield.
//
//go:linkname dstYieldPoint
func dstYieldPoint() {
	dstYieldAccess(0, false)
}

//go:linkname dstAccessYieldFP
func dstAccessYieldFP() uint64 { return dstAccessYieldPoints }

//go:linkname dstAccessYieldReset
func dstAccessYieldReset() { dstAccessYieldPoints = 0 }

// dstSchedulePrefix[s] is the stable per-bubble index (dstSeq, not goid) to run at
// scheduled-strategy decision s. Beyond its end the strategy runs the
// lowest-dstSeq enabled candidate (a deterministic default), and every decision is
// recorded so the brain can extend the prefix. Set by dstSetSchedule before a Run.
var dstSchedulePrefix []uint64

// dstScheduleStep is the index into dstSchedulePrefix (decisions taken this
// bubble). dstScheduleAborted is set if a prefix entry named a dstSeq not enabled
// at its decision — an invalid prefix for this execution; this must not occur for a
// prefix derived from a recorded trace (it would signal a replay-determinism bug,
// DST-L2-2), and the brain treats it as a hard error (see runOnce).
var (
	dstScheduleStep    int
	dstScheduleAborted bool
)

// dstSeqCtr assigns stable per-bubble goroutine indices for the scheduled
// strategy. goid is a process-global monotonic counter, so the SAME logical
// goroutine gets a DIFFERENT goid in each bubble re-execution — useless as a
// schedule identity for replay. Instead each bubble goroutine gets a per-bubble
// index (its dstSeq, storing index+1; 0 = unassigned) the first time it appears as
// a scheduling candidate, in deterministic first-candidacy order. Following a
// fixed prefix reproduces the same execution → the same candidacy order → the same
// indices, so a recorded schedule replays exactly. Reset per bubble.
var dstSeqCtr uint64

// Pre-sized trace buffers (allocated by dstExploreInit, never under the lock). The
// enabled sets are stored flat: decision i's enabled dstSeq indices are
// dstTraceEnabFlat[dstTraceEnabOff[i] : +dstTraceEnabLen[i]]. dstTraceN is the
// decision count this bubble; dstTraceOverflow signals the budget was exceeded.
var (
	dstTraceChosen   []uint64
	dstTraceEnabOff  []int32
	dstTraceEnabLen  []int32
	dstTraceEnabFlat []uint64
	dstTraceAddr     []uintptr // [decision] -> chosen transition's memory-access address (0 = none)
	dstTraceWrite    []bool    // [decision] -> chosen transition's access is a write
	dstTraceN        int
	dstTraceFlatN    int
	dstTraceOverflow bool
)

// Happens-before edge buffers (increment 2). Each goready under the scheduled
// strategy records that the readier goroutine happens-before the readied one's
// resumption: edge e is (dstEdgeFrom[e] readier -> dstEdgeTo[e] readied) observed
// during the transition with dstScheduleStep == dstEdgeStep[e]. The DPOR engine
// builds vector clocks offline from these + program order, so two conflicting
// accesses are dependent only if they are CONCURRENT (neither happens-before the
// other) — pruning mutex/channel-serialized pairs the conservative relation would
// over-explore. Pre-sized (never grown under the lock); over-budget sets
// dstEdgeOverflow (reported, never a silent cap).
var (
	dstEdgeFrom     []uint64
	dstEdgeTo       []uint64
	dstEdgeStep     []int32
	dstEdgeN        int
	dstEdgeOverflow bool
)

// Access log buffers (shared-address filtering, increment 6). Unlike the per-decision
// trace (which records the access of the goroutine CHOSEN at each scheduling
// decision), the access log records EVERY instrumented access in execution order —
// (accessing goroutine dstSeq, addr, isWrite, the dstScheduleStep it occurred under) —
// decoupled from whether the access yielded. This decoupling is what lets the runtime
// FILTER: a single-owner access can "record but not yield" (design.md D1) while the
// brain still sees it for the dependency relation. The brain sources DPOR's
// dependency/HB relation from this log (not the decision trace), so a conflicting
// pair whose FIRST access was single-owner-at-the-time (hence did not yield, so is not
// a decision) is still reversed. Pre-sized (never grown under the lock); over-budget
// sets dstAccLogOverflow (reported, never a silent cap).
var (
	dstAccLogSeq      []uint64
	dstAccLogAddr     []uintptr
	dstAccLogWrite    []bool
	dstAccLogStep     []int32
	dstAccLogN        int
	dstAccLogOverflow bool
)

// dstRecordAccess appends one access to the log in COMMIT order: (seq, addr, write,
// step), where step is the dstScheduleStep the access commits under (so an access with
// step s was committed by the goroutine chosen at decision s-1, and its reversal
// anchors at decision s-1). Logging in COMMIT order, not announce order, is
// load-bearing: an access is *announced* at dstAccessYield (when the goroutine reaches
// it and yields) but *commits* only when the goroutine is next resumed — those orders
// differ, and the dependency relation needs the order the memory operations actually
// take effect. So a YIELDING access is logged at the scheduling decision that resumes
// it (dstScheduledSelect, step = dstScheduleStep+1); a NON-yielding (shared-address-
// filtered) access commits inline with no reschedule, so announce order == commit
// order and it is logged at dstAccessYield (step = dstScheduleStep). Allocation-free;
// the dstScheduledSelect caller runs under sched.lock, so this must not allocate.
func dstRecordAccess(seq uint64, addr uintptr, write bool, step int) {
	if dstAccLogN < len(dstAccLogSeq) {
		dstAccLogSeq[dstAccLogN] = seq
		dstAccLogAddr[dstAccLogN] = addr
		dstAccLogWrite[dstAccLogN] = write
		dstAccLogStep[dstAccLogN] = int32(step)
		dstAccLogN++
	} else {
		dstAccLogOverflow = true
	}
}

// dstRecordReadyEdge records the happens-before edge readier -> readied at a
// goready, under the scheduled strategy. Both ends are assigned a stable index if
// needed (lazy, like first candidacy). Called from goready before the systemstack
// switch (readier == getg()); skips when either end is not a bubble goroutine
// (a system/driver wake carries no application HB). Allocation-free.
func dstRecordReadyEdge(readier, readied *g) {
	if readier == nil || readied == nil || readier.bubble == nil || readied.bubble == nil {
		return
	}
	if readier.dstSeq == 0 {
		dstSeqCtr++
		readier.dstSeq = dstSeqCtr
	}
	if readied.dstSeq == 0 {
		dstSeqCtr++
		readied.dstSeq = dstSeqCtr
	}
	if dstEdgeN < len(dstEdgeFrom) {
		dstEdgeFrom[dstEdgeN] = readier.dstSeq
		dstEdgeTo[dstEdgeN] = readied.dstSeq
		dstEdgeStep[dstEdgeN] = int32(dstScheduleStep)
		dstEdgeN++
	} else {
		dstEdgeOverflow = true
	}
}

// dstExploreInit pre-sizes the trace buffers for the exploration. Called by the
// brain once, on a normal goroutine (off-lock), before any scheduled Run.
// maxDecisions bounds the per-bubble decision count; maxEnabledTotal bounds the
// total enabled-set entries across all decisions in one bubble; maxAccesses bounds
// the per-bubble access-log entries.
//
//go:linkname dstExploreInit
func dstExploreInit(maxDecisions, maxEnabledTotal, maxEdges, maxAccesses int) {
	dstTraceChosen = make([]uint64, maxDecisions)
	dstTraceEnabOff = make([]int32, maxDecisions)
	dstTraceEnabLen = make([]int32, maxDecisions)
	dstTraceEnabFlat = make([]uint64, maxEnabledTotal)
	dstTraceAddr = make([]uintptr, maxDecisions)
	dstTraceWrite = make([]bool, maxDecisions)
	dstEdgeFrom = make([]uint64, maxEdges)
	dstEdgeTo = make([]uint64, maxEdges)
	dstEdgeStep = make([]int32, maxEdges)
	dstAccLogSeq = make([]uint64, maxAccesses)
	dstAccLogAddr = make([]uintptr, maxAccesses)
	dstAccLogWrite = make([]bool, maxAccesses)
	dstAccLogStep = make([]int32, maxAccesses)
}

// dstScheduleReset prepares the scheduled-strategy state for a new bubble. Called
// from synctestRun under dstActive when the scheduled strategy is active. Zeroes
// counters only — no allocation.
func dstScheduleReset() {
	dstScheduleStep = 0
	dstScheduleAborted = false
	dstTraceN = 0
	dstTraceFlatN = 0
	dstTraceOverflow = false
	dstSeqCtr = 0
	dstEdgeN = 0
	dstEdgeOverflow = false
	dstAccLogN = 0
	dstAccLogOverflow = false
}

// dstScheduledSelect implements the scheduled strategy at the unified seam. Every
// candidate is a bubble goroutine here (dstFindRunnable schedules system Gs first,
// so dstSchedSelect is reached only when no candidate is a system G), so the
// enabled set is the whole candidate set. It records the enabled set + choice and
// returns the chosen candidate index. Allocation-free (runs under sched.lock).
func dstScheduledSelect(c *dstCandidates, total uint32) uint32 {
	// Assign stable per-bubble indices to any not-yet-seen candidate, in candidate
	// order (deterministic per execution). dstSeq stores index+1; 0 = unassigned.
	for k := uint32(0); k < total; k++ {
		if g := c.at(k); g.dstSeq == 0 {
			dstSeqCtr++
			g.dstSeq = dstSeqCtr
		}
	}
	var sel uint32
	if dstScheduleStep < len(dstSchedulePrefix) {
		want := dstSchedulePrefix[dstScheduleStep]
		sel = ^uint32(0)
		for k := uint32(0); k < total; k++ {
			if c.at(k).dstSeq == want {
				sel = k
				break
			}
		}
		if sel == ^uint32(0) {
			dstScheduleAborted = true
			sel = dstLowestSeqIdx(c, total)
		}
	} else {
		sel = dstLowestSeqIdx(c, total)
	}
	// Record the decision (enabled set in candidate order + chosen id), bounded.
	if dstTraceN < len(dstTraceChosen) && dstTraceFlatN+int(total) <= len(dstTraceEnabFlat) {
		chosen := c.at(sel)
		dstTraceChosen[dstTraceN] = chosen.dstSeq
		dstTraceAddr[dstTraceN] = chosen.dstAccAddr
		dstTraceWrite[dstTraceN] = chosen.dstAccWrite
		// Log this decision's transition in COMMIT order: the chosen goroutine announced
		// its pending access (set dstAccAddr) at an earlier dstAccessYield and yielded;
		// it is now resumed to commit it, in the interval after this decision (so it runs
		// with dstScheduleStep+1). One log entry per decision — INCLUDING coarse points
		// (block/create/pure yield), recorded with addr==0 — so the log mirrors the
		// decision sequence the DPOR happens-before clocks and source-set witness range
		// over (a coarse transition is independent of everything, addr==0, but it ticks
		// its goroutine's clock and can be a weak-initial, exactly as a decision does).
		// Logging here (not at the announce) gives the order the memory operations
		// actually take effect.
		dstRecordAccess(chosen.dstSeq, chosen.dstAccAddr, chosen.dstAccWrite, dstScheduleStep+1)
		// Consume the pending access: the transition about to run performs it
		// exactly once. Without this, a goroutine that next blocks at a *coarse*
		// point (channel/WaitGroup) rather than at another dstAccessYield would carry
		// a stale address into that later decision — over-approximating the
		// dependency relation (extra DPOR exploration; still sound, never a missed
		// class). The goroutine sets a fresh dstAccAddr at its next dstAccessYield.
		chosen.dstAccAddr = 0
		chosen.dstAccWrite = false
		dstTraceEnabOff[dstTraceN] = int32(dstTraceFlatN)
		dstTraceEnabLen[dstTraceN] = int32(total)
		for k := uint32(0); k < total; k++ {
			dstTraceEnabFlat[dstTraceFlatN] = c.at(k).dstSeq
			dstTraceFlatN++
		}
		dstTraceN++
	} else {
		dstTraceOverflow = true
	}
	dstScheduleStep++
	return sel
}

// dstLowestSeqIdx returns the candidate index with the smallest stable index
// (dstSeq) — the deterministic default choice beyond the prefix (and the canonical
// first child in the exhaustive DFS). All candidates are assigned by the caller
// before this runs. Allocation-free.
func dstLowestSeqIdx(c *dstCandidates, total uint32) uint32 {
	best := uint32(0)
	bestSeq := c.at(0).dstSeq
	for k := uint32(1); k < total; k++ {
		if s := c.at(k).dstSeq; s < bestSeq {
			best, bestSeq = k, s
		}
	}
	return best
}

// --- brain-facing API (linkname'd; called on normal goroutines, off-lock) ------

// dstSetSchedule installs the prefix the next scheduled Run follows. The brain
// owns the slice and must not mutate it during the Run.
//
//go:linkname dstSetSchedule
func dstSetSchedule(prefix []uint64) { dstSchedulePrefix = prefix }

// dstTraceLenFP reports the decision count recorded by the last scheduled Run.
//
//go:linkname dstTraceLenFP
func dstTraceLenFP() int { return dstTraceN }

// dstTraceChosenFP reports the dstSeq chosen at decision i.
//
//go:linkname dstTraceChosenFP
func dstTraceChosenFP(i int) uint64 { return dstTraceChosen[i] }

// dstTraceAccessFP reports the memory access the transition chosen at decision i
// performed: addr (0 = none) and whether it was a write. Kept as part of the
// decision-trace interface; log-based DPOR uses dstAccLogAtFP for dependency/HB.
//
//go:linkname dstTraceAccessFP
func dstTraceAccessFP(i int) (addr uintptr, write bool) {
	return dstTraceAddr[i], dstTraceWrite[i]
}

// dstTraceEnabledFP reports the enabled bubble-goroutine dstSeq set at decision i. The
// returned slice aliases the runtime's flat buffer; the brain must copy it before
// the next Run overwrites it.
//
//go:linkname dstTraceEnabledFP
func dstTraceEnabledFP(i int) []uint64 {
	off := dstTraceEnabOff[i]
	return dstTraceEnabFlat[off : off+dstTraceEnabLen[i]]
}

// dstTraceOverflowFP reports whether the last run exceeded the trace budget
// (dstExploreInit sizing) — the brain reports this rather than treating a
// truncated trace as complete.
//
//go:linkname dstTraceOverflowFP
func dstTraceOverflowFP() bool { return dstTraceOverflow }

// dstEdgeLenFP reports the happens-before edge count recorded by the last run, and
// dstEdgeAtFP reports edge i as (readier index, readied index, the dstScheduleStep
// at which the goready occurred). dstEdgeOverflowFP reports a budget overflow.
//
//go:linkname dstEdgeLenFP
func dstEdgeLenFP() int { return dstEdgeN }

//go:linkname dstEdgeAtFP
func dstEdgeAtFP(i int) (from, to uint64, step int) {
	return dstEdgeFrom[i], dstEdgeTo[i], int(dstEdgeStep[i])
}

//go:linkname dstEdgeOverflowFP
func dstEdgeOverflowFP() bool { return dstEdgeOverflow }

// dstAccLogLenFP reports the access-log entry count recorded by the last run;
// dstAccLogAtFP reports entry i as (accessing goroutine dstSeq, addr, isWrite, the
// dstScheduleStep it occurred under). The brain sources DPOR's dependency/HB relation
// from this log (decoupled from the decision trace) so single-owner accesses can be
// filtered to non-yields without losing the dependency. dstAccLogOverflowFP reports a
// budget overflow (coverage incomplete — never a silent cap).
//
//go:linkname dstAccLogLenFP
func dstAccLogLenFP() int { return dstAccLogN }

//go:linkname dstAccLogAtFP
func dstAccLogAtFP(i int) (seq uint64, addr uintptr, write bool, step int) {
	return dstAccLogSeq[i], dstAccLogAddr[i], dstAccLogWrite[i], int(dstAccLogStep[i])
}

//go:linkname dstAccLogOverflowFP
func dstAccLogOverflowFP() bool { return dstAccLogOverflow }

// dstScheduleAbortedFP reports whether the last run's prefix was invalid for that
// execution (named a non-enabled dstSeq at some decision).
//
//go:linkname dstScheduleAbortedFP
func dstScheduleAbortedFP() bool { return dstScheduleAborted }
