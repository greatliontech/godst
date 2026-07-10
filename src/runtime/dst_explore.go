// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Deterministic simulation testing (DST) Level-2 exploration substrate: the
// scheduled scheduling strategy (dstSchedScheduled). Under it the runtime is a
// pure schedule-FOLLOWER and trace-RECORDER — it follows an explicit prefix of
// bubble-goroutine choices at the dstSchedSelect seam and records, per decision,
// the enabled bubble-goroutine set and the chosen one. The exploration brain
// (offline, between Runs: exhaustive enumeration, then DPOR) consumes the trace
// and emits the next prefix. See docs/dst/exploration.md "Level 2 — access-granularity
// interleaving + DPOR", increments 2–4 (build order (b): the brain is proven on
// the manual access-yield hook before auto-instrumentation).
//
// CONSTRAINT (load-bearing): dstScheduledSelect runs on g0 while holding
// sched.lock (it is called from dstFindRunnable). It must NOT allocate. The trace
// buffers are therefore pre-sized by the brain via dstExploreInit (on a normal
// goroutine, off-lock) and written by index here; an over-budget run sets
// dstTraceOverflow rather than growing (the brain reports it — no silent cap).

package runtime

import (
	"internal/runtime/sys"
	"unsafe" // for go:linkname and the access-yield hook
)

// dstAccessYieldPoints counts guarded access yields taken since the last reset —
// the per-run yield magnitude, read via dstAccessYieldFP.
var dstAccessYieldPoints uint64

const (
	dstFilterMaxClockProcs  = 1024
	dstFilterMaxSyncObjects = 1024

	// dstSyncRendezvousAux keys unbuffered-channel rendezvous HB events,
	// distinct from close/closed-receive events (aux 0) and buffered slot
	// events (slot+1) — mirroring TSan, which keys the rendezvous on
	// chanbuf(c,0) and close on c.raceaddr().
	dstSyncRendezvousAux = ^uintptr(0)
)

// dstYieldAccess is the shared core of every Level-2 transition-boundary hook: it
// records the transition (addr, size, write) for DPOR's dependency relation and yields,
// subject to the safe-point guard and the dst-race shared-address filter. The guard
// (DST-L2-1) lives here, in ONE place, so it cannot drift across the hooks: yield
// only on a bubble (SUT) goroutine running on its own stack with no runtime lock
// held; otherwise return without yielding — skipping a yield is always sound (it
// only forgoes an interleaving). goyield requeues the current G and reschedules
// through dstFindRunnable, so the seam never runs a blocked G; soundness is
// inherited from Seq 5 unchanged.
func dstYieldAccess(addr, size uintptr, write bool, filter bool, pc uintptr) {
	gp := getg()
	// Access-granularity yielding is a Level-2 (scheduled-strategy) mechanism. Under
	// the Seq-5 Random/PCT strategies the interleaving atoms are the coarse cooperative
	// points, and an extra yield at every access would perturb those strategies — which
	// matters now that the dst-race compiler mode auto-inserts this at EVERY memory
	// access under -race (so any -tags dst -race run, not just Explore, reaches here).
	// Gating to dstSchedScheduled confines access yields to Explore; non-scheduled
	// strategies are byte-for-byte unaffected.
	if !dstActive() || dstSchedKind != dstSchedScheduled || gp.bubble == nil {
		return
	}
	if gp != gp.m.curg || gp.m.locks != 0 {
		// Safe-point guard failed: yielding here is unsound, but the access is still
		// a real transition of a bubble goroutine and its conflicts prune classes if
		// dropped — record it without yielding (D1's "record the access but do not
		// yield" is normative; exploration.md, hardening clause 3). dstCommitAccess
		// is pool-allocating and lock-free by design (it already runs under
		// sched.lock from the scheduled-select commit), so it is safe in this
		// restricted context. A replay-promoted force landing here is intentionally
		// not honored: yielding is unsound in this context regardless, and
		// promoteAccessForces cannot loop on it (it grows only on a NEW force).
		seq := dstEnsureSeq(gp)
		gp.dstAccCount++
		dstCommitAccess(gp, seq, addr, size, write, dstAccessPCKey(pc), gp.dstAccCount, filter && raceenabled, dstScheduleStep)
		return
	}
	seq := dstEnsureSeq(gp)
	gp.dstAccCount++
	pc = dstAccessPCKey(pc)
	auto := filter && raceenabled
	forced := dstAccessForced(seq, gp.dstAccCount, pc)
	if auto && !forced && !dstAccessShouldYield(gp, seq, addr, size, write) {
		dstCommitAccess(gp, seq, addr, size, write, pc, gp.dstAccCount, auto, dstScheduleStep)
		return
	}
	gp.dstAccAddr = addr
	gp.dstAccSize = size
	gp.dstAccWrite = write
	gp.dstAccPC = pc
	gp.dstAccPend = true
	gp.dstAccAuto = auto
	dstAccessYieldPoints++
	goyield()
}

// dstAccessYield is the access-granularity cooperative yield + access record: the
// Level-2 transition boundary (D1) at a MEMORY access — manually for the
// build-order-(b) validation phase, by the dst-race compiler mode later. In non-race
// manual validation builds it remains an explicit transition boundary; in dst-race
// builds the shared-address filter may record the access without yielding if it is
// independent.
//
//go:linkname dstAccessYield
func dstAccessYield(addr unsafe.Pointer, write bool) {
	dstYieldAccess(uintptr(addr), 1, write, true, sys.GetCallerPC())
}

// dstAccessYieldRange is the range/composite form of dstAccessYield. The compiler
// emits it before race{read,write}range hooks under -tags dst -race so DST uses the
// same byte interval the unchanged TSan oracle observes.
//
//go:linkname dstAccessYieldRange
func dstAccessYieldRange(addr unsafe.Pointer, size uintptr, write bool) {
	dstYieldAccess(uintptr(addr), size, write, true, sys.GetCallerPC())
}

// dstSyncAcquire is the Level-2 transition boundary for a synchronization object
// decision (mutex/RWMutex acquire, try, release; channel send/recv/select/close): it
// announces the sync object's identity as a write-conflict and yields BEFORE the
// state decision/transition. Two decisions on the same object by different goroutines
// are then a co-enabled, concurrent, conflicting pair whose BOTH orderings DPOR
// explores — without which a program whose outcome depends on sync-decision order
// silently loses Mazurkiewicz classes (DST-L2-3). Modeled as a write-conflict because
// same-object sync decisions do not commute. Same guard/soundness as dstAccessYield:
// a pre-decision yield is sound because the real scheduler can switch before any
// goroutine changes the sync object's state.
//
//go:linkname dstSyncAcquire
func dstSyncAcquire(id unsafe.Pointer) {
	dstYieldAccess(uintptr(id), 1, true, false, sys.GetCallerPC())
}

// Atomic-operation kinds for dstAtomicYield. MIRRORED by the compiler's
// dst-race emission (cmd/compile/internal/ssagen, dstAtomicCallInfo) — the
// compiler cannot import runtime, so the values are a shared convention.
const (
	dstAtomicLoad  = 0 // Load*: read-conflict; HB acquire (it observes)
	dstAtomicStore = 1 // Store*: write-conflict; HB release (it publishes, observes nothing)
	dstAtomicRMW   = 2 // Swap*/Add*/And*/Or*: write-conflict; HB acquire+release (observes and publishes)
	dstAtomicCAS   = 3 // CompareAndSwap*: write-conflict; HB acquire ONLY (see below)
)

