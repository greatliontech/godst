// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

// Level-2 systematic interleaving exploration. Explore drives repeated bubble
// re-executions of a SUT, each following an explicit scheduling-decision prefix
// (the scheduled strategy, runtime/dst_explore.go), and enumerates the sound
// interleavings — exhaustively, or (later) pruned by DPOR. The runtime is a pure
// schedule-follower + trace-recorder; the exploration "brain" lives here, offline
// between Runs. See docs/dst/exploration.md "Level 2 — access-granularity interleaving
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

// AccessForce identifies one auto-instrumented access hook that must yield during
// replay. It is part of a Failure's replay token for races discovered while the
// shared-address filter is still promoting inline accesses. The PC key is relative to
// the containing function, not an absolute code address, so it survives fresh-process
// replay of the same binary.
type AccessForce struct {
	// Seq is the accessing goroutine's stable per-bubble index.
	Seq uint64
	// Count is that goroutine's hook ordinal.
	Count uint64
	// PCKey is a stable key for the hook call site: function name hash plus PC offset.
	PCKey uintptr
}

// Failure records an interleaving under which the SUT reported a bug.
type Failure struct {
	// Schedule is the decision prefix — the stable per-bubble goroutine indices
	// (dstSeq, not goid) chosen in decision order.
	Schedule []uint64
	// AccessForces are the forced access-yield watchpoints active when this failure was
	// observed. Replaying a race first found before promotion convergence requires these
	// in addition to Schedule, because TSan dedups race reports process-wide.
	AccessForces []AccessForce
	// Race is true iff the -race detector reported a NEW data race during this
	// schedule's run (the D5 oracle). False means the failure is a SUT assertion
	// (sut returned true), a SUT panic (Panic != ""), or a synctest deadlock
	// (Deadlock != ""). In a non-race build Race is always false. The detector
	// dedups by signature, so each distinct race yields exactly one Race failure:
	// the first schedule that exhibits it.
	Race bool
	// Panic is non-empty iff the SUT panicked while executing this schedule, including
	// from a SUT-created child goroutine or finalizer/cleanup callback. The Schedule
	// and AccessForces replay the same interleaving; Replay panics again for panic
	// failures.
	Panic string
	// Deadlock is non-empty iff the schedule ended with a synctest deadlock. Replay
	// panics with the same deadlock marker for deadlock failures.
	Deadlock string
	// CrashTear records whether the exploration that found this failure was
	// tearing host crashes (ExploreOptions.CrashTear). Replay restores the same
	// policy: the fault RNG draws that shaped the wreckage are part of the
	// execution, so a failure found under a torn crash reproduces only under one
	// (DST-FAULT-REPLAY).
	CrashTear bool
}

// ExploreResult reports an Explore run.
type ExploreResult struct {
	// Schedules is the number of interleavings actually explored.
	Schedules int
	// Failures lists every explored interleaving that exhibited a bug: the SUT returned
	// true (an assertion failure), panicked (Failure.Panic), deadlocked
	// (Failure.Deadlock), or the -race detector reported a new data race
	// (Failure.Race; see D5), each with its replay metadata.
	Failures []Failure
	// Exhausted is true iff the (pruned) interleaving space was fully covered. It
	// is false when Overflow truncated coverage.
	Exhausted bool
	// Overflow is true iff some run exceeded the per-bubble trace, edge, or access-log
	// budget; coverage is then INCOMPLETE (reported, never silently capped).
	Overflow bool
	// BudgetHit is true iff exploration stopped at a caller-supplied MaxSchedules or
	// MaxSteps budget. Coverage is then incomplete and Exhausted is false.
	BudgetHit bool
}

// ExploreOptions configures ExploreWith. The zero value is exhaustive exploration
// with the implementation's internal trace budgets and no caller-imposed cap.
type ExploreOptions struct {
	// Mode selects the exploration algorithm: Exhaustive or DPOR. The zero value is
	// Exhaustive, matching Explore's explicit mode argument.
	Mode ExploreMode
	// MaxSchedules, if > 0, stops after exploring this many schedules and reports
	// BudgetHit unless the interleaving space is exhausted exactly at that boundary.
	MaxSchedules int
	// CrashTear makes CrashHost tear at page granularity rather than losing every
	// unsynced byte, exactly as Options.CrashTear does for Run. Each explored
	// schedule draws its crash outcomes from the per-bubble fault RNG, so a
	// schedule that finds a crash-recovery bug replays it exactly.
	CrashTear bool
	// MaxSteps, if > 0, bounds the scheduling decisions recorded in any one run.
	// Hitting it reports BudgetHit rather than silently treating the truncated trace
	// as exhausted.
	MaxSteps int
}

type exploreConfig struct {
	maxDecisions    int
	maxEnabledTotal int
	maxEdges        int
	maxAccesses     int
	maxSchedules    int
	maxSteps        int
}

// Per-bubble trace budget. An over-budget run sets ExploreResult.Overflow and the
// space is reported not-exhausted, rather than silently capping coverage.
const (
	exploreMaxDecisions    = 1 << 14
	exploreMaxEnabledTotal = 1 << 18
	exploreMaxEdges        = 1 << 16
	exploreMaxAccesses     = 1 << 16
)

