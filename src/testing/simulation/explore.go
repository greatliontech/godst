// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

// Level-2 systematic interleaving exploration. Explore drives repeated bubble
// re-executions of a SUT, each following an explicit scheduling-decision prefix
// (the scheduled strategy, runtime/dst_explore.go), and enumerates the sound
// interleavings — exhaustively, or (later) pruned by DPOR. The runtime is a pure
// schedule-follower + trace-recorder; the exploration "brain" lives here, offline
// between Runs. See docs/dst/design.md "Level 2 — access-granularity interleaving
// + DPOR".

// ExploreMode selects the interleaving-exploration algorithm.
type ExploreMode int

const (
	// Exhaustive enumerates every distinct sound interleaving by brute force over
	// the scheduling-decision tree. Complete but exponential; the baseline DPOR
	// must match.
	Exhaustive ExploreMode = iota
	// DPOR (dynamic partial-order reduction) explores one interleaving per
	// Mazurkiewicz equivalence class, pruning provably-equivalent reorderings —
	// finding the same bugs as Exhaustive with far fewer runs.
	DPOR
)

// Failure records an interleaving under which the SUT reported a bug, with the
// schedule prefix that reproduces it deterministically.
type Failure struct {
	// Schedule is the decision prefix — the stable per-bubble goroutine indices
	// (dstSeq, not goid) chosen in decision order — that reproduces this failure
	// when fed back as the scheduled strategy's prefix.
	Schedule []uint64
	// Race is true iff the -race detector reported a NEW data race during this
	// schedule's run (the D5 oracle). False means the failure is a SUT assertion
	// (sut returned true). In a non-race build Race is always false. The detector
	// dedups by signature, so each distinct race yields exactly one Race failure —
	// the first schedule that exhibits it, a deterministic reproducer.
	Race bool
}

// ExploreResult reports an Explore run.
type ExploreResult struct {
	// Schedules is the number of interleavings actually explored.
	Schedules int
	// Failures lists every explored interleaving that exhibited a bug — the SUT
	// returned true (an assertion failure) OR the -race detector reported a new data
	// race (Failure.Race; see D5) — each with its reproducing schedule.
	Failures []Failure
	// Exhausted is true iff the (pruned) interleaving space was fully covered. It
	// is false when Overflow truncated coverage.
	Exhausted bool
	// Overflow is true iff some run exceeded the per-bubble trace, edge, or access-log
	// budget; coverage is then INCOMPLETE (reported, never silently capped).
	Overflow bool
}

// Per-bubble trace budget. An over-budget run sets ExploreResult.Overflow and the
// space is reported not-exhausted, rather than silently capping coverage.
const (
	exploreMaxDecisions    = 1 << 14
	exploreMaxEnabledTotal = 1 << 18
	exploreMaxEdges        = 1 << 16
	exploreMaxAccesses     = 1 << 16
)

// Explore systematically explores the sound interleavings of sut under seed. sut
// returns true iff THIS interleaving exhibited a bug (a failed assertion); Explore
// records every such schedule. The seed fixes the program's data randomness
// (select/map/math+crypto rand); the interleaving is controlled by Explore,
// independent of the seed. Requires building with -tags dst (and -race to also use
// the happens-before detector as an oracle within each interleaving).
//
// Exhaustive enumerates the whole decision tree. DPOR explores one interleaving
// per Mazurkiewicz equivalence class, finding the same bugs with far fewer runs.
func Explore(seed uint64, mode ExploreMode, sut func() bool) ExploreResult {
	if !dstBuilt() {
		panic("testing/simulation: Explore requires building with -tags dst")
	}
	dstExploreInit(exploreMaxDecisions, exploreMaxEnabledTotal, exploreMaxEdges, exploreMaxAccesses)
	if mode == DPOR {
		return dporExplore(seed, sut)
	}
	return exhaustiveExplore(seed, sut)
}