// dstSyncAtomicAux keys atomic happens-before events apart from the channel
// auxes on the same id space (close=0, buffered slots=slot+1, rendezvous=^0):
// an atomic variable's address and a channel pointer are distinct objects, but
// a distinct aux keeps the keying collision-free by construction.
const dstSyncAtomicAux = ^uintptr(1)

// dstAtomicYield is the Level-2 transition boundary for a sync/atomic
// operation: the dst-race compiler mode emits it immediately before each
// static call to a sync/atomic function in instrumented code (the atomic
// implementations themselves are NOSPLIT race assembly and cannot host a
// yield — the same constraint that put dstAccessYield in the compiler, D1).
// Which goroutine's atomic op on an address commits first is an
// outcome-determining decision (a CAS winner) decided at TSan's atomic entry
// points, which record no DST transition — without this hook DPOR explores
// one outcome class and still reports Exhausted=true (the prog#257 lesson
// replayed for atomics; the Completeness boundary's former named exclusion).
//
// Announced like dstSyncAcquire — always-yield, never the shared-address
// filter: atomic ops create happens-before BETWEEN THEMSELVES (the events
// below), so the filter would learn the executed order and suppress exactly
// the decisions whose reversals matter. Loads announce as read-conflicts
// (load pairs commute), everything else as write-conflicts on the address's
// real byte width, so atomics also pair with PLAIN accesses of the same
// memory in the dependency relation.
//
// After the yield returns — the goroutine is resumed and the atomic commits
// next, in the NOSPLIT assembly, with no further yield — the op's
// happens-before contribution is recorded for the offline DPOR relation and
// the live filter clocks. The effective release semantics are cumulative
// (release clocks MERGE on the object — see the exploration.md effective-semantics
// paragraph; where the merge claims an edge the literal observed-by reading
// would not, the announce-reorderability masking below applies and the
// sweep's DPOR==Exhaustive equivalence is the enforced contract). On top of
// that floor, CAS records acquire ONLY: it
// always observes the old value (acquire is real, success or failure) but
// may write nothing, and the hook runs before the outcome is known —
// claiming its release would publish a failed CAS's clock, an edge the
// memory model does not grant. In the CURRENT explorer that over-claim is
// masked rather than fatal: same-address atomic announces always form a
// conflicting pair, so the release-op/acquire-op reorder is always seeded,
// and the per-trace re-analysis drops the claimed edge in the reordered
// trace and recovers any class it suppressed (verified: the over-claim
// mutant is outcome-equivalent across the sweep and a crafted two-variable
// probe). Acquire-only is kept as the faithful model so pruning soundness
// rests on the memory model, not on that masking. The missed release edge
// of a SUCCESSFUL CAS only forgoes pruning (over-exploration), never a
// class.
//
//go:linkname dstAtomicYield
func dstAtomicYield(addr unsafe.Pointer, size uintptr, kind uintptr) {
	dstYieldAccess(uintptr(addr), size, kind != dstAtomicLoad, false, sys.GetCallerPC())
	switch kind {
	case dstAtomicLoad, dstAtomicCAS:
		dstRecordSyncAcquireID(uintptr(addr), dstSyncAtomicAux)
	case dstAtomicStore:
		dstRecordSyncReleaseID(uintptr(addr), dstSyncAtomicAux)
	case dstAtomicRMW:
		// Acquire first, then release: the published clock then includes the
		// just-observed history, so RMW chains (atomic counters) stay
		// transitively ordered for pruning.
		dstRecordSyncAcquireID(uintptr(addr), dstSyncAtomicAux)
		dstRecordSyncReleaseID(uintptr(addr), dstSyncAtomicAux)
	}
}

// dstSyncObserve is the read-side sibling of dstSyncAcquire: a len(ch)
// observation announces the channel's identity as a READ-conflict and yields
// before reading the count. A len observed concurrently with a send/recv/
// close (write-conflicts on the same identity) is an outcome-determining
// order the explorer must branch on; two len observations commute, so a
// read-conflict (not write) avoids exploring their orders. cap(ch) is NOT
// hooked: a channel's capacity is immutable after make, so a cap read
// carries no ordering decision at all.
//
//go:linkname dstSyncObserve
func dstSyncObserve(id unsafe.Pointer) {
	dstYieldAccess(uintptr(id), 1, false, false, sys.GetCallerPC())
}

// dstYieldPoint is a cooperative yield with no specific memory access recorded — a
// pure scheduling point (used by soundness probes). See dstAccessYield.
//
//go:linkname dstYieldPoint
func dstYieldPoint() {
	dstYieldAccess(0, 0, false, false, sys.GetCallerPC())
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
	// dstScheduleAbortStep/dstExplorePanicStep (-1 = unset) locate the abort and the
	// first recorded SUT panic in decision steps, so the harness can attribute an
	// abort to panic truncation ONLY when the abort lies at/after the panic point —
	// a divergence BEFORE the panic is a genuine DST-L2-2 violation a coinciding
	// panic must not mask (exploration.md, hardening clause 4).
	dstScheduleAbortStep int32
	dstExplorePanicStep  int32
)

var (
	dstExplorePanicValue any
	dstExplorePanicSet   bool
	dstExploreDeadlock   string
)

// dstPostGoYield enables the scheduled-strategy boundary immediately after a go
// statement. It is on for public Explore. The manual DPOR-brain sweep disables it
// through dstSetPostGoYield so its exhaustive baseline remains the explicitly
// annotated transition set it is designed to validate.
var dstPostGoYield = true

//go:linkname dstSetPostGoYield
func dstSetPostGoYield(enabled bool) bool {
	old := dstPostGoYield
	dstPostGoYield = enabled
	return old
}

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
// during the transition with dstScheduleStep == dstEdgeStep[e]. dstEdgeAcc[e] is the
// access-log length at the moment the edge was observed, so offline HB can place an
// edge before later inline filtered accesses in the same schedule step. The DPOR engine
// builds vector clocks offline from these + program order, so two conflicting accesses
// are dependent only if they are CONCURRENT (neither happens-before the other) — pruning
// mutex/channel-serialized pairs the conservative relation would over-explore.
// Pre-sized (never grown under the lock); over-budget sets dstEdgeOverflow (reported,
// never a silent cap).
var (
	dstEdgeFrom     []uint64
	dstEdgeTo       []uint64
	dstEdgeStep     []int32
	dstEdgeAcc      []int32
	dstEdgeOrder    []int32
	dstEdgeN        int
	dstEdgeOverflow bool
	dstHBEventN     int32
	// dstSyncEventOverflow reports a dropped release/acquire sync event. NOT merely a
	// pruning loss: the offline trace-HB the sync-event log feeds computes the
	// WEAK-INITIAL sets, and an under-ordered trace-HB produces spurious weak-initials
	// that can early-return addSourceBacktrack before the genuine reversal is seeded —
	// a dropped Mazurkiewicz class while Exhausted reads true (exploration.md,
	// hardening clause 1). Folded into the trace overflow exactly as dstEdgeOverflow.
	dstSyncEventOverflow bool
)