func defaultExploreConfig() exploreConfig {
	return exploreConfig{
		maxDecisions:    exploreMaxDecisions,
		maxEnabledTotal: exploreMaxEnabledTotal,
		maxEdges:        exploreMaxEdges,
		maxAccesses:     exploreMaxAccesses,
	}
}

func exploreConfigFromOptions(opts ExploreOptions) exploreConfig {
	cfg := defaultExploreConfig()
	cfg.maxSchedules = opts.MaxSchedules
	cfg.maxSteps = opts.MaxSteps
	if opts.MaxSteps > 0 {
		cfg.maxDecisions = opts.MaxSteps
		// Keep enough enabled-set room for ordinary tests while still letting a
		// pathological fan-out hit the same explicit step budget instead of an
		// unreported internal cap.
		cfg.maxEnabledTotal = opts.MaxSteps * 64
		if cfg.maxEnabledTotal < opts.MaxSteps {
			cfg.maxEnabledTotal = opts.MaxSteps
		}
	}
	return cfg
}

// Explore systematically explores the sound interleavings of sut under seed. sut
// returns true iff THIS interleaving exhibited a bug (a failed assertion); Explore
// records every such schedule. The seed fixes the program's data randomness
// (select/map/math+crypto rand); the interleaving is controlled by Explore,
// independent of the seed. Requires building with -tags dst (and -race to also use
// the happens-before detector as an oracle within each interleaving). Explore is a
// process-global simulation operation: it must not overlap any other simulation
// operation in one process, and must not be called from within a synctest bubble.
//
// Exhaustive enumerates the whole decision tree. DPOR explores one interleaving
// per Mazurkiewicz equivalence class, finding the same bugs with far fewer runs.
//
// Boundary: exploration branches on recorded transitions — memory accesses,
// channel/mutex/RWMutex decisions, sync/atomic operations (free functions and
// the typed APIs, recorded at instrumented STATIC call sites), and len(ch)
// reads (cap(ch) is immutable after make and carries no ordering decision).
// Atomic call forms that record no transition: calls from inside
// non-instrumented packages (the runtime; a norace package like sync when the
// call is not inlined into instrumented code — the sync packages' own
// decision hooks cover their primitives), and dynamic forms — interface
// dispatch on an atomic value, method values (f := x.Load), and func-valued
// free functions — plus //go:nosplit callers and embedded-promotion tail-call
// wrappers. An outcome-determining atomic
// reached only through those forms may under-explore even when Exhausted
// reports true.
func Explore(seed uint64, mode ExploreMode, sut func() bool) ExploreResult {
	return ExploreWith(seed, ExploreOptions{Mode: mode}, sut)
}

// ExploreWith is Explore with caller-supplied exploration budgets. Budgeted runs
// report BudgetHit and never report Exhausted for truncated coverage. It has the
// same process-global non-overlap restriction as Explore.
func ExploreWith(seed uint64, opts ExploreOptions, sut func() bool) ExploreResult {
	enterSimulation("Explore", "testing/simulation: Explore requires building with -tags dst")
	defer leaveSimulation()
	setCrashTear(opts.CrashTear)
	cfg := exploreConfigFromOptions(opts)
	dstExploreInit(cfg.maxDecisions, cfg.maxEnabledTotal, cfg.maxEdges, cfg.maxAccesses)
	if opts.Mode == DPOR {
		return dporExplore(seed, sut, cfg)
	}
	return exhaustiveExplore(seed, sut, cfg)
}

// Replay executes sut once under failure's recorded schedule and access-force set.
// It returns the SUT assertion verdict and whether the race detector reported a new
// race during this replay. For race failures, run Replay in a fresh process if the
// same process already observed that race, because TSan dedups reports by signature.
// Replay is a process-global simulation operation and must not overlap any other
// simulation operation in one process.
//
// raced reports whether the race detector fired during the replay; it does not
// verify the replayed race is the SAME race the original exploration found. A
// different (new) race also reports raced=true — confirm specific races in a
// fresh process, where the detector's per-process dedup cannot suppress them.
func Replay(seed uint64, failure Failure, sut func() bool) (failed, raced bool) {
	enterSimulation("Replay", "testing/simulation: Replay requires building with -tags dst")
	defer leaveSimulation()
	// The crash policy is part of the execution the failure recorded: replaying a
	// torn crash untorn would restore a different disk and not reproduce.
	setCrashTear(failure.CrashTear)
	cfg := defaultExploreConfig()
	dstExploreInit(cfg.maxDecisions, cfg.maxEnabledTotal, cfg.maxEdges, cfg.maxAccesses)
	r := runOnceResultLocked(seed, failure.Schedule, accessForceMap(failure.AccessForces), sut, cfg)
	if r.panic != "" {
		panic(r.panic)
	}
	if r.deadlock != "" {
		panic(r.deadlock)
	}
	return r.failed, r.raceCount > 0
}