// exploreTrace is one scheduled run's recorded decision trace, copied out of the
// runtime's per-bubble buffers (which the next run overwrites).
type exploreTrace struct {
	procs   []uint64   // [decision] chosen goroutine (stable per-bubble index, dstSeq)
	enabled [][]uint64 // [decision] enabled goroutine set (dstSeq indices)
	// Access log: EVERY instrumented access in execution order, decoupled from the
	// decision trace (a single-owner access records here without yielding, so it is
	// not a decision). DPOR's dependency/HB relation is sourced from this log, not from
	// per-decision addrs, so a conflicting pair whose first access was single-owner (so
	// did not yield) is still reversed. accStep[k] is the dstScheduleStep the access
	// occurred under: an access with step s was performed by procs[s-1] (the goroutine
	// chosen at decision s-1) during the interval after decision s-1, so its reversal
	// anchors at decision s-1. accSeq[k] therefore equals procs[accStep[k]-1].
	accSeq   []uint64
	accAddr  []uintptr
	accWrite []bool
	accStep  []int
	// Happens-before edges (goready: edgeFrom happens-before edgeTo's resumption),
	// edgeStep = the dstScheduleStep when the goready fired.
	edgeFrom []uint64
	edgeTo   []uint64
	edgeStep []int
	aborted  bool // prefix named a non-enabled goroutine (a replay-determinism bug)
	overflow bool // run exceeded the trace, edge, or access-log budget (coverage incomplete)
}

// runOnce runs sut once under the scheduled strategy following prefix, and copies
// out the recorded trace. failed is sut's verdict for this interleaving; raced is
// true iff the -race detector reported a NEW data race during this run (D5 oracle;
// always false in a non-race build — dstRaceErrors returns 0).
func runOnce(seed uint64, prefix []uint64, sut func() bool) (failed, raced bool, tr exploreTrace) {
	racesBefore := dstRaceErrors()
	run(seed, kindScheduled, 0, 0, defaultHostname, defaultPID, defaultNumCPU, 0, prefix, func() {
		failed = sut()
	})
	raced = dstRaceErrors() > racesBefore
	tr.overflow = dstTraceOverflowFP() || dstEdgeOverflowFP()
	tr.aborted = dstScheduleAbortedFP()
	if tr.aborted {
		// The prefix named a goroutine not enabled at its decision: this replay
		// diverged from the run that produced the prefix. Since every prefix is
		// derived from a recorded trace, that is a replay-determinism (DST-L2-2)
		// violation that must never happen — surface it loudly rather than silently
		// dropping the subtree, which would make ExploreResult.Exhausted a false
		// positive (silent incompleteness). A panic here is a hard assertion on the
		// determinism contract, not a tolerable budget condition like Overflow.
		panic("testing/simulation: internal error: schedule prefix diverged on replay " +
			"(a goroutine in the prefix was not enabled at its decision) — DST-L2-2 violation")
	}
	tr.overflow = tr.overflow || dstAccLogOverflowFP()
	n := dstTraceLenFP()
	tr.procs = make([]uint64, n)
	tr.enabled = make([][]uint64, n)
	for i := 0; i < n; i++ {
		tr.procs[i] = dstTraceChosenFP(i)
		tr.enabled[i] = append([]uint64(nil), dstTraceEnabledFP(i)...) // copy: aliases runtime buffer
	}
	a := dstAccLogLenFP()
	tr.accSeq = make([]uint64, a)
	tr.accAddr = make([]uintptr, a)
	tr.accWrite = make([]bool, a)
	tr.accStep = make([]int, a)
	for i := 0; i < a; i++ {
		tr.accSeq[i], tr.accAddr[i], tr.accWrite[i], tr.accStep[i] = dstAccLogAtFP(i)
	}
	m := dstEdgeLenFP()
	tr.edgeFrom = make([]uint64, m)
	tr.edgeTo = make([]uint64, m)
	tr.edgeStep = make([]int, m)
	for i := 0; i < m; i++ {
		tr.edgeFrom[i], tr.edgeTo[i], tr.edgeStep[i] = dstEdgeAtFP(i)
	}
	return
}