const (
	dstSyncEventRelease = 1
	dstSyncEventAcquire = 2
)

// Sync happens-before event buffers. These are separate from dstSyncAcquire's
// decision-conflict log: a mutex Lock and Unlock both conflict for DPOR, but only an
// actual Unlock->later Lock (or buffered send->receive) is a memory-model HB edge.
// The brain replays these as object clocks offline, preserving the release-time
// snapshot instead of over-ordering with the releasing goroutine's later accesses.
// Overflow only loses pruning, so it is conservative and not a coverage overflow.
var (
	dstSyncEventKind []uint8
	dstSyncEventID   []uintptr
	dstSyncEventAux  []uintptr
	dstSyncEventSeq  []uint64
	dstSyncEventStep []int32
	dstSyncEventAcc  []int32
	dstSyncEventOrd  []int32
	dstSyncEventN    int
)

// Access log buffers (shared-address filtering, increment 6). Unlike the per-decision
// trace (which records the access of the goroutine CHOSEN at each scheduling
// decision), the access log records EVERY instrumented access in execution order —
// (accessing goroutine dstSeq, addr, size, hook PC key, hook ordinal, isWrite, the
// dstScheduleStep it occurred under) — decoupled from whether the access yielded.
// This decoupling is what lets the runtime FILTER: a single-owner access can "record
// but not yield" (exploration.md D1) while the brain still sees it for the dependency
// relation. The PC+ordinal let the brain promote a filtered access to a forced replay
// yield if a later conflict proves the inline interval needed a split. Pre-sized
// (never grown under the lock); over-budget sets dstAccLogOverflow (reported, never a
// silent cap).
var (
	dstAccLogSeq      []uint64
	dstAccLogAddr     []uintptr
	dstAccLogSize     []uintptr
	dstAccLogPC       []uintptr
	dstAccLogCount    []uint64
	dstAccLogWrite    []bool
	dstAccLogStep     []int32
	dstAccLogN        int
	dstAccLogOverflow bool
)

// Shared-address filter state (increment 6). The runtime needs a live, conservative
// HB view to decide whether an auto-instrumented memory access is a transition worth
// yielding at. It records every access either way; filtering only chooses whether a
// dst-race auto-instrumented access becomes a scheduling decision. Manual non-race
// validation hooks remain explicit boundaries so the brain-validation corpus stays
// hand-controlled. Auto-instrumented accesses to the current goroutine's stack are
// logged as addr=0 at commit, because stack storage can be reused by another goroutine
// after this one exits; raw stack-address equality is not a shared-memory identity.
//
// Clocks are bounded and preallocated. If the run exceeds the precise clock/table
// budget, filtering falls back to yielding every later access; that loses pruning but
// cannot lose a class.
var (
	dstClockProcs int
	dstClock      []uint32 // flat [dstClockProcs][dstClockProcs]

	dstAccessTab        []int32 // hash bucket -> 1-based entry index
	dstAccessEntryAddr  []uintptr
	dstAccessEntrySize  []uintptr
	dstAccessEntryProc  []int32
	dstAccessNext       []int32
	dstAccessReadEpoch  []uint32
	dstAccessWriteEpoch []uint32
	dstAccessEntryN     int
	dstSyncClockID      []uintptr
	dstSyncClockAux     []uintptr
	dstSyncClock        []uint32 // flat [sync-object][dstClockProcs]
	dstSyncClockN       int

	dstFilterConservative bool
	dstForceSeq           []uint64
	dstForceCount         []uint64
	dstForcePC            []uintptr

	// Force-lookup table: dstAccessForced runs on every auto-
	// instrumented access, and a linear scan over the installed triples is
	// O(F) per access as promotion grows. dstSetAccessForce (off-lock,
	// between runs) builds this open-addressed table over the triples;
	// lookups probe O(1). 1-based indices into dstForceSeq/Count/PC; 0 empty.
	dstForceTab  []int32
	dstForceMask uintptr

	// Page index over the access entries: dstAccessShouldYield needs
	// every recorded entry overlapping the queried range, and scanning all
	// entries is O(A) per access — O(A²) for heap-heavy SUTs. Entries are
	// indexed by the 256-byte pages their range covers: per-page hash chains
	// of (entry, page) nodes from a preallocated pool. An entry covering more
	// than dstAccPageMaxSpan pages goes to the small always-scanned large-
	// entry list; a QUERY spanning more than dstAccPageMaxQuery pages falls
	// back to the full scan (correct, rare). Pool or list exhaustion sets
	// dstFilterConservative — the existing lose-pruning-never-a-class
	// fallback.
	dstAccPageTab      []int32 // page-hash bucket -> 1-based node index
	dstAccPageNodeEnt  []int32
	dstAccPageNodeNext []int32
	dstAccPageNodePage []uintptr
	dstAccPageNodeN    int
	// dstAccPageChargeN is the deterministic budget counter: the sum of worst-case
	// per-entry charges (dstAccPageCharge), against which capacity is decided —
	// dstAccPageNodeN is the physical consumption, always <= the charge, never a
	// capacity input (it is address-dependent).
	dstAccPageChargeN int
	dstAccLargeEnt    [dstAccLargeMax]int32
	dstAccLargeN      int
)

const (
	dstAccPageShift    = 8  // 256-byte pages
	dstAccPageMaxSpan  = 8  // pages an entry is indexed under before the large list
	dstAccPageMaxQuery = 64 // pages a query walks before falling back to the full scan
	dstAccLargeMax     = 64
)

func dstEnsureSeq(gp *g) uint64 {
	if gp.dstSeq == 0 {
		dstSeqCtr++
		gp.dstSeq = dstSeqCtr
	}
	return gp.dstSeq
}

// dstClearSchedState clears per-bubble scheduled-strategy identity and pending
// access state from gp. Goroutine structs are reused across bubbles, and the
// synctest root goroutine is reused directly by repeated Explore/Replay calls, so
// this state must not survive past one scheduled bubble.
func dstClearSchedState(gp *g) {
	if gp == nil {
		return
	}
	gp.dstSeq = 0
	gp.dstAccAddr = 0
	gp.dstAccSize = 0
	gp.dstAccWrite = false
	gp.dstAccPC = 0
	gp.dstAccCount = 0
	gp.dstAccPend = false
	gp.dstAccAuto = false
}

func dstClockIdx(seq uint64) int {
	if seq == 0 || seq > uint64(dstClockProcs) {
		return -1
	}
	return int(seq - 1)
}

func dstClockAt(proc, component int) uint32 {
	return dstClock[proc*dstClockProcs+component]
}

func dstClockSet(proc, component int, v uint32) {
	dstClock[proc*dstClockProcs+component] = v
}

func dstClockTick(proc int) uint32 {
	e := dstClockAt(proc, proc) + 1
	dstClockSet(proc, proc, e)
	return e
}

