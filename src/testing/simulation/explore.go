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
}

// ExploreResult reports an Explore run.
type ExploreResult struct {
	// Schedules is the number of interleavings actually explored.
	Schedules int
	// Failures lists every explored interleaving in which the SUT returned true,
	// each with its reproducing schedule.
	Failures []Failure
	// Exhausted is true iff the (pruned) interleaving space was fully covered. It
	// is false when Overflow truncated coverage.
	Exhausted bool
	// Overflow is true iff some run exceeded the per-bubble trace budget; coverage
	// is then INCOMPLETE (reported, never silently capped).
	Overflow bool
}

// Per-bubble trace budget. An over-budget run sets ExploreResult.Overflow and the
// space is reported not-exhausted, rather than silently capping coverage.
const (
	exploreMaxDecisions    = 1 << 14
	exploreMaxEnabledTotal = 1 << 18
	exploreMaxEdges        = 1 << 16
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
	dstExploreInit(exploreMaxDecisions, exploreMaxEnabledTotal, exploreMaxEdges)
	if mode == DPOR {
		return dporExplore(seed, sut)
	}
	return exhaustiveExplore(seed, sut)
}

// exploreTrace is one scheduled run's recorded decision trace, copied out of the
// runtime's per-bubble buffers (which the next run overwrites).
type exploreTrace struct {
	procs   []uint64   // [decision] chosen goroutine (stable per-bubble index, dstSeq)
	addrs   []uintptr  // [decision] transition's access address (0 = none)
	writes  []bool     // [decision] transition's access is a write
	enabled [][]uint64 // [decision] enabled goroutine set (dstSeq indices)
	// Happens-before edges (goready: edgeFrom happens-before edgeTo's resumption),
	// edgeStep = the dstScheduleStep when the goready fired = (transition index)+1.
	edgeFrom []uint64
	edgeTo   []uint64
	edgeStep []int
	aborted  bool // prefix named a non-enabled goroutine (a replay-determinism bug)
	overflow bool // run exceeded the trace or edge budget (coverage incomplete)
}