// exhaustiveExplore enumerates every distinct interleaving by DFS over the
// scheduling-decision tree: each run reveals its decisions, and every not-taken
// enabled goroutine at every free decision is queued as a child prefix.
func exhaustiveExplore(seed uint64, sut func() bool) ExploreResult {
	var res ExploreResult
	visited := map[string]bool{}
	stack := [][]uint64{nil}
	for len(stack) > 0 {
		prefix := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if k := encodePrefix(prefix); visited[k] {
			continue
		} else {
			visited[k] = true
		}
		failed, raced, tr := runOnce(seed, prefix, sut)
		res.Schedules++
		if tr.overflow {
			res.Overflow = true
		}
		if failed || raced {
			res.Failures = append(res.Failures, Failure{Schedule: clonePrefix(prefix), Race: raced})
		}
		for i := len(prefix); i < len(tr.procs); i++ {
			for _, g := range tr.enabled[i] {
				if g == tr.procs[i] {
					continue
				}
				child := make([]uint64, i+1)
				copy(child, tr.procs[:i])
				child[i] = g
				stack = append(stack, child)
			}
		}
	}
	res.Exhausted = !res.Overflow
	return res
}

// dporTrans is one access's sleep-set pruning identity: its run-local address
// (0 = no conflict identity) and whether it is a write.
type dporTrans struct {
	addr  uintptr
	write bool
}

// indepTrans reports the conservative independence relation used only for sleep-set
// pruning. Sleep transitions are carried across stateless re-executions, but raw
// access addresses are run-local: the same logical object can have different numeric
// addresses in sibling runs after the explorer allocates. Therefore a nonzero access
// with a write is treated as dependent on every other nonzero access. This may wake
// extra sleepers (less pruning) but cannot keep a genuinely dependent transition
// asleep and drop a class. Read/read and addr==0 transitions still commute.
func indepTrans(a, b dporTrans) bool {
	return a.addr == 0 || b.addr == 0 || !(a.write || b.write)
}

// indepSets reports whether two interval access-SETS commute: no access in a conflicts
// with any access in b. A decision's "transition" under shared-address filtering is the
// SET of accesses the chosen goroutine performed in its interval — single-owner
// accesses record but do not yield, so one interval can hold several accesses plus the
// conflicting one that yielded. For sleep sets this is CONSERVATIVE: a possible
// conflict in ANY pair wakes the sleeper, so it can only wake more goroutines, never
// wrongly keep one asleep — it cannot drop a class (DST-L2-3) while still pruning the
// bulk of the equivalence-class redundancy.
func indepSets(a, b []dporTrans) bool {
	for _, x := range a {
		for _, y := range b {
			if !indepTrans(x, y) {
				return false
			}
		}
	}
	return true
}

// dporFrame is one decision on the DPOR DFS stack. Its "transition" is the SET of
// accesses the chosen goroutine performed in its interval (the access-log entries with
// accStep == this decision's index + 1).
type dporFrame struct {
	proc      uint64                 // the goroutine currently chosen at this decision
	enabled   []uint64               // enabled goroutines at this decision (deterministic order)
	backtrack map[uint64]bool        // goroutines that must be explored here (source set)
	done      map[uint64]bool        // goroutines already explored here
	doneTrans map[uint64][]dporTrans // explored goroutines' interval access-set here (for sleep propagation)
	sleep     map[uint64][]dporTrans // goroutines asleep here (their interval access-set): redundant to explore — skip
}