func dstClockMerge(dst, src int) {
	baseDst := dst * dstClockProcs
	baseSrc := src * dstClockProcs
	for i := 0; i < dstClockProcs; i++ {
		if dstClock[baseSrc+i] > dstClock[baseDst+i] {
			dstClock[baseDst+i] = dstClock[baseSrc+i]
		}
	}
}

func dstSyncClockEntry(id, aux uintptr, create bool) int {
	for i := 0; i < dstSyncClockN; i++ {
		if dstSyncClockID[i] == id && dstSyncClockAux[i] == aux {
			return i
		}
	}
	if !create {
		return -1
	}
	if dstSyncClockN >= len(dstSyncClockID) {
		dstFilterConservative = true
		return -1
	}
	entry := dstSyncClockN
	dstSyncClockN++
	dstSyncClockID[entry] = id
	dstSyncClockAux[entry] = aux
	base := entry * dstClockProcs
	for i := 0; i < dstClockProcs; i++ {
		dstSyncClock[base+i] = 0
	}
	return entry
}

func dstSyncClockRelease(entry, proc int) {
	baseObj := entry * dstClockProcs
	baseProc := proc * dstClockProcs
	for i := 0; i < dstClockProcs; i++ {
		if dstClock[baseProc+i] > dstSyncClock[baseObj+i] {
			dstSyncClock[baseObj+i] = dstClock[baseProc+i]
		}
	}
}

func dstSyncClockAcquire(proc, entry int) {
	baseProc := proc * dstClockProcs
	baseObj := entry * dstClockProcs
	for i := 0; i < dstClockProcs; i++ {
		if dstSyncClock[baseObj+i] > dstClock[baseProc+i] {
			dstClock[baseProc+i] = dstSyncClock[baseObj+i]
		}
	}
}

func dstApplyLiveSyncEvent(kind uint8, id, aux uintptr, seq uint64) {
	proc := dstClockIdx(seq)
	if proc < 0 || dstClockProcs == 0 {
		dstFilterConservative = true
		return
	}
	entry := dstSyncClockEntry(id, aux, true)
	if entry < 0 {
		return
	}
	switch kind {
	case dstSyncEventRelease:
		dstSyncClockRelease(entry, proc)
	case dstSyncEventAcquire:
		dstSyncClockAcquire(proc, entry)
	}
}

func dstAccessBucket(addr, size uintptr) int {
	if len(dstAccessTab) == 0 {
		dstFilterConservative = true
		return -1
	}
	h := (addr >> 3) ^ (addr >> 17) ^ (size << 7) ^ (size >> 11)
	return int(h % uintptr(len(dstAccessTab)))
}

func dstFindAccessEntry(addr, size uintptr, proc int, create bool) int {
	b := dstAccessBucket(addr, size)
	if b < 0 {
		return -1
	}
	for e := int(dstAccessTab[b]) - 1; e >= 0; e = int(dstAccessNext[e]) - 1 {
		if dstAccessEntryAddr[e] == addr && dstAccessEntrySize[e] == size && int(dstAccessEntryProc[e]) == proc {
			return e
		}
	}
	if !create {
		return -1
	}
	if dstAccessEntryN >= len(dstAccessEntryAddr) {
		dstFilterConservative = true
		return -1
	}
	e := dstAccessEntryN
	dstAccessEntryN++
	dstAccessEntryAddr[e] = addr
	dstAccessEntrySize[e] = size
	dstAccessEntryProc[e] = int32(proc)
	dstAccessReadEpoch[e] = 0
	dstAccessWriteEpoch[e] = 0
	dstAccessNext[e] = dstAccessTab[b]
	dstAccessTab[b] = int32(e + 1)
	dstAccPageInsert(e, addr, size)
	return e
}

// dstAccPageBucket hashes a page number into dstAccPageTab.
func dstAccPageBucket(page uintptr) int {
	h := page ^ (page >> 13) ^ (page << 5)
	return int(h % uintptr(len(dstAccPageTab)))
}

// dstAccPageCharge is the deterministic worst-case page-node cost of a size-S
// range: the EXACT maximum number of dstAccPageShift-pages a range of S bytes can
// cover over all start alignments — ceil((S-1)/page) + 1. The capacity accounting
// charges THIS, never the actual page count, because the actual count is a function
// of the entry's run-local *address alignment* — and an alignment-dependent pool
// exhaustion flips dstFilterConservative at a different decision in a fresh process
// (explorer-side allocations and per-launch arena placement move addresses),
// misaligning a replayed schedule prefix: the DST-L2-2 abort, or silent divergence
// (exploration.md, hardening clause 2 — filter capacity is a function of counts the
// schedule determines, never of addresses). Exact, not merely an upper bound, so the
// filter degrades to conservative no sooner than a real alignment forces it to.
//go:linkname dstAccPageCharge
func dstAccPageCharge(size uintptr) uintptr {
	if size == 0 {
		size = 1
	}
	// Saturate only at the genuine uintptr wrap boundary of the ceil arithmetic
	// below (size-1 + page-1): a size within page-1 of the address-space top would
	// wrap the addition to a tiny value → small-class → the loop then walks a
	// clamped ~2^56-page range and hits the "unreachable" throw. Such a size is
	// astronomically unreachable in-spec; saturating it to a large-class charge is
	// exact for every realistic size (the threshold is ~2^64, far above any real
	// range, so e.g. 65537 still computes its true 257).
	if size-1 > ^uintptr(0)-(1<<dstAccPageShift-1) {
		return dstAccPageMaxSpan + 2 // > dstAccPageMaxSpan: large class
	}
	// ceil((size-1)/page) + 1, page = 1<<dstAccPageShift, all powers of two.
	return (size-1+(1<<dstAccPageShift)-1)>>dstAccPageShift + 1
}

// dstAccPageInsert indexes a freshly created access entry under the pages its
// range covers (or the large-entry list). Called on the dstFindAccessEntry
// create path — under sched.lock from dstScheduledSelect's commit, lock-free
// from dstYieldAccess's inline filtered commit — and pool-allocating only,
// never the heap, which is what both contexts require. The pool is sized for
// two pages per entry on average (2x the entry budget): a workload averaging
// wider ranges exhausts the deterministic budget sooner and flips
// dstFilterConservative — trading pruning, never a class, exactly as the
// filter's overflow contract authorizes; size the dstExploreInit budgets up if
// such a workload needs the pruning. Every capacity decision here — the
// large-entry classification and the budget exhaustion — is address-independent
// (size-classified, worst-case-charged; see dstAccPageCharge), decided BEFORE
// anything is inserted so no entry is ever partially indexed.
func dstAccPageInsert(e int, addr, size uintptr) {
	if len(dstAccPageTab) == 0 {
		dstFilterConservative = true
		return
	}
	charge := dstAccPageCharge(size)
	if charge > dstAccPageMaxSpan {
		// Large-class by SIZE: a span-based test (end-start) would classify the
		// same size differently depending on whether the range happens to straddle
		// one extra boundary at its address.
		if dstAccLargeN >= len(dstAccLargeEnt) {
			dstFilterConservative = true
			return
		}
		dstAccLargeEnt[dstAccLargeN] = int32(e)
		dstAccLargeN++
		return
	}
	if uintptr(dstAccPageChargeN)+charge > uintptr(len(dstAccPageNodeEnt)) {
		dstFilterConservative = true
		return
	}
	dstAccPageChargeN += int(charge)
	start := addr >> dstAccPageShift
	end := (dstAccessRangeEnd(addr, size) - 1) >> dstAccPageShift
	for p := start; p <= end; p++ {
		if dstAccPageNodeN >= len(dstAccPageNodeEnt) {
			// Unreachable: actual consumption <= the budget charged above, and the
			// budget is capped at the pool size. Reaching here is accounting
			// corruption, not a capacity condition — fail loud, never a silent
			// (and address-dependent) conservative flip.
			throw("dst: page-node pool exhausted under budget")
		}
		b := dstAccPageBucket(p)
		n := dstAccPageNodeN
		dstAccPageNodeN++
		dstAccPageNodeEnt[n] = int32(e)
		dstAccPageNodePage[n] = p
		dstAccPageNodeNext[n] = dstAccPageTab[b]
		dstAccPageTab[b] = int32(n + 1)
	}
}