// runOnce runs sut once under the scheduled strategy following prefix, and copies
// out the recorded trace. failed is sut's verdict for this interleaving.
func runOnce(seed uint64, prefix []uint64, sut func() bool) (failed bool, tr exploreTrace) {
	run(seed, kindScheduled, 0, 0, defaultHostname, defaultPID, defaultNumCPU, 0, prefix, func() {
		failed = sut()
	})
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
	n := dstTraceLenFP()
	tr.procs = make([]uint64, n)
	tr.addrs = make([]uintptr, n)
	tr.writes = make([]bool, n)
	tr.enabled = make([][]uint64, n)
	for i := 0; i < n; i++ {
		tr.procs[i] = dstTraceChosenFP(i)
		tr.addrs[i], tr.writes[i] = dstTraceAccessFP(i)
		tr.enabled[i] = append([]uint64(nil), dstTraceEnabledFP(i)...) // copy: aliases runtime buffer
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
		failed, tr := runOnce(seed, prefix, sut)
		res.Schedules++
		if tr.overflow {
			res.Overflow = true
		}
		if failed {
			res.Failures = append(res.Failures, Failure{Schedule: clonePrefix(prefix)})
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

// dporFrame is one decision on the DPOR DFS stack.
type dporFrame struct {
	proc      uint64          // the goroutine currently chosen at this decision
	enabled   []uint64        // enabled goroutines at this decision (deterministic order)
	backtrack map[uint64]bool // goroutines that must be explored here
	done      map[uint64]bool // goroutines already explored here
}

// dporExplore is iterative stateless Dynamic Partial-Order Reduction. It explores
// one interleaving per Mazurkiewicz equivalence class: two transitions are
// *dependent* iff they record the same nonzero conflict identity with at least one
// write, by different goroutines — where the identity is a shared memory address
// (dstAccessYield) OR a synchronization object's identity (dstSyncAcquire, recording
// a mutex/channel acquisition as a write-conflict so its acquisition ORDER is a
// dependency; see runtime/dst_explore.go and design.md "Completeness boundary").
// Only orderings of dependent transitions are explored (independent reorderings are
// provably equivalent and pruned). Each run re-executes from the start following the
// stack's chosen prefix; the dependency analysis over the resulting trace adds
// backtrack points at ancestor decisions.
func dporExplore(seed uint64, sut func() bool) ExploreResult {
	var res ExploreResult
	var stack []*dporFrame
	for {
		prefix := make([]uint64, len(stack))
		for i, fr := range stack {
			prefix[i] = fr.proc
		}
		failed, tr := runOnce(seed, prefix, sut)
		res.Schedules++
		if tr.overflow {
			res.Overflow = true
		}
		if failed {
			res.Failures = append(res.Failures, Failure{Schedule: clonePrefix(prefix)})
		}
		// runOnce panics on a divergent (aborted) replay, so the trace here is always
		// a faithful replay of the followed prefix.
		n := len(tr.procs)
		// Extend the stack with the transitions this run newly revealed.
		for d := len(stack); d < n; d++ {
			stack = append(stack, &dporFrame{
				proc:      tr.procs[d],
				enabled:   tr.enabled[d],
				backtrack: map[uint64]bool{tr.procs[d]: true},
				done:      map[uint64]bool{},
			})
		}
		// Dependency analysis: two transitions are dependent iff they record the same
		// nonzero conflict identity with at least one write, by different goroutines,
		// AND are CONCURRENT (neither happens-before the other — clocks computed from
		// the recorded goready edges + program order). The identity is a memory address
		// (dstAccessYield) or a synchronization object's identity (dstSyncAcquire); the
		// concurrency test prunes mutex/channel-SERIALIZED pairs the identity relation
		// would over-explore, while an acquisition ORDER (which contender acquires
		// first) is a co-enabled concurrent conflicting pair and IS explored both ways.
		// For each dependent pair (i<j) the reverse ordering must be explored — add a
		// backtrack at decision i to run j's goroutine there (if it was enabled at i;
		// else, conservatively, every goroutine enabled at i).
		//
		// addr==0 transitions are treated as independent of everything. Those are pure
		// infrastructure decisions that record neither a memory access nor a sync
		// acquisition — goroutine creation, WaitGroup wakeups, and the finalizer-drain
		// goroutine (gcDrain is also isolated from the schedule in firstSystemG): they
		// carry no outcome-determining order choice a recorded access/acquisition does
		// not already capture. Sound and complete for SUTs whose shared accesses AND
		// synchronization acquisitions are recorded (dstAccessYield + dstSyncAcquire)
		// and that do not observe finalizer/cleanup timing — enforced over a generated
		// family by TestDSTExploreSweep (DPOR outcome set == exhaustive). The dst-race
		// compiler/runtime phase records accesses and acquisitions automatically,
		// removing the manual-annotation assumption. See design.md "Completeness
		// boundary (addr=0 transitions)".
		clk, pidx := dporClocks(tr)
		for j := 0; j < n; j++ {
			for i := j - 1; i >= 0; i-- {
				if tr.addrs[i] != 0 && tr.addrs[i] == tr.addrs[j] &&
					(tr.writes[i] || tr.writes[j]) && tr.procs[i] != tr.procs[j] &&
					dporConcurrent(clk, pidx, tr, i, j) {
					if containsU(tr.enabled[i], tr.procs[j]) {
						stack[i].backtrack[tr.procs[j]] = true
					} else {
						for _, g := range tr.enabled[i] {
							stack[i].backtrack[g] = true
						}
					}
				}
			}
		}
		// Backtrack to the deepest decision with an unexplored backtrack choice
		// (picked in deterministic enabled order), discarding fully-explored frames.
		picked := false
		for len(stack) > 0 {
			top := stack[len(stack)-1]
			top.done[top.proc] = true
			for _, g := range top.enabled {
				if top.backtrack[g] && !top.done[g] {
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

func containsU(s []uint64, v uint64) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// dporClocks computes a vector clock per transition from program order plus the
// recorded goready happens-before edges, so dporConcurrent can test whether two
// transitions are causally ordered. clk[i] is transition i's proc's vector clock
// snapshot right after its program-order tick; pidx maps a goroutine's stable
// index (dstSeq) to a vector position.
//
// Processing is in execution (trace) order: at transition i, tick proc_i's own
// component and snapshot; then apply every goready edge observed during that
// transition (edgeStep == i+1, since the transition chosen at decision i runs with
// dstScheduleStep == i+1), flowing the readier's current clock into the readied's
// (so the readied's later transitions inherit the happens-before). Edges recorded
// before the first decision (step 0) flow a zero clock and are no-ops.
func dporClocks(tr exploreTrace) (clk [][]uint32, pidx map[uint64]int) {
	pidx = make(map[uint64]int)
	addProc := func(p uint64) {
		if _, ok := pidx[p]; !ok {
			pidx[p] = len(pidx)
		}
	}
	for _, p := range tr.procs {
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
	applyEdges(0) // pre-first-decision edges (zero clocks; no-op, applied for faithfulness)
	n := len(tr.procs)
	clk = make([][]uint32, n)
	for i := 0; i < n; i++ {
		pi := pidx[tr.procs[i]]
		cur[pi][pi]++
		clk[i] = append([]uint32(nil), cur[pi]...)
		applyEdges(i + 1) // goready edges observed while transition i ran
	}
	return clk, pidx
}

// dporConcurrent reports whether transitions i and j (i<j, different goroutines)
// are concurrent — neither happens-before the other. Transition a happens-before b
// iff b's clock has seen a's proc up to at least a's tick; the pair is concurrent
// iff neither direction holds.
func dporConcurrent(clk [][]uint32, pidx map[uint64]int, tr exploreTrace, i, j int) bool {
	pi := pidx[tr.procs[i]]
	pj := pidx[tr.procs[j]]
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