// dporExplore is iterative stateless SOURCE-DPOR with SLEEP SETS (Abdulla, Aronis,
// Jonsson & Sagonas, "Optimal Dynamic Partial Order Reduction", POPL 2014). It
// explores (at most) one interleaving per Mazurkiewicz equivalence class. Two
// transitions are *dependent* iff they record the same nonzero conflict identity
// with at least one write, by different goroutines — where the identity is a shared
// memory address (dstAccessYield) OR a synchronization object's identity
// (dstSyncAcquire, recording a mutex/channel acquisition as a write-conflict so its
// acquisition ORDER is a dependency; see runtime/dst_explore.go and design.md
// "Completeness boundary"). Each run re-executes from the start following the stack's
// chosen prefix.
//
// Two cooperating mechanisms remove the redundant re-exploration that a plain
// backtrack-set search incurs:
//
//   - SLEEP SETS. A frame inherits, from its parent, the parent's asleep +
//     already-explored goroutines FILTERED by independence with the transition the
//     parent chose (indepSets). A goroutine that commutes with the chosen one would
//     only re-derive an already-explored equivalent interleaving, so it stays asleep;
//     a DEPENDENT one is woken. An asleep backtrack choice is skipped.
//
//   - SOURCE SETS (weak-initial backtracking). When a run reveals a reversible race
//     (a concurrent dependent pair e_i, e_j, i<j), the reverse ordering must be
//     explored. Adding e_j's process directly to backtrack(i) is NOT enough: if that
//     process is asleep at i, sleep-pruning skips it and the class is lost (the naive
//     bug). Source-DPOR instead adds a WEAK-INITIAL of the race witness — a process
//     that can run first at decision i and lead to e_j before e_i (addSourceBacktrack)
//     — which is provably not sleep-blocked for that ordering. This is what makes the
//     sleep-pruned search complete (DST-L2-3), enforced by TestDSTExploreSweep.
//
// Independence for sleep is deliberately coarser than the dependency relation because
// sleep transitions cross stateless re-executions and raw addresses are run-local; a
// nonzero access with a write wakes any nonzero sleeper. The race relation itself uses
// per-run addresses plus the happens-before clocks (dporConcurrent), so mutex/channel-
// SERIALIZED conflicts are pruned while a free acquisition ORDER is explored both ways.
// addr==0 transitions (goroutine creation, WaitGroup wakeups, the isolated gcDrain
// goroutine) record no conflict identity and are independent of everything — they
// carry no outcome-determining order choice a recorded access/acquisition does not
// already capture. See design.md "Completeness boundary (addr=0 transitions)".
func dporExplore(seed uint64, sut func() bool) ExploreResult {
	var res ExploreResult
	var stack []*dporFrame
	for {
		prefix := make([]uint64, len(stack))
		for i, fr := range stack {
			prefix[i] = fr.proc
		}
		failed, raced, tr := runOnce(seed, prefix, sut)
		res.Schedules++
		if tr.overflow {
			res.Overflow = true
		}
		if failed || raced {
			res.Failures = append(res.Failures, Failure{Schedule: clonePrefix(prefix), Race: raced})
		}
		// runOnce panics on a divergent (aborted) replay, so the trace here is always
		// a faithful replay of the followed prefix.
		n := len(tr.procs)
		// Per-decision interval access-set: decision d's transition is the SET of
		// accesses performed in its interval — the access-log entries with
		// accStep == d+1 (performed by procs[d]). Under shared-address filtering an
		// interval may hold several single-owner accesses plus the conflicting one that
		// yielded; with yield-at-every-access each interval is a singleton.
		intervalSet := make([][]dporTrans, n)
		for k := range tr.accSeq {
			if d := tr.accStep[k] - 1; d >= 0 && d < n {
				intervalSet[d] = append(intervalSet[d], dporTrans{addr: tr.accAddr[k], write: tr.accWrite[k]})
			}
		}
		// Extend the stack with the decisions this run newly revealed. Each new frame
		// inherits its sleep set from its parent: the parent's asleep + already-explored
		// goroutines, FILTERED by independence with the parent's chosen interval set. A
		// sleeping goroutine has not executed since it was put to sleep, so the access
		// set stored with it stays valid down the frames it sleeps through.
		for d := len(stack); d < n; d++ {
			sleep := map[uint64][]dporTrans{}
			if d > 0 {
				par := stack[d-1]
				pt := intervalSet[d-1]
				for q, qt := range par.sleep {
					if indepSets(qt, pt) {
						sleep[q] = qt
					}
				}
				for q, qt := range par.doneTrans {
					if indepSets(qt, pt) {
						sleep[q] = qt
					}
				}
			}
			stack = append(stack, &dporFrame{
				proc:      tr.procs[d],
				enabled:   tr.enabled[d],
				backtrack: map[uint64]bool{tr.procs[d]: true},
				done:      map[uint64]bool{},
				doneTrans: map[uint64][]dporTrans{},
				sleep:     sleep,
			})
		}
		// Source-set race analysis over the ACCESS LOG: for every reversible race (a
		// concurrent dependent pair of log entries i<j), add a weak-initial of the
		// witness to the backtrack of the decision BEFORE log entry i — decision
		// accStep[i]-1, the choice point that ran i's goroutine (so a weak-initial run
		// there reaches j before i). The REORDERABILITY gate uses the SYNC
		// happens-before (dporClocks); the weak-initial / notdep computation uses the
		// TRACE happens-before (dporTraceClocks), which also orders conflicting accesses
		// so an independent access never becomes a spurious weak-initial. Both clocks
		// are over log entries.
		clk, pidx := dporClocks(tr)
		traceClk, tracePidx := dporTraceClocks(tr)
		nLog := len(tr.accSeq)
		for j := 0; j < nLog; j++ {
			for i := j - 1; i >= 0; i-- {
				if tr.accAddr[i] != 0 && tr.accAddr[i] == tr.accAddr[j] &&
					(tr.accWrite[i] || tr.accWrite[j]) && tr.accSeq[i] != tr.accSeq[j] {
					if dporConcurrent(clk, pidx, tr, i, j) {
						if d := tr.accStep[i] - 1; d >= 0 && d < n {
							addSourceBacktrack(stack[d], tr.enabled[d], tr, traceClk, tracePidx, i, j)
						}
					}
				}
			}
		}
		// Backtrack to the deepest decision with an unexplored, NON-ASLEEP backtrack
		// choice (deterministic enabled order); discard fully-explored frames. As each
		// chosen goroutine's subtree completes, record its interval access-set
		// (doneTrans — the prefix matched so stack[d].proc == procs[d]) so children
		// inherit it into their sleep set.
		picked := false
		for len(stack) > 0 {
			d := len(stack) - 1
			top := stack[d]
			top.done[top.proc] = true
			top.doneTrans[top.proc] = intervalSet[d]
			for _, g := range top.enabled {
				if top.backtrack[g] && !top.done[g] {
					if _, asleep := top.sleep[g]; asleep {
						continue
					}
					top.proc = g
					picked = true
					break
				}
			}
			if picked {
				break
			}
			stack = stack[:len(stack)-1]
		}
		if !picked {
			break
		}
	}
	res.Exhausted = !res.Overflow
	return res
}