func dstAccessHBBefore(curProc, priorProc int, priorEpoch uint32) bool {
	return priorEpoch == 0 || dstClockAt(curProc, priorProc) >= priorEpoch
}

func dstAccessRangeEnd(addr, size uintptr) uintptr {
	if size == 0 {
		size = 1
	}
	end := addr + size
	if end < addr {
		return ^uintptr(0)
	}
	return end
}

func dstAccessOverlap(addr, size, otherAddr, otherSize uintptr) bool {
	if addr == 0 || otherAddr == 0 {
		return false
	}
	end := dstAccessRangeEnd(addr, size)
	otherEnd := dstAccessRangeEnd(otherAddr, otherSize)
	return addr < otherEnd && otherAddr < end
}

func dstAccessMaybeShared(gp *g, addr, size uintptr) bool {
	if addr == 0 {
		return false
	}
	end := dstAccessRangeEnd(addr, size)
	return addr < gp.stack.lo || end > gp.stack.hi
}

func dstAccessPCKey(pc uintptr) uintptr {
	f := findfunc(pc)
	if !f.valid() {
		return pc
	}
	h := uint64(1469598103934665603)
	name := funcname(f)
	for i := 0; i < len(name); i++ {
		h ^= uint64(name[i])
		h *= 1099511628211
	}
	h ^= uint64(pc - f.entry())
	h *= 1099511628211
	return uintptr(h)
}

func dstForceHash(seq, count uint64, pc uintptr) uintptr {
	h := seq*0x9e3779b97f4a7c15 ^ count*0xbf58476d1ce4e5b9 ^ uint64(pc)*0x94d049bb133111eb
	h ^= h >> 29
	return uintptr(h)
}

func dstAccessForced(seq, count uint64, pc uintptr) bool {
	if dstForceTab == nil {
		return false
	}
	h := dstForceHash(seq, count, pc) & dstForceMask
	for {
		i := int(dstForceTab[h]) - 1
		if i < 0 {
			return false
		}
		if dstForceSeq[i] == seq && dstForceCount[i] == count && dstForcePC[i] == pc {
			return true
		}
		h = (h + 1) & dstForceMask
	}
}

func dstAccessShouldYield(gp *g, seq uint64, addr, size uintptr, write bool) bool {
	if addr == 0 || dstFilterConservative || dstFilterForceConservative {
		return true
	}
	if !dstAccessMaybeShared(gp, addr, size) {
		return false
	}
	proc := dstClockIdx(seq)
	if proc < 0 {
		dstFilterConservative = true
		return true
	}
	if len(dstAccessTab) == 0 {
		dstFilterConservative = true
		return true
	}
	start := addr >> dstAccPageShift
	end := (dstAccessRangeEnd(addr, size) - 1) >> dstAccPageShift
	if len(dstAccPageTab) == 0 || end-start >= dstAccPageMaxQuery {
		// Index unavailable, or a range so large that walking its pages would
		// cost more than scanning the entries: full scan, same semantics.
		for e := 0; e < dstAccessEntryN; e++ {
			if dstAccessEntryConflicts(e, proc, addr, size, write) {
				return true
			}
		}
		return false
	}
	for p := start; p <= end; p++ {
		b := dstAccPageBucket(p)
		for n := int(dstAccPageTab[b]) - 1; n >= 0; n = int(dstAccPageNodeNext[n]) - 1 {
			if dstAccPageNodePage[n] != p {
				// Page-hash collision: a node for another page sharing this
				// bucket. Skipping is sound — an entry overlapping the query
				// shares a page with it and is found under that page.
				continue
			}
			if dstAccessEntryConflicts(int(dstAccPageNodeEnt[n]), proc, addr, size, write) {
				return true
			}
		}
	}
	for i := 0; i < dstAccLargeN; i++ {
		if dstAccessEntryConflicts(int(dstAccLargeEnt[i]), proc, addr, size, write) {
			return true
		}
	}
	return false
}

// dstAccessEntryConflicts reports whether prior access entry e conflicts with
// the (proc, addr, size, write) access — overlapping range, different
// goroutine, and not ordered by the live HB clocks. The per-entry test the
// page index and the full-scan fallback share.
func dstAccessEntryConflicts(e, proc int, addr, size uintptr, write bool) bool {
	if !dstAccessOverlap(addr, size, dstAccessEntryAddr[e], dstAccessEntrySize[e]) {
		return false
	}
	priorProc := int(dstAccessEntryProc[e])
	if priorProc == proc {
		return false
	}
	if dstAccessWriteEpoch[e] != 0 && !dstAccessHBBefore(proc, priorProc, dstAccessWriteEpoch[e]) {
		return true
	}
	if write && dstAccessReadEpoch[e] != 0 && !dstAccessHBBefore(proc, priorProc, dstAccessReadEpoch[e]) {
		return true
	}
	return false
}

func dstCommitAccess(gp *g, seq uint64, addr, size uintptr, write bool, pc uintptr, count uint64, auto bool, step int) {
	if auto && !dstAccessMaybeShared(gp, addr, size) {
		addr = 0
		size = 0
		write = false
	}
	proc := dstClockIdx(seq)
	if proc >= 0 {
		epoch := dstClockTick(proc)
		if addr != 0 {
			entry := dstFindAccessEntry(addr, size, proc, true)
			if entry >= 0 {
				if write {
					dstAccessWriteEpoch[entry] = epoch
				} else {
					dstAccessReadEpoch[entry] = epoch
				}
			}
		}
	} else {
		dstFilterConservative = true
	}
	dstRecordAccess(seq, addr, size, write, pc, count, step)
}