// exploreTrace is one scheduled run's recorded decision trace, copied out of the
// runtime's per-bubble buffers (which the next run overwrites).
type exploreTrace struct {
	procs   []uint64   // [decision] chosen goroutine (stable per-bubble index, dstSeq)
	enabled [][]uint64 // [decision] enabled goroutine set (dstSeq indices)
	// abortStep/panicStep (-1 = unset): decision step of a prefix abort and of the
	// first runtime-recorded SUT panic, for panic-truncation attribution — an abort
	// BEFORE the panic step is a genuine DST-L2-2 violation, not truncation fallout.
	abortStep int32
	panicStep int32
	// syncEventOverflow: the offline sync-event log dropped events; weak-initial
	// precision is compromised for this trace, so source-backtracking degrades to
	// the all-enabled over-approximation (sound: over-explore, never prune).
	syncEventOverflow bool
	// Access log: EVERY instrumented access in execution order, decoupled from the
	// decision trace (a single-owner access records here without yielding, so it is
	// not a decision). DPOR's dependency/HB relation is sourced from this log, not from
	// per-decision addrs. accStep[k] is the dstScheduleStep the access occurred under:
	// an access with step s was performed by procs[s-1] during the interval after
	// decision s-1, so its reversal anchors at decision s-1. accAddr+accSize is the
	// byte interval used for range-aware conflict checks. accPC (a stable PC key) + accCount identify a
	// filtered hook call precisely enough to force it to yield on replay if a later
	// conflict proves that interval needed a split.
	accSeq   []uint64
	accAddr  []uintptr
	accSize  []uintptr
	accPC    []uintptr
	accCount []uint64
	accWrite []bool
	accStep  []int
	// Happens-before edges (goready/create: edgeFrom happens-before edgeTo's resumption).
	// edgeStep is the dstScheduleStep when the edge fired; edgeAcc is the access-log
	// length at that moment, which orders same-step edges against inline filtered accesses.
	edgeFrom  []uint64
	edgeTo    []uint64
	edgeStep  []int
	edgeAcc   []int
	edgeOrder []int
	// Sync events are release/acquire operations on synchronization objects (mutex
	// unlock/lock, buffered channel slot send/receive). They are consumed as object
	// clocks so an acquire observes the release-time snapshot, not the releasing
	// goroutine's later accesses.
	syncKind  []uint8
	syncID    []uintptr
	syncAux   []uintptr
	syncSeq   []uint64
	syncStep  []int
	syncAcc   []int
	syncOrd   []int
	aborted   bool // prefix named a non-enabled goroutine (a replay-determinism bug)
	overflow  bool // run exceeded the trace, edge, or access-log budget (coverage incomplete)
	budgetHit bool // run exceeded a caller-supplied per-run step budget
}

type accessForce struct {
	seq   uint64
	count uint64
	pc    uintptr
}

func accessForceMap(forces []AccessForce) map[accessForce]bool {
	m := make(map[accessForce]bool, len(forces))
	for _, f := range forces {
		m[accessForce{seq: f.Seq, count: f.Count, pc: f.PCKey}] = true
	}
	return m
}

func accessForceLess(a, b AccessForce) bool {
	if a.Seq != b.Seq {
		return a.Seq < b.Seq
	}
	if a.Count != b.Count {
		return a.Count < b.Count
	}
	return a.PCKey < b.PCKey
}

func cloneAccessForces(forces map[accessForce]bool) []AccessForce {
	out := make([]AccessForce, 0, len(forces))
	for f := range forces {
		out = append(out, AccessForce{Seq: f.seq, Count: f.count, PCKey: f.pc})
	}
	for i := 1; i < len(out); i++ {
		v := out[i]
		j := i
		for j > 0 && accessForceLess(v, out[j-1]) {
			out[j] = out[j-1]
			j--
		}
		out[j] = v
	}
	return out
}

func newFailure(prefix []uint64, raced bool, panicMsg, deadlockMsg string, forces map[accessForce]bool) Failure {
	return Failure{Schedule: clonePrefix(prefix), AccessForces: cloneAccessForces(forces), Race: raced, Panic: panicMsg, Deadlock: deadlockMsg, CrashTear: crashTearEnabled()}
}

func installAccessForces(forces map[accessForce]bool) {
	if len(forces) == 0 {
		dstSetAccessForce(nil, nil, nil)
		return
	}
	seq := make([]uint64, 0, len(forces))
	count := make([]uint64, 0, len(forces))
	pc := make([]uintptr, 0, len(forces))
	for f := range forces {
		seq = append(seq, f.seq)
		count = append(count, f.count)
		pc = append(pc, f.pc)
	}
	dstSetAccessForce(seq, count, pc)
}

// runOnce runs sut once under the scheduled strategy following prefix, with the
// current forced access-yield watchpoints installed, and copies out the recorded
// trace. failed is sut's verdict for this interleaving; raced is true iff the -race
// detector reported at least one NEW data race during this run (D5 oracle; always
// false in a non-race build — dstRaceErrors returns 0). It is kept for white-box
// tests; Explore uses runOnceResult so it can report panic failures and multiple
// new races separately.
func runOnce(seed uint64, prefix []uint64, forces map[accessForce]bool, sut func() bool) (failed, raced bool, tr exploreTrace) {
	r := runOnceResult(seed, prefix, forces, sut, defaultExploreConfig())
	if r.panic != "" {
		panic(r.panic)
	}
	if r.deadlock != "" {
		panic(r.deadlock)
	}
	return r.failed, r.raceCount > 0, r.tr
}