// dporHB reports whether access a happens-before access b (a → b) per the vector
// clocks (a, b are ACCESS-LOG indices): b's clock has seen a's goroutine up to at
// least a's tick.
func dporHB(clk [][]uint32, pidx map[uint64]int, tr exploreTrace, a, b int) bool {
	pa := pidx[tr.accSeq[a]]
	return clk[b][pa] >= clk[a][pa]
}

// addSourceBacktrack adds a WEAK-INITIAL of the race witness for the concurrent
// dependent ACCESS-LOG pair (i, j), i<j, to fr.backtrack — where fr is the frame at
// the decision BEFORE log entry i (decision accStep[i]-1) and enabled is that
// decision's enabled set — so the reversed ordering e_i ⋖ e_j survives sleep-set
// pruning. i and j are ACCESS-LOG indices.
//
// The witness is the log entries after i, up to and including j, that e_i does NOT
// happen-before (so they could precede e_i's reversal) — always including j. A
// goroutine p is a WEAK-INITIAL of the witness iff its first witness entry has no
// earlier witness entry happening-before it: p can be scheduled first at the decision
// and lead toward e_j-before-e_i. Adding such a p (rather than e_j's goroutine
// directly) is the source-DPOR fix: e_j's goroutine may be asleep, but a weak-initial
// is by construction not sleep-blocked for this ordering.
//
// If fr.backtrack already holds an enabled weak-initial, nothing is added (source-set
// minimality). When NO weak-initial is enabled, the body skips (adds no backtrack):
// every goroutine that could lead to e_j has an intervening access that
// trace-happens-after e_i, so it is blocked until e_i runs, making the reversed order
// unreachable from this state — not a missed class. The hand-annotated sweeps never
// reach this branch; the dst-race compiler's denser auto-instrumentation does (a
// read-modify-write whose read and write both yield), and the DPOR-vs-Exhaustive
// equivalence checks validate that skipping there keeps DPOR complete (DST-L2-3).
func addSourceBacktrack(fr *dporFrame, enabled []uint64, tr exploreTrace, clk [][]uint32, pidx map[uint64]int, i, j int) {
	// Witness entries: k in (i, j], with k == j or e_i does NOT happen-before e_k.
	var witness []int
	for k := i + 1; k <= j; k++ {
		if k == j || !dporHB(clk, pidx, tr, i, k) {
			witness = append(witness, k)
		}
	}
	// Weak-initials: a goroutine whose FIRST witness entry is happens-before-minimal
	// within the witness (no earlier witness entry happens-before it).
	wi := map[uint64]bool{}
	seenProc := map[uint64]bool{}
	for idx, k := range witness {
		p := tr.accSeq[k]
		if seenProc[p] {
			continue
		}
		seenProc[p] = true
		minimal := true
		for _, m := range witness[:idx] {
			if dporHB(clk, pidx, tr, m, k) {
				minimal = false
				break
			}
		}
		if minimal {
			wi[p] = true
		}
	}
	// If an enabled weak-initial is already scheduled here, this reversal is covered.
	for _, g := range enabled {
		if wi[g] && fr.backtrack[g] {
			return
		}
	}
	// Add the lowest-dstSeq enabled weak-initial.
	have, best := false, uint64(0)
	for _, g := range enabled {
		if wi[g] && (!have || g < best) {
			have, best = true, g
		}
	}
	if have {
		fr.backtrack[best] = true
		return
	}
	// No weak-initial is enabled — the reversed order is UNREACHABLE from this state
	// (every goroutine that could lead to e_j is blocked until e_i runs); not a missed
	// Mazurkiewicz class, so add no backtrack. (Skipping, not an all-enabled add, which
	// under sleep sets could drop a class. The denser auto-instrumentation reaches this;
	// the DPOR-vs-Exhaustive equivalence checks validate it stays complete.)
}