// dstRecordAccess appends one access to the log in COMMIT order: (seq, addr, size, pc,
// count, write, step), where step is the dstScheduleStep the access commits under (so
// an access with step s was committed by the goroutine chosen at decision s-1, and its
// reversal anchors at decision s-1). Logging in COMMIT order, not announce order, is
// load-bearing: an access is *announced* at dstAccessYield (when the goroutine reaches
// it and yields) but *commits* only when the goroutine is next resumed — those orders
// differ, and the dependency relation needs the order the memory operations actually
// take effect. So a YIELDING access is logged at the scheduling decision that resumes
// it (dstScheduledSelect, step = dstScheduleStep+1); a NON-yielding (shared-address-
// filtered) access commits inline with no reschedule, so announce order == commit
// order and it is logged at dstAccessYield (step = dstScheduleStep). Allocation-free;
// the dstScheduledSelect caller runs under sched.lock, so this must not allocate.
func dstRecordAccess(seq uint64, addr, size uintptr, write bool, pc uintptr, count uint64, step int) {
	if dstAccLogN < len(dstAccLogSeq) {
		dstAccLogSeq[dstAccLogN] = seq
		dstAccLogAddr[dstAccLogN] = addr
		dstAccLogSize[dstAccLogN] = size
		dstAccLogPC[dstAccLogN] = pc
		dstAccLogCount[dstAccLogN] = count
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
	from := dstEnsureSeq(readier)
	to := dstEnsureSeq(readied)
	if fromIdx, toIdx := dstClockIdx(from), dstClockIdx(to); fromIdx >= 0 && toIdx >= 0 {
		dstClockMerge(toIdx, fromIdx)
	} else {
		dstFilterConservative = true
	}
	if dstEdgeN < len(dstEdgeFrom) {
		dstEdgeFrom[dstEdgeN] = from
		dstEdgeTo[dstEdgeN] = to
		dstEdgeStep[dstEdgeN] = int32(dstScheduleStep)
		dstEdgeAcc[dstEdgeN] = int32(dstAccLogN)
		dstEdgeOrder[dstEdgeN] = dstHBEventN
		dstHBEventN++
		dstEdgeN++
	} else {
		dstEdgeOverflow = true
	}
}

func dstRecordSyncEvent(kind uint8, id unsafe.Pointer) {
	if id == nil {
		return
	}
	dstRecordSyncEventID(kind, uintptr(id), 0)
}

func dstRecordSyncEventID(kind uint8, id, aux uintptr) {
	if !dstActive() || dstSchedKind != dstSchedScheduled || id == 0 {
		return
	}
	gp := getg()
	if gp == nil || gp != gp.m.curg {
		return
	}
	dstRecordSyncEventForGID(kind, id, aux, gp)
}

func dstRecordSyncEventForGID(kind uint8, id, aux uintptr, gp *g) {
	if !dstActive() || dstSchedKind != dstSchedScheduled {
		return
	}
	if gp == nil || gp.bubble == nil || id == 0 {
		return
	}
	// The HB shadow honors the EXECUTING goroutine's raceignore, exactly as
	// every race.go acquire/release variant does (the g-credited forms
	// raceacquireg/racereleaseg also check getg().raceignore, not the passed
	// g's): an event whose TSan twin is ignored must not enter the shadow, or
	// the offline clocks disagree with the -race oracle. This single choke
	// point covers every recorder — the sync-package bridges, chan.go/
	// select.go (including their g-credited sites), and dstAtomicYield. Announces
	// and ready/create edges do not flow through here and stay unaffected.
	if raceenabled && getg().raceignore != 0 {
		return
	}
	seq := dstEnsureSeq(gp)
	dstApplyLiveSyncEvent(kind, id, aux, seq)
	// Buffer overflow below is REPORTED (dstSyncEventOverflow), never silent. The
	// "a missing offline edge only enlarges the computed concurrent set" argument
	// holds for the reorderability gate (dporConcurrent) alone — over-exploration —
	// but the same under-ordered trace-HB also feeds the WEAK-INITIAL computation,
	// where a spurious weak-initial can early-return addSourceBacktrack before the
	// genuine reversal is seeded: a dropped class while Exhausted reads true. The
	// live clocks above were already applied either way.
	if dstSyncEventN < len(dstSyncEventKind) {
		dstSyncEventKind[dstSyncEventN] = kind
		dstSyncEventID[dstSyncEventN] = id
		dstSyncEventAux[dstSyncEventN] = aux
		dstSyncEventSeq[dstSyncEventN] = seq
		dstSyncEventStep[dstSyncEventN] = int32(dstScheduleStep)
		dstSyncEventAcc[dstSyncEventN] = int32(dstAccLogN)
		dstSyncEventOrd[dstSyncEventN] = dstHBEventN
		dstHBEventN++
		dstSyncEventN++
	} else {
		dstSyncEventOverflow = true
	}
}

// dstFilterForceConservative forces the live shared-address filter into its
// conservative yield-everything mode. Test-only (set via the FP below): it
// gives enforcement tests an UNFILTERED exploration leg to cross-check the
// filtered outcome set against, so a filter defect cannot cancel out of a
// filtered-vs-filtered comparison.
var dstFilterForceConservative bool

//go:linkname dstFilterForceConservativeFP
func dstFilterForceConservativeFP(on bool) {
	dstFilterForceConservative = on
}

func dstExploreRecordUncaughtPanic(v any) bool {
	if !dstActive() || dstSchedKind != dstSchedScheduled {
		return false
	}
	gp := getg()
	if gp == nil || gp.bubble == nil || gp == gp.bubble.root {
		return false
	}
	if gp == gp.bubble.gcDrain {
		lock(&gp.bubble.mu)
		if gp.bubble.gcDrain == gp {
			gp.bubble.gcDrain = nil
			gp.bubble.gcDrainDied = true
		}
		unlock(&gp.bubble.mu)
	}
	if !dstExplorePanicSet {
		dstExplorePanicValue = v
		dstExplorePanicSet = true
		dstExplorePanicStep = int32(dstScheduleStep)
	}
	return true
}

func dstExploreDropPanicDefers(gp *g) {
	for p := gp._panic; p != nil; p = p.link {
		if !p.goexit {
			runningPanicDefers.Add(-1)
		}
	}
}

func dstExploreRecordDeadlock(reason string, bubble *synctestBubble) bool {
	if !dstActive() || dstSchedKind != dstSchedScheduled || bubble == nil {
		return false
	}
	if dstExploreDeadlock == "" {
		dstExploreDeadlock = reason
	}
	return true
}

//go:linkname dstRecordSyncRelease
func dstRecordSyncRelease(id unsafe.Pointer) {
	dstRecordSyncEvent(dstSyncEventRelease, id)
}

//go:linkname dstRecordSyncAcquire
func dstRecordSyncAcquire(id unsafe.Pointer) {
	dstRecordSyncEvent(dstSyncEventAcquire, id)
}

func dstRecordSyncReleaseID(id, aux uintptr) {
	dstRecordSyncEventID(dstSyncEventRelease, id, aux)
}

func dstRecordSyncAcquireID(id, aux uintptr) {
	dstRecordSyncEventID(dstSyncEventAcquire, id, aux)
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
	dstEdgeAcc = make([]int32, maxEdges)
	dstEdgeOrder = make([]int32, maxEdges)
	dstSyncEventKind = make([]uint8, maxEdges)
	dstSyncEventID = make([]uintptr, maxEdges)
	dstSyncEventAux = make([]uintptr, maxEdges)
	dstSyncEventSeq = make([]uint64, maxEdges)
	dstSyncEventStep = make([]int32, maxEdges)
	dstSyncEventAcc = make([]int32, maxEdges)
	dstSyncEventOrd = make([]int32, maxEdges)
	dstAccLogSeq = make([]uint64, maxAccesses)
	dstAccLogAddr = make([]uintptr, maxAccesses)
	dstAccLogSize = make([]uintptr, maxAccesses)
	dstAccLogPC = make([]uintptr, maxAccesses)
	dstAccLogCount = make([]uint64, maxAccesses)
	dstAccLogWrite = make([]bool, maxAccesses)
	dstAccLogStep = make([]int32, maxAccesses)
	dstClockProcs = maxDecisions
	if dstClockProcs > dstFilterMaxClockProcs {
		dstClockProcs = dstFilterMaxClockProcs
	}
	dstClock = make([]uint32, dstClockProcs*dstClockProcs)
	dstAccessTab = make([]int32, maxAccesses*2+1)
	dstAccessEntryAddr = make([]uintptr, maxAccesses)
	dstAccessEntrySize = make([]uintptr, maxAccesses)
	dstAccessEntryProc = make([]int32, maxAccesses)
	dstAccessNext = make([]int32, maxAccesses)
	dstAccessReadEpoch = make([]uint32, maxAccesses)
	dstAccessWriteEpoch = make([]uint32, maxAccesses)
	dstAccPageTab = make([]int32, maxAccesses*2+1)
	dstAccPageNodeEnt = make([]int32, maxAccesses*2)
	dstAccPageNodeNext = make([]int32, maxAccesses*2)
	dstAccPageNodePage = make([]uintptr, maxAccesses*2)
	syncClockObjects := maxEdges
	if syncClockObjects > dstFilterMaxSyncObjects {
		syncClockObjects = dstFilterMaxSyncObjects
	}
	dstSyncClockID = make([]uintptr, syncClockObjects)
	dstSyncClockAux = make([]uintptr, syncClockObjects)
	dstSyncClock = make([]uint32, syncClockObjects*dstClockProcs)
	dstSyncClockN = 0
}

// dstScheduleReset prepares the scheduled-strategy state for a new bubble. Called
// from synctestRun under dstActive when the scheduled strategy is active. Zeroes
// counters only — no allocation.
func dstScheduleReset() {
	dstScheduleStep = 0
	dstScheduleAborted = false
	dstSchedForeignSeen = false
	dstExplorePanicValue = nil
	dstExplorePanicSet = false
	dstExploreDeadlock = ""
	dstTraceN = 0
	dstTraceFlatN = 0
	dstTraceOverflow = false
	dstSeqCtr = 0
	dstEdgeN = 0
	dstEdgeOverflow = false
	dstHBEventN = 0
	dstSyncEventN = 0
	dstSyncEventOverflow = false
	dstScheduleAbortStep = -1
	dstExplorePanicStep = -1
	dstAccLogN = 0
	dstAccLogOverflow = false
	for i := range dstClock {
		dstClock[i] = 0
	}
	for i := range dstAccessTab {
		dstAccessTab[i] = 0
	}
	dstAccessEntryN = 0
	for i := range dstAccPageTab {
		dstAccPageTab[i] = 0
	}
	dstAccPageNodeN = 0
	dstAccPageChargeN = 0
	dstAccLargeN = 0
	for i := 0; i < dstSyncClockN*dstClockProcs; i++ {
		dstSyncClock[i] = 0
	}
	dstSyncClockN = 0
	dstFilterConservative = false
}

// dstSchedForeignSeen reports that a foreign candidate (a goroutine outside
// the simulation — not the sim bubble's own drain) was present at one of the
// scheduled strategy's recorded decisions this run. Read per episode by the
// explore harness (dstSchedForeignSeenFP), reset by dstScheduleReset.
var dstSchedForeignSeen bool

//go:linkname dstSchedForeignSeenFP
func dstSchedForeignSeenFP() bool { return dstSchedForeignSeen }

// dstScheduledSelect implements the scheduled strategy at the unified seam. The
// enabled set it assigns, matches against, and records is the SIMULATION's
// candidates only: dstFindRunnable schedules infrastructure first, and when its
// starvation-fairness hand-off passes a set that still contains infrastructure
// candidates (a persistently-runnable foreign goroutine), those are skipped —
// they are not part of the simulated program, must never enter the recorded
// schedule or the DPOR enabled sets, and their presence must not shift which
// simulation candidate a prefix entry resolves to. With a pure set the loops
// are unchanged. It records the enabled set + choice and returns the chosen
// candidate index. Allocation-free (runs under sched.lock).
func dstScheduledSelect(c *dstCandidates, total uint32) uint32 {
	// Assign stable per-bubble indices to any not-yet-seen simulation
	// candidate, in candidate order (deterministic per execution). dstSeq
	// stores index+1; 0 = unassigned. A FOREIGN candidate (infra other than
	// the sim bubble's own drain) is recorded in dstSchedForeignSeen:
	// exploration downgrades its exhaustion claim when foreign work was
	// runnable at its decisions, because foreign activity can perturb where
	// the instrumented yield points fall (observed under -race) — a coverage
	// effect that must be reported, never silent.
	for k := uint32(0); k < total; k++ {
		if gp := c.at(k); gp != nil {
			if !dstIsInfraCandidate(gp) {
				dstEnsureSeq(gp)
			} else if !(gp.bubble == dstSimBubble && gp.bubble != nil && gp.bubble.gcDrain == gp) {
				dstSchedForeignSeen = true
			}
		}
	}
	var sel uint32
	if dstScheduleStep < len(dstSchedulePrefix) {
		want := dstSchedulePrefix[dstScheduleStep]
		sel = ^uint32(0)
		for k := uint32(0); k < total; k++ {
			if gp := c.at(k); gp != nil && !dstIsInfraCandidate(gp) && gp.dstSeq == want {
				sel = k
				break
			}
		}
		if sel == ^uint32(0) {
			if !dstScheduleAborted {
				dstScheduleAborted = true
				dstScheduleAbortStep = int32(dstScheduleStep)
			}
			sel = dstLowestSeqIdx(c, total)
		}
	} else {
		sel = dstLowestSeqIdx(c, total)
	}
	// Record the decision (enabled set in candidate order + chosen id), bounded.
	// The enabled set is the simulation's candidates only (see the doc above);
	// with a pure set simTotal == total.
	simTotal := c.simCount(total)
	if dstTraceN < len(dstTraceChosen) && dstTraceFlatN+int(simTotal) <= len(dstTraceEnabFlat) {
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
		addr, pc, count := uintptr(0), uintptr(0), uint64(0)
		size := uintptr(0)
		write := false
		auto := false
		if chosen.dstAccPend {
			addr = chosen.dstAccAddr
			size = chosen.dstAccSize
			write = chosen.dstAccWrite
			pc = chosen.dstAccPC
			count = chosen.dstAccCount
			auto = chosen.dstAccAuto
		}
		dstCommitAccess(chosen, chosen.dstSeq, addr, size, write, pc, count, auto, dstScheduleStep+1)
		// Consume the pending access: the transition about to run performs it
		// exactly once. Without this, a goroutine that next blocks at a *coarse*
		// point (channel/WaitGroup) rather than at another dstAccessYield would carry
		// a stale address into that later decision — over-approximating the
		// dependency relation (extra DPOR exploration; still sound, never a missed
		// class). The goroutine sets a fresh dstAccAddr at its next dstAccessYield.
		chosen.dstAccAddr = 0
		chosen.dstAccSize = 0
		chosen.dstAccWrite = false
		chosen.dstAccPC = 0
		chosen.dstAccPend = false
		chosen.dstAccAuto = false
		dstTraceEnabOff[dstTraceN] = int32(dstTraceFlatN)
		dstTraceEnabLen[dstTraceN] = int32(simTotal)
		for k := uint32(0); k < total; k++ {
			if gp := c.at(k); gp != nil && !dstIsInfraCandidate(gp) {
				dstTraceEnabFlat[dstTraceFlatN] = gp.dstSeq
				dstTraceFlatN++
			}
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
// first child in the exhaustive DFS). Infrastructure candidates are skipped:
// the fairness hand-off (dstFindRunnable) can pass a mixed set, and only
// simulation candidates carry assigned seqs. All simulation candidates are
// assigned by the caller before this runs, and the caller guarantees at least
// one is present. Allocation-free.
func dstLowestSeqIdx(c *dstCandidates, total uint32) uint32 {
	best := ^uint32(0)
	var bestSeq uint64
	for k := uint32(0); k < total; k++ {
		gp := c.at(k)
		if gp == nil || dstIsInfraCandidate(gp) {
			continue
		}
		if best == ^uint32(0) || gp.dstSeq < bestSeq {
			best, bestSeq = k, gp.dstSeq
		}
	}
	if best == ^uint32(0) {
		throw("dst: no simulation candidate in scheduled selection")
	}
	return best
}

// --- brain-facing API (linkname'd; called on normal goroutines, off-lock) ------

// dstSetSchedule installs the prefix the next scheduled Run follows. The brain
// owns the slice and must not mutate it during the Run.
//
//go:linkname dstSetSchedule
func dstSetSchedule(prefix []uint64) { dstSchedulePrefix = prefix }

// dstSetAccessForce installs replay watchpoints for filtered access-log entries the
// brain has proven need a real yield boundary. Each triple is (dstSeq, per-g hook
// ordinal, hook PC key). The brain owns the slices and must not mutate them during the Run.
//
//go:linkname dstSetAccessForce
func dstSetAccessForce(seq, count []uint64, pc []uintptr) {
	dstForceSeq = seq
	dstForceCount = count
	dstForcePC = pc
	// Build the O(1) lookup table here, off-lock; dstAccessForced
	// probes it on every auto-instrumented access during the Run.
	if len(seq) == 0 {
		dstForceTab = nil
		dstForceMask = 0
		return
	}
	size := 8
	for size < len(seq)*2 {
		size <<= 1
	}
	dstForceTab = make([]int32, size)
	dstForceMask = uintptr(size - 1)
	for i := range seq {
		h := dstForceHash(seq[i], count[i], pc[i]) & dstForceMask
		for dstForceTab[h] != 0 {
			h = (h + 1) & dstForceMask
		}
		dstForceTab[h] = int32(i + 1)
	}
}

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

//go:linkname dstExplorePanicFP
func dstExplorePanicFP() (any, bool) { return dstExplorePanicValue, dstExplorePanicSet }

//go:linkname dstExploreDeadlockFP
func dstExploreDeadlockFP() string { return dstExploreDeadlock }

//go:linkname dstRunningPanicDefersFP
func dstRunningPanicDefersFP() uint32 { return runningPanicDefers.Load() }

//go:linkname dstCurrentSeqFP
func dstCurrentSeqFP() uint64 { return getg().dstSeq }

// dstEdgeLenFP reports the happens-before edge count recorded by the last run, and
// dstEdgeAtFP reports edge i as (readier index, readied index, the dstScheduleStep
// at which the goready occurred, and the access-log length at that moment).
// dstEdgeOverflowFP reports a budget overflow.
//
//go:linkname dstEdgeLenFP
func dstEdgeLenFP() int { return dstEdgeN }

//go:linkname dstEdgeAtFP
func dstEdgeAtFP(i int) (from, to uint64, step, acc int) {
	return dstEdgeFrom[i], dstEdgeTo[i], int(dstEdgeStep[i]), int(dstEdgeAcc[i])
}

//go:linkname dstEdgeOrderFP
func dstEdgeOrderFP(i int) int { return int(dstEdgeOrder[i]) }

//go:linkname dstEdgeOverflowFP
func dstEdgeOverflowFP() bool { return dstEdgeOverflow }

//go:linkname dstSyncEventLenFP
func dstSyncEventLenFP() int { return dstSyncEventN }

//go:linkname dstSyncEventAtFP
func dstSyncEventAtFP(i int) (kind uint8, id, aux uintptr, seq uint64, step, acc, order int) {
	return dstSyncEventKind[i], dstSyncEventID[i], dstSyncEventAux[i], dstSyncEventSeq[i], int(dstSyncEventStep[i]), int(dstSyncEventAcc[i]), int(dstSyncEventOrd[i])
}

// dstAccLogLenFP reports the access-log entry count recorded by the last run;
// dstAccLogAtFP reports entry i as (accessing goroutine dstSeq, addr, size, hook PC key, hook ordinal, isWrite, the
// dstScheduleStep it occurred under). The brain sources DPOR's dependency/HB relation
// from this log (decoupled from the decision trace) so single-owner accesses can be
// filtered to non-yields without losing the dependency. dstAccLogOverflowFP reports a
// budget overflow (coverage incomplete — never a silent cap).
//
//go:linkname dstAccLogLenFP
func dstAccLogLenFP() int { return dstAccLogN }

//go:linkname dstAccLogAtFP
func dstAccLogAtFP(i int) (seq uint64, addr uintptr, size uintptr, pc uintptr, count uint64, write bool, step int) {
	return dstAccLogSeq[i], dstAccLogAddr[i], dstAccLogSize[i], dstAccLogPC[i], dstAccLogCount[i], dstAccLogWrite[i], int(dstAccLogStep[i])
}

//go:linkname dstAccLogOverflowFP
func dstAccLogOverflowFP() bool { return dstAccLogOverflow }

//go:linkname dstRaceEnabledFP
func dstRaceEnabledFP() bool { return raceenabled }

// dstScheduleAbortedFP reports whether the last run's prefix was invalid for that
// execution (named a non-enabled dstSeq at some decision).
//
//go:linkname dstScheduleAbortedFP
func dstScheduleAbortedFP() bool { return dstScheduleAborted }

// dstScheduleAbortStepFP / dstExplorePanicStepFP report the decision step of the
// prefix abort and of the first recorded SUT panic (-1 = none) for the harness's
// panic-truncation attribution (a pre-panic abort is a real DST-L2-2 violation).
//
//go:linkname dstScheduleAbortStepFP
func dstScheduleAbortStepFP() int32 { return dstScheduleAbortStep }

//go:linkname dstExplorePanicStepFP
func dstExplorePanicStepFP() int32 { return dstExplorePanicStep }

//go:linkname dstSyncEventOverflowFP
func dstSyncEventOverflowFP() bool { return dstSyncEventOverflow }