type runResult struct {
	failed    bool
	raceCount int
	panic     string
	deadlock  string
	tr        exploreTrace
}

func panicString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		if x == "" {
			return "empty panic"
		}
		return x
	case error:
		if x.Error() == "" {
			return "empty panic"
		}
		return x.Error()
	default:
		return "non-string panic"
	}
}

func runOnceResult(seed uint64, prefix []uint64, forces map[accessForce]bool, sut func() bool, cfg exploreConfig) (out runResult) {
	enterSimulation("Run", "testing/simulation: Run requires building with -tags dst (for a reproducible map hash key)")
	defer leaveSimulation()
	return runOnceResultLocked(seed, prefix, forces, sut, cfg)
}

func runOnceResultLocked(seed uint64, prefix []uint64, forces map[accessForce]bool, sut func() bool, cfg exploreConfig) (out runResult) {
	racesBefore := dstRaceErrors()
	installAccessForces(forces)
	var failed bool
	defer func() {
		out.failed = failed
		out.raceCount = dstRaceErrors() - racesBefore
		if out.raceCount < 0 {
			out.raceCount = 0
		}
		out.tr = copyExploreTrace(cfg.maxSteps > 0)
		if out.panic == "" {
			if v, ok := dstExplorePanicFP(); ok {
				out.panic = panicString(v)
			}
		}
		out.deadlock = dstExploreDeadlockFP()
		// A recorded SUT panic legitimately truncates the run (the panicking
		// goroutine dies, so later prefix entries naming it are not enabled) and
		// the abort flag is then expected — but ONLY for an abort at or after the
		// panic's decision step: a divergence BEFORE the panic point is a genuine
		// determinism violation a coinciding panic must not mask (exploration.md,
		// hardening clause 4). A harness-recovered panic with no runtime-recorded
		// step (panicStep < 0, e.g. a root-goroutine panic) keeps the lenient
		// attribution: its truncation point is unknown.
		if out.tr.aborted {
			excused := out.panic != "" &&
				(out.tr.panicStep < 0 || out.tr.abortStep < 0 || out.tr.abortStep >= out.tr.panicStep)
			if !excused {
				panic("testing/simulation: internal error: schedule prefix diverged on replay " +
					"(a goroutine in the prefix was not enabled at its decision) — DST-L2-2 violation")
			}
		}
	}()
	// The scheduled-replay strategy carries no network config (latency/jitter/bandwidth
	// are 0, as everywhere here); take the send-buffer/retransmit DEFAULTS from the one
	// source of truth (resolveNetConfig) so a default change can't silently desync them.
	defSendBuf, defRetransNs := resolveNetConfig(NetworkConfig{})
	runLocked(seed, kindScheduled, 0, 0, defaultHostname, defaultPID, defaultNumCPU, 0, 0, 0, 0, defSendBuf, defRetransNs, prefix, false, func() {
		defer func() {
			if v := recover(); v != nil && out.panic == "" {
				if pv, ok := dstExplorePanicFP(); ok {
					out.panic = panicString(pv)
				} else {
					out.panic = panicString(v)
				}
			}
		}()
		failed = sut()
	})
	return out
}