// dporClocks computes a SYNC happens-before vector clock per ACCESS-LOG entry from
// program order plus the recorded goready edges, so dporConcurrent can test whether
// two accesses are causally ordered. clk[k] is access k's goroutine's clock snapshot
// right after access k's program-order tick; pidx maps a goroutine's stable index
// (dstSeq) to a vector position.
//
// Processing is in execution order, grouped by step (the access log is sorted by
// accStep, which is non-decreasing in execution order): for each step s, tick and
// snapshot every access in interval s (accStep == s), then apply every goready edge
// observed during interval s (edgeStep == s), flowing the readier's current clock into
// the readied's so its later accesses inherit the happens-before. A step with no
// accesses (a coarse-point-only interval) still applies its edges. Edges at step 0
// (before the first decision) flow zero clocks and are no-ops.
func dporClocks(tr exploreTrace) (clk [][]uint32, pidx map[uint64]int) {
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
	P := len(pidx)
	cur := make([][]uint32, P)
	for i := range cur {
		cur[i] = make([]uint32, P)
	}
	flow := func(from, to int) { // to = elementwise-max(to, from)
		for k := 0; k < P; k++ {
			if cur[from][k] > cur[to][k] {
				cur[to][k] = cur[from][k]
			}
		}
	}
	applyEdges := func(step int) {
		for e := range tr.edgeStep {
			if tr.edgeStep[e] == step {
				flow(pidx[tr.edgeFrom[e]], pidx[tr.edgeTo[e]])
			}
		}
	}
	maxStep := 0
	for _, s := range tr.accStep {
		if s > maxStep {
			maxStep = s
		}
	}
	for _, s := range tr.edgeStep {
		if s > maxStep {
			maxStep = s
		}
	}
	nLog := len(tr.accSeq)
	clk = make([][]uint32, nLog)
	applyEdges(0) // pre-first-decision edges (zero clocks; no-op, applied for faithfulness)
	li := 0
	for s := 1; s <= maxStep; s++ {
		for li < nLog && tr.accStep[li] == s {
			pi := pidx[tr.accSeq[li]]
			cur[pi][pi]++
			clk[li] = append([]uint32(nil), cur[pi]...)
			li++
		}
		applyEdges(s)
	}
	return clk, pidx
}

