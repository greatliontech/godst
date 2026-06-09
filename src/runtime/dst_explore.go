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

// dstAccessYield is the access-granularity cooperative yield + access record: the
// Level-2 transition boundary (D1). Placed at a memory access — manually for the
// build-order-(b) validation phase, by the dst-race compiler mode later — it
// records the pending access on the goroutine (for DPOR's dependency relation) and
// lets the deterministic scheduler switch at this access.
//
// Safe-point guard (DST-L2-1): yield only on a bubble (SUT) goroutine running on
// its own stack with no runtime lock held; otherwise return without yielding —
// skipping a yield is always sound (it only forgoes an interleaving). goyield
// requeues the current G and reschedules through dstFindRunnable, so the seam never
// runs a blocked G; soundness is inherited from Seq 5 unchanged.
//
//go:linkname dstAccessYield
func dstAccessYield(addr unsafe.Pointer, write bool) {
	gp := getg()
	if !dstActive() || gp.bubble == nil || gp != gp.m.curg || gp.m.locks != 0 {
		return
	}
	gp.dstAccAddr = uintptr(addr)
	gp.dstAccWrite = write
	dstAccessYieldPoints++
	goyield()
}

// dstYieldPoint is a cooperative yield with no specific memory access recorded — a
// pure scheduling point (used by soundness probes). See dstAccessYield.
//
//go:linkname dstYieldPoint
func dstYieldPoint() {
	gp := getg()
	if !dstActive() || gp.bubble == nil || gp != gp.m.curg || gp.m.locks != 0 {
		return
	}
	gp.dstAccAddr = 0
	dstAccessYieldPoints++
	goyield()
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

// dstExploreInit pre-sizes the trace buffers for the exploration. Called by the
// brain once, on a normal goroutine (off-lock), before any scheduled Run.
// maxDecisions bounds the per-bubble decision count; maxEnabledTotal bounds the
// total enabled-set entries across all decisions in one bubble.
//
//go:linkname dstExploreInit
func dstExploreInit(maxDecisions, maxEnabledTotal int) {
	dstTraceChosen = make([]uint64, maxDecisions)
	dstTraceEnabOff = make([]int32, maxDecisions)
	dstTraceEnabLen = make([]int32, maxDecisions)
	dstTraceEnabFlat = make([]uint64, maxEnabledTotal)
	dstTraceAddr = make([]uintptr, maxDecisions)
	dstTraceWrite = make([]bool, maxDecisions)
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
// performed: addr (0 = none) and whether it was a write. The DPOR dependency
// relation pairs decisions with the same nonzero addr where at least one is a write.
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

// dstScheduleAbortedFP reports whether the last run's prefix was invalid for that
// execution (named a non-enabled dstSeq at some decision).
//
//go:linkname dstScheduleAbortedFP
func dstScheduleAbortedFP() bool { return dstScheduleAborted }