func copyExploreTrace(stepBudget bool) (tr exploreTrace) {
	traceOverflow := dstTraceOverflowFP()
	tr.budgetHit = stepBudget && traceOverflow
	tr.syncEventOverflow = dstSyncEventOverflowFP()
	tr.overflow = (traceOverflow && !tr.budgetHit) || dstEdgeOverflowFP() || tr.syncEventOverflow
	tr.aborted = dstScheduleAbortedFP()
	tr.abortStep = dstScheduleAbortStepFP()
	tr.panicStep = dstExplorePanicStepFP()
	if tr.aborted {
		// The prefix named a goroutine not enabled at its decision: this replay
		// diverged from the run that produced the prefix. Since every prefix is
		// derived from a recorded trace, that is a replay-determinism (DST-L2-2)
		// violation that must never happen; runOnceResult surfaces it loudly after
		// copying the trace.
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
	tr.accSize = make([]uintptr, a)
	tr.accPC = make([]uintptr, a)
	tr.accCount = make([]uint64, a)
	tr.accWrite = make([]bool, a)
	tr.accStep = make([]int, a)
	for i := 0; i < a; i++ {
		tr.accSeq[i], tr.accAddr[i], tr.accSize[i], tr.accPC[i], tr.accCount[i], tr.accWrite[i], tr.accStep[i] = dstAccLogAtFP(i)
	}
	m := dstEdgeLenFP()
	tr.edgeFrom = make([]uint64, m)
	tr.edgeTo = make([]uint64, m)
	tr.edgeStep = make([]int, m)
	tr.edgeAcc = make([]int, m)
	tr.edgeOrder = make([]int, m)
	for i := 0; i < m; i++ {
		tr.edgeFrom[i], tr.edgeTo[i], tr.edgeStep[i], tr.edgeAcc[i] = dstEdgeAtFP(i)
		tr.edgeOrder[i] = dstEdgeOrderFP(i)
	}
	s := dstSyncEventLenFP()
	tr.syncKind = make([]uint8, s)
	tr.syncID = make([]uintptr, s)
	tr.syncAux = make([]uintptr, s)
	tr.syncSeq = make([]uint64, s)
	tr.syncStep = make([]int, s)
	tr.syncAcc = make([]int, s)
	tr.syncOrd = make([]int, s)
	for i := 0; i < s; i++ {
		tr.syncKind[i], tr.syncID[i], tr.syncAux[i], tr.syncSeq[i], tr.syncStep[i], tr.syncAcc[i], tr.syncOrd[i] = dstSyncEventAtFP(i)
	}
	return
}

// exhaustiveExplore enumerates every distinct interleaving by DFS over the
// scheduling-decision tree: each run reveals its decisions, and every not-taken
// enabled goroutine at every free decision is queued as a child prefix.
func exhaustiveExplore(seed uint64, sut func() bool, cfg exploreConfig) ExploreResult {
	forces := map[accessForce]bool{}
	var carriedRace []Failure
	totalSchedules := 0
	for {
		passCfg, ok := explorePassConfig(cfg, totalSchedules)
		if !ok {
			return ExploreResult{Schedules: totalSchedules, Failures: carriedRace, BudgetHit: true}
		}
		res, grew := exhaustiveExplorePass(seed, sut, forces, passCfg)
		totalSchedules += res.Schedules
		res.Schedules = totalSchedules
		if !grew {
			if len(carriedRace) != 0 {
				res.Failures = append(carriedRace, res.Failures...)
			}
			return res
		}
		if cfg.maxSchedules > 0 && totalSchedules >= cfg.maxSchedules {
			res.BudgetHit = true
			res.Exhausted = false
			if len(carriedRace) != 0 {
				res.Failures = append(carriedRace, res.Failures...)
			}
			return res
		}
		// Assertion failures from an obsolete force set are not replayable under the next
		// pass. Race failures are process-global TSan reports and may be deduped before the
		// converged pass, so keep the first observing replay token.
		for _, f := range res.Failures {
			if f.Race {
				carriedRace = append(carriedRace, f)
			}
		}
	}
}

// checkReplayPrefix verifies DST-L2-2 over a replayed schedule prefix: each frame's
// recorded decision and enabled set must match the replay's, over the part of the
// prefix that actually ran (a recorded panic legitimately truncates the tail; the
// truncated part was never replayed, so there is nothing to compare there — the
// panic-attribution check in runOnceResult covers the abort semantics).
func checkReplayPrefix(stack []*dporFrame, tr exploreTrace) {
	n := len(stack)
	if len(tr.procs) < n {
		n = len(tr.procs)
	}
	for d := 0; d < n; d++ {
		if tr.procs[d] != stack[d].proc || !equalSeqSets(tr.enabled[d], stack[d].enabled) {
			panic("testing/simulation: internal error: replay diverged from the recorded prefix " +
				"(decision or enabled set changed at a followed step) — DST-L2-2 violation (nondeterministic SUT?)")
		}
	}
}

// checkReplayEnabled is checkReplayPrefix's form for a prefix whose parent enabled
// sets were carried explicitly (the exhaustive pass). The final prefix entry is the
// forced flip g — the enabled set there must still match the parent's; the chosen
// procs before it must equal the prefix (the runtime follows it or aborts).
func checkReplayEnabled(prefix []uint64, parentEnabled [][]uint64, tr exploreTrace) {
	n := len(parentEnabled)
	if len(tr.procs) < n {
		n = len(tr.procs)
	}
	for d := 0; d < n; d++ {
		if (d < len(prefix) && tr.procs[d] != prefix[d]) || !equalSeqSets(tr.enabled[d], parentEnabled[d]) {
			panic("testing/simulation: internal error: replay diverged from the recorded prefix " +
				"(decision or enabled set changed at a followed step) — DST-L2-2 violation (nondeterministic SUT?)")
		}
	}
}

func equalSeqSets(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func explorePassConfig(cfg exploreConfig, schedules int) (exploreConfig, bool) {
	if cfg.maxSchedules <= 0 {
		return cfg, true
	}
	remaining := cfg.maxSchedules - schedules
	if remaining <= 0 {
		return cfg, false
	}
	cfg.maxSchedules = remaining
	return cfg, true
}

func exhaustiveExplorePass(seed uint64, sut func() bool, forces map[accessForce]bool, cfg exploreConfig) (ExploreResult, bool) {
	var res ExploreResult
	visited := map[string]bool{}
	// Each queued prefix carries the parent trace's enabled sets over its length
	// (aliasing the parent's copied slices), so the replay can be cross-checked
	// against the recorded decisions — the divergence that keeps the named seqs
	// enabled slips the runtime's non-enabled abort (hardening clause 4).
	type queued struct {
		prefix        []uint64
		parentEnabled [][]uint64
	}
	stack := []queued{{nil, nil}}
	for len(stack) > 0 {
		if cfg.maxSchedules > 0 && res.Schedules >= cfg.maxSchedules {
			res.BudgetHit = true
			break
		}
		q := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if k := encodePrefix(q.prefix); visited[k] {
			continue
		} else {
			visited[k] = true
		}
		prefix := q.prefix
		r := runOnceResultLocked(seed, prefix, forces, sut, cfg)
		tr := r.tr
		checkReplayEnabled(prefix, q.parentEnabled, tr)
		res.Schedules++
		if tr.budgetHit {
			res.BudgetHit = true
		}
		if tr.overflow {
			res.Overflow = true
		}
		appendRunFailures(&res, prefix, forces, r)
		if tr.budgetHit {
			continue
		}
		if promoteAccessForces(tr, forces) {
			return res, true
		}
		for i := len(prefix); i < len(tr.procs); i++ {
			for _, g := range tr.enabled[i] {
				if g == tr.procs[i] {
					continue
				}
				child := make([]uint64, i+1)
				copy(child, tr.procs[:i])
				child[i] = g
				stack = append(stack, queued{child, tr.enabled[:i+1]})
			}
		}
	}
	res.Exhausted = !res.Overflow && !res.BudgetHit
	return res, false
}

func appendRunFailures(res *ExploreResult, prefix []uint64, forces map[accessForce]bool, r runResult) {
	if r.failed {
		res.Failures = append(res.Failures, newFailure(prefix, false, "", "", forces))
	}
	for i := 0; i < r.raceCount; i++ {
		res.Failures = append(res.Failures, newFailure(prefix, true, "", "", forces))
	}
	if r.panic != "" {
		res.Failures = append(res.Failures, newFailure(prefix, false, r.panic, "", forces))
	}
	if r.deadlock != "" {
		res.Failures = append(res.Failures, newFailure(prefix, false, "", r.deadlock, forces))
	}
}

func accessHasPriorConflictingInInterval(tr exploreTrace, conflicting []bool, k int) bool {
	for i := k - 1; i >= 0 && tr.accStep[i] == tr.accStep[k]; i-- {
		if tr.accSeq[i] == tr.accSeq[k] && conflicting[i] {
			return true
		}
	}
	return false
}

func accessNeedsReplayBoundary(tr exploreTrace, conflicting []bool, k int) bool {
	return tr.accStep[k] == 0 || accessHasPriorConflictingInInterval(tr, conflicting, k)
}

func accessSize(tr exploreTrace, k int) uintptr {
	if k < len(tr.accSize) && tr.accSize[k] != 0 {
		return tr.accSize[k]
	}
	return 1
}

func accessRangeEnd(addr, size uintptr) uintptr {
	if size == 0 {
		size = 1
	}
	end := addr + size
	if end < addr {
		return ^uintptr(0)
	}
	return end
}

func accessOverlap(addr, size, otherAddr, otherSize uintptr) bool {
	if addr == 0 || otherAddr == 0 {
		return false
	}
	end := accessRangeEnd(addr, size)
	otherEnd := accessRangeEnd(otherAddr, otherSize)
	return addr < otherEnd && otherAddr < end
}

func accessConflict(tr exploreTrace, i, j int) bool {
	return accessOverlap(tr.accAddr[i], accessSize(tr, i), tr.accAddr[j], accessSize(tr, j)) &&
		(tr.accWrite[i] || tr.accWrite[j]) && tr.accSeq[i] != tr.accSeq[j]
}

// promoteAccessForces closes the gap a live prior-conflict filter cannot see: a
// filtered access that was safe with respect to prior accesses may later prove to need
// a boundary inside its inline interval. When the completed trace demonstrates that,
// promote the exact hook call to a forced yield and restart the exploration pass.
func promoteAccessForces(tr exploreTrace, forces map[accessForce]bool) bool {
	if tr.overflow || tr.budgetHit {
		return false
	}
	clk, pidx := dporClocks(tr)
	conflicting := make([]bool, len(tr.accSeq))
	for j := 0; j < len(tr.accSeq); j++ {
		for i := j - 1; i >= 0; i-- {
			if accessConflict(tr, i, j) && dporConcurrent(clk, pidx, tr, i, j) {
				conflicting[i] = true
				conflicting[j] = true
			}
		}
	}
	grew := false
	for k := range tr.accSeq {
		if tr.accCount[k] == 0 || !conflicting[k] || !accessNeedsReplayBoundary(tr, conflicting, k) {
			continue
		}
		f := accessForce{seq: tr.accSeq[k], count: tr.accCount[k], pc: tr.accPC[k]}
		if !forces[f] {
			forces[f] = true
			grew = true
		}
	}
	return grew
}

// dporTrans is one access's sleep-set pruning identity: its run-local byte interval
// (addr==0 means no conflict identity) and whether it is a write.
type dporTrans struct {
	addr  uintptr
	size  uintptr
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
// transitions are *dependent* iff they record overlapping nonzero memory byte
// intervals (dstAccessYield/dstAccessYieldRange) or the same synchronization object's
// identity, with at least one write, by different goroutines (dstSyncAcquire records
// mutex/channel state decisions as write-conflicts so their order is a dependency;
// see runtime/dst_explore.go and exploration.md "Completeness boundary"). Each run
// re-executes from the start following the stack's
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
// per-run intervals plus the happens-before clocks (dporConcurrent), so mutex/channel-
// SERIALIZED conflicts are pruned while a free sync-decision order is explored both ways.
// addr==0 transitions (goroutine creation, WaitGroup wakeups, the isolated gcDrain
// goroutine) record no conflict identity and are independent of everything — they
// carry no outcome-determining order choice a recorded access/sync decision does not
// already capture. See exploration.md "Completeness boundary (addr=0 transitions)".
//
// Under race-enabled auto-instrumentation, filtered intervals and replay-promoted
// accesses can make the minimal source-set calculation too narrow around goroutine
// creation prefixes. Race-enabled DPOR therefore uses conservative all-enabled
// backtracking at each observed conflict anchor and disables sleep sets; the non-race
// sweep keeps the full source-DPOR + sleep-set algorithm and its optimality guard.
func dporExplore(seed uint64, sut func() bool, cfg exploreConfig) ExploreResult {
	forces := map[accessForce]bool{}
	var carriedRace []Failure
	totalSchedules := 0
	for {
		passCfg, ok := explorePassConfig(cfg, totalSchedules)
		if !ok {
			return ExploreResult{Schedules: totalSchedules, Failures: carriedRace, BudgetHit: true}
		}
		res, grew := dporExplorePass(seed, sut, forces, passCfg)
		totalSchedules += res.Schedules
		res.Schedules = totalSchedules
		if !grew {
			if len(carriedRace) != 0 {
				res.Failures = append(carriedRace, res.Failures...)
			}
			return res
		}
		if cfg.maxSchedules > 0 && totalSchedules >= cfg.maxSchedules {
			res.BudgetHit = true
			res.Exhausted = false
			if len(carriedRace) != 0 {
				res.Failures = append(carriedRace, res.Failures...)
			}
			return res
		}
		// Assertion failures from an obsolete force set are not replayable under the next
		// pass. Race failures are process-global TSan reports and may be deduped before the
		// converged pass, so keep the first observing replay token.
		for _, f := range res.Failures {
			if f.Race {
				carriedRace = append(carriedRace, f)
			}
		}
	}
}

func dporExplorePass(seed uint64, sut func() bool, forces map[accessForce]bool, cfg exploreConfig) (ExploreResult, bool) {
	var res ExploreResult
	var stack []*dporFrame
	raceEnabled := dstRaceEnabledFP()
	useSleep := !raceEnabled
	for {
		if cfg.maxSchedules > 0 && res.Schedules >= cfg.maxSchedules {
			res.BudgetHit = true
			break
		}
		prefix := make([]uint64, len(stack))
		for i, fr := range stack {
			prefix[i] = fr.proc
		}
		r := runOnceResultLocked(seed, prefix, forces, sut, cfg)
		tr := r.tr
		res.Schedules++
		if tr.budgetHit {
			res.BudgetHit = true
		}
		if tr.overflow {
			res.Overflow = true
		}
		appendRunFailures(&res, prefix, forces, r)
		if tr.budgetHit {
			break
		}
		if promoteAccessForces(tr, forces) {
			return res, true
		}
		// runOnceResult panics on a divergent (aborted) replay whose named seq was
		// not enabled. The complementary divergence — the SUT behaving differently
		// while the named seqs HAPPEN to stay enabled — slips that check, so verify
		// the replayed prefix against the frames' recorded decisions and enabled
		// sets too (exploration.md, hardening clause 4). A prefix truncated by a
		// recorded panic is checked over the part that ran.
		checkReplayPrefix(stack, tr)
		n := len(tr.procs)
		// Per-decision interval access-set: decision d's transition is the SET of
		// accesses performed in its interval — the access-log entries with
		// accStep == d+1 (performed by procs[d]). Under shared-address filtering an
		// interval may hold several single-owner accesses plus the conflicting one that
		// yielded; with yield-at-every-access each interval is a singleton.
		intervalSet := make([][]dporTrans, n)
		for k := range tr.accSeq {
			if d := tr.accStep[k] - 1; d >= 0 && d < n {
				intervalSet[d] = append(intervalSet[d], dporTrans{addr: tr.accAddr[k], size: accessSize(tr, k), write: tr.accWrite[k]})
			}
		}
		// Extend the stack with the decisions this run newly revealed. Each new frame
		// inherits its sleep set from its parent: the parent's asleep + already-explored
		// goroutines, FILTERED by independence with the parent's chosen interval set. A
		// sleeping goroutine has not executed since it was put to sleep, so the access
		// set stored with it stays valid down the frames it sleeps through.
		for d := len(stack); d < n; d++ {
			sleep := map[uint64][]dporTrans{}
			if useSleep && d > 0 {
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
				if accessConflict(tr, i, j) {
					if dporConcurrent(clk, pidx, tr, i, j) {
						if d := tr.accStep[i] - 1; d >= 0 && d < n {
							if raceEnabled || tr.syncEventOverflow {
								// All-enabled over-approximation. For a sync-event overflow
								// trace the weak-initial computation is under-ordered (a
								// spurious weak-initial can seed the WRONG backtrack and drop
								// a class), so precision degrades to soundness: backtrack
								// everything enabled. The overflow is still reported
								// (Exhausted=false).
								for _, g := range tr.enabled[d] {
									stack[d].backtrack[g] = true
								}
							} else {
								addSourceBacktrack(stack[d], tr.enabled[d], tr, traceClk, tracePidx, i, j)
							}
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
			if d >= len(intervalSet) {
				// The stack is deeper than the last replay's decision trace: only SUT
				// nondeterminism (or state corruption) can shrink a replayed prefix's
				// tree below frames already explored — surface the DST-L2-2 diagnostic,
				// not a bare index panic.
				panic("testing/simulation: internal error: DPOR stack deeper than the replayed trace — DST-L2-2 violation (nondeterministic SUT?)")
			}
			top.doneTrans[top.proc] = intervalSet[d]
			for _, g := range top.enabled {
				if top.backtrack[g] && !top.done[g] {
					if _, asleep := top.sleep[g]; useSleep && asleep {
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
	res.Exhausted = !res.Overflow && !res.BudgetHit
	return res, false
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

const (
	hbEventReady = iota + 1
	hbEventSyncRelease
	hbEventSyncAcquire
)

const (
	syncEventRelease = 1
	syncEventAcquire = 2
)

type hbEvent struct {
	kind      uint8
	order     int
	step, acc int
	from, to  uint64
	seq       uint64
	obj       syncObjectKey
}

type syncObjectKey struct {
	id  uintptr
	aux uintptr
}

func edgeOrderAt(tr exploreTrace, i int) int {
	if i < len(tr.edgeOrder) {
		return tr.edgeOrder[i]
	}
	return i
}

func syncOrderAt(tr exploreTrace, i int) int {
	if i < len(tr.syncOrd) {
		return tr.syncOrd[i]
	}
	return len(tr.edgeFrom) + i
}

func syncObjectAt(tr exploreTrace, i int) syncObjectKey {
	obj := syncObjectKey{id: tr.syncID[i]}
	if i < len(tr.syncAux) {
		obj.aux = tr.syncAux[i]
	}
	return obj
}

func orderedHBEvents(tr exploreTrace) []hbEvent {
	out := make([]hbEvent, 0, len(tr.edgeFrom)+len(tr.syncKind))
	e, s := 0, 0
	for e < len(tr.edgeFrom) || s < len(tr.syncKind) {
		if s >= len(tr.syncKind) || (e < len(tr.edgeFrom) && edgeOrderAt(tr, e) <= syncOrderAt(tr, s)) {
			out = append(out, hbEvent{kind: hbEventReady, order: edgeOrderAt(tr, e), step: tr.edgeStep[e], acc: tr.edgeAcc[e], from: tr.edgeFrom[e], to: tr.edgeTo[e]})
			e++
			continue
		}
		kind := uint8(hbEventSyncAcquire)
		if tr.syncKind[s] == syncEventRelease {
			kind = hbEventSyncRelease
		}
		out = append(out, hbEvent{kind: kind, order: syncOrderAt(tr, s), step: tr.syncStep[s], acc: tr.syncAcc[s], seq: tr.syncSeq[s], obj: syncObjectAt(tr, s)})
		s++
	}
	return out
}

func maxHBStep(tr exploreTrace) int {
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
	for _, s := range tr.syncStep {
		if s > maxStep {
			maxStep = s
		}
	}
	return maxStep
}

// dporClocks computes a SYNC happens-before vector clock per ACCESS-LOG entry from
// program order plus the recorded goready/create edges and sync release/acquire
// events, so dporConcurrent can test whether two accesses are causally ordered.
// clk[k] is access k's goroutine's clock snapshot right after access k's
// program-order tick; pidx maps a goroutine's stable index (dstSeq) to a vector
// position.
//
// Processing is in execution order, grouped by step (the access log is sorted by
// accStep, which is non-decreasing in execution order): within each step s, each
// HB event's access-log length places it before the first access whose log index is
// >= that length. This is load-bearing for filtered inline accesses after a wake: a
// wake edge must not make later same-step parent accesses happen-before the readied
// goroutine. A step with no accesses (a coarse-point-only interval) still applies its
// events; step 0 accesses are modeled too, so replay-promotion can detect conflicts
// before the first decision.
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
			cur[pi][pi]++
			clk[li] = append([]uint32(nil), cur[pi]...)
			li++
			applyEvents(s, li)
		}
		applyEvents(s, li)
	}
	return clk, pidx
}

// dporTraceClocks computes the TRACE happens-before over ACCESS-LOG entries — the
// Mazurkiewicz partial order = the transitive closure of the DEPENDENCY relation in
// trace order, plus program order and the recorded sync HB events. Unlike
// dporClocks (sync HB + program order only), it ALSO orders every pair of
// conflicting accesses (overlapping nonzero byte intervals, >=1 write) by their log order: a later
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
			// Conflict edges: a later access to an overlapping interval with >=1 write causally
			// depends on every earlier conflicting access — merge their clocks in, so e_i
			// trace-happens-before every later conflicting access.
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