// dporTraceClocks computes the TRACE happens-before over ACCESS-LOG entries — the
// Mazurkiewicz partial order = the transitive closure of the DEPENDENCY relation in
// trace order, plus program order and the recorded sync (goready) edges. Unlike
// dporClocks (sync edges + program order only), it ALSO orders every pair of
// conflicting accesses (same nonzero address, >=1 write) by their log order: a later
// conflicting access causally depends on the earlier one in THIS execution. This is
// the relation source-DPOR's notdep / weak-initials need — "which accesses can move
// before e_i" = those NOT trace-happening-after e_i — so that an independent access
// (e.g. a concurrent read of a variable e_i wrote) does not become a spurious
// weak-initial. dporConcurrent deliberately still uses dporClocks: reorderability is a
// SYNC question, and under the trace order every conflicting pair is ordered, which is
// the wrong test for the race gate.
func dporTraceClocks(tr exploreTrace) (clk [][]uint32, pidx map[uint64]int) {
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
	applyEdges := func(step int) {
		for e := range tr.edgeStep {
			if tr.edgeStep[e] == step {
				mergeInto(cur[pidx[tr.edgeTo[e]]], cur[pidx[tr.edgeFrom[e]]])
			}
		}
	}
	maxStep := 0
	for _, s := range tr.accStep {
		if s > maxStep {
			maxStep = s
		}
	}
	for _, s := range tr.edgeStep {
		if s > maxStep {
			maxStep = s
		}
	}
	nLog := len(tr.accSeq)
	clk = make([][]uint32, nLog)
	applyEdges(0)
	li := 0
	for s := 1; s <= maxStep; s++ {
		for li < nLog && tr.accStep[li] == s {
			pi := pidx[tr.accSeq[li]]
			// Conflict edges: a later access to the same address with >=1 write causally
			// depends on every earlier conflicting access — merge their clocks in, so e_i
			// trace-happens-before every later conflicting access.
			for m := 0; m < li; m++ {
				if tr.accAddr[m] != 0 && tr.accAddr[m] == tr.accAddr[li] && (tr.accWrite[m] || tr.accWrite[li]) {
					mergeInto(cur[pi], clk[m])
				}
			}
			cur[pi][pi]++
			clk[li] = append([]uint32(nil), cur[pi]...)
			li++
		}
		applyEdges(s)
	}
	return clk, pidx
}

// dporConcurrent reports whether accesses i and j (i<j ACCESS-LOG indices, different
// goroutines) are concurrent — neither sync-happens-before the other. Access a
// happens-before b iff b's clock has seen a's goroutine up to at least a's tick; the
// pair is concurrent iff neither direction holds.
func dporConcurrent(clk [][]uint32, pidx map[uint64]int, tr exploreTrace, i, j int) bool {
	pi := pidx[tr.accSeq[i]]
	pj := pidx[tr.accSeq[j]]
	iBeforeJ := clk[j][pi] >= clk[i][pi]
	jBeforeI := clk[i][pj] >= clk[j][pj]
	return !iBeforeJ && !jBeforeI
}

// encodePrefix packs a decision prefix into a string key for the visited set
// (no fmt dependency: 8 little-endian bytes per goid).
func encodePrefix(p []uint64) string {
	b := make([]byte, len(p)*8)
	for i, v := range p {
		for j := 0; j < 8; j++ {
			b[i*8+j] = byte(v >> (8 * j))
		}
	}
	return string(b)
}

func clonePrefix(p []uint64) []uint64 {
	c := make([]uint64, len(p))
	copy(c, p)
	return c
}
