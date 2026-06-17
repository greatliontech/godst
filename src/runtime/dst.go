// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Deterministic simulation testing (DST) runtime support.
//
// When DST is active (dstSeed != 0), runtime randomness that is
// application-observable — select poll order, map seed and iteration order, the
// math/rand and math/rand/v2 globals, and synctest fake-timer ordering — is
// drawn from a per-goroutine stream (g.dstrand) seeded as a deterministic tree,
// rather than from the per-m cheaprand/chacha8 streams. Combined with a single P
// (GOMAXPROCS=1) and no asynchronous or time-based preemption, this makes
// goroutine scheduling and randomness a reproducible function of the seed.
//
// The public entry point is package testing/simulation. dstActivate is also linkname'd
// by the runtime's own white-box tests, which exercise the per-g mechanism under
// GOMAXPROCS>1 M-migration that testing/simulation.Run (single-P) cannot reproduce.
// See docs/dst/design.md.

package runtime

import (
	"internal/runtime/atomic"
	_ "unsafe" // for go:linkname
)

// dstSeed is the process DST seed: 0 means DST is off, non-zero means on and is
// the root seed. It is set at runtime by dstActivate (not at startup), read by
// the per-g routing hot paths and by sysmon.
var dstSeed atomic.Uint64

// dstRunEpoch is a monotonic counter bumped once per run (dstActivate), so a
// consumer can detect a new run and reset per-run in-memory state.
var dstRunEpoch atomic.Uint64

// dstPreparing is true during dstActivate's pre-active GC queue-detach pass.
// It suppresses ordinary async finalizer/cleanup workers while dstActive is still
// false, so process-level callbacks cannot start running and remain counted as
// pending inside the upcoming run.
var dstPreparing atomic.Bool

// dstCallbackWorkersBlocked reports whether process-global finalizer/cleanup
// workers must not dequeue more callback work. The workers may already exist (or
// already be running a pre-bubble callback) before a Run starts, so the scheduler
// wake gates are not sufficient by themselves: the worker loops also check this
// before taking another callback block.
//
//go:nosplit
func dstCallbackWorkersBlocked() bool {
	return dstActive() || (dstBuild && dstPreparing.Load())
}

// dstNetEpoch returns the current run's epoch (0 outside a run). The name
// predates the second consumer: both the net registry and the os simulated
// filesystem key their per-run state off this counter. net keys its
// simulated-network registry by it: a different epoch means a new run, so the
// registry resets — keeping listeners from one run out of the next, with no
// explicit teardown hook. Read by net via linkname.
//
//go:linkname dstNetEpoch
func dstNetEpoch() uint64 {
	if !dstActive() {
		return 0
	}
	return dstRunEpoch.Load()
}

// dstActive reports whether deterministic simulation testing is active.
//
// The dstBuild guard is load-bearing for cost, not just correctness: dstBuild is
// a constant (dst_on.go/dst_off.go), so in a non-`-tags dst` build dstActive()
// folds to a constant false and every `if dstActive()` branch — on the rand() and
// scheduler hot paths included — is dead-code-eliminated. The DST machinery then
// has zero footprint unless the build opted in. In a `-tags dst` build the guard
// is true and this is the runtime seed load as before. (dstSeed is never set
// without dstBuild anyway: simulation.Run requires the tag, and dstActivate is an
// unexported test-only linkname.)
//
// The push linkname lets net pull it to gate the simulated network at Dial/Listen.
//
//go:nosplit
//go:linkname dstActive
func dstActive() bool {
	return dstBuild && dstSeed.Load() != 0
}

// dstBuilt reports whether the program was built with -tags dst (so the map hash
// key is deterministic). testing/simulation.Run requires this.
//
//go:linkname dstBuilt
func dstBuilt() bool {
	return dstBuild
}

// dstActivate turns on DST with the given seed (0 is treated as 1) and roots the
// per-g DST tree at the calling goroutine, so goroutines it subsequently creates
// descend from this seed. A synctest bubble re-roots its own subtree from the
// seed independently (see synctestRun), so bubble randomness does not depend on
// the activation order.
//
//go:linkname dstActivate
func dstActivate(seed uint64) {
	if seed == 0 {
		seed = 1
	}
	// Record the activating goroutine: the simulation's own bubble is the one
	// created by THIS goroutine's synctest.Run call (runLocked calls it right
	// after activation). Identity, not order: dstActivate blocks in its setup
	// GCs below, so a foreign goroutine can run — and start a foreign synctest
	// bubble — between activation and the simulation's own synctest.Run. A
	// foreign bubble must NOT claim dstSimBubble: it would steal the
	// simulation's re-root/drain and demote the simulated program to RNG-free
	// infrastructure scheduling.
	dstSimRootG = getg()
	// Bump the per-run epoch so per-run in-memory state keyed by it (e.g. net's
	// simulated-network registry) resets between runs without an explicit hook:
	// a consumer that sees a new epoch discards its old state. One dstActivate per
	// simulation.Run, so one epoch per run/bubble.
	dstRunEpoch.Add(1)
	// Root the caller, then turn routing on. Correctness does not rely on the
	// atomic ordering the store provides: every goroutine that can draw under DST
	// is either created after activation (newproc1 seeds it from its parent, with
	// goroutine creation establishing happens-before) or is the caller itself,
	// rooted here; a synctest bubble re-roots its main independently. The store
	// order just avoids the caller observing dstActive with an unrooted dstrand.
	getg().dstrand = dstBubbleRoot(seed)
	// Root the caller's host/process identity at the default (0,0); the bubble
	// main re-roots to (0,0) too (synctestRun), and Host/Process stamp subtrees
	// from there. Only bubble goroutines are attributed: a goroutine that already
	// existed before activation keeps its current (host,proc), and the white-box
	// dstActivate path (GOMAXPROCS>1, no bubble) never reads identity — mirroring
	// how it leaves the simulated process identity unset.
	getg().dstHost = 0
	getg().dstProc = 0
	getg().dstClockOffset = 0
	getg().dstPid = int32(dstSimPID) // root pid; dstSetSimEnv ran before activation
	dstSchedRand = dstSchedRoot(seed)
	dstFaultRand.Store(dstFaultRoot(seed))
	// Queue process-level finalizers/cleanups before DST is active and detach them
	// from the queues the bubble drain observes. They are not part of this run's
	// deterministic universe: running them here could block Run entry or consume
	// seeded DST state, and leaving them queued would let them run in the first
	// bubble drain. They are released back to the ordinary async pools at
	// dstDeactivate.
	dstPreparing.Store(true)
	for range 2 {
		GC()
		dstDeferPreBubbleFinq()
		dstDeferPreBubbleCleanups()
	}
	dstPreparing.Store(false)
	// Free callback chains a previous run's drain abandoned without dying (the
	// drain left parked forever inside a callback at a recorded deadlock, or a
	// Run deadlock panic was recovered). A later run's discard must never
	// splice a dead run's blocks into its own ledger.
	dstDiscardAbandonedDrainChains()
	dstResetFinqRunCounters()
	dstResetCleanupRunCounters()
	dstSeed.Store(seed)
	// Establish the per-bubble heap baseline: a full GC here (STW now that DST is
	// active) collects pre-bubble garbage so gcController.heapMarked is the
	// process *live* set, and we snapshot it as the baseline the relative heap
	// trigger subtracts out. Without the GC the baseline would include pre-bubble
	// garbage that the first in-bubble GC then frees, driving heapMarked below the
	// baseline and breaking the relative computation. See docs/dst/gc.md
	// (Tier 2, per-bubble relative trigger).
	GC()
	dstHeapBase.Store(gcController.heapMarked)
	dstFinqBase.Store(finqueued)
}

// dstSetNode stamps the calling goroutine's DST host/process identity and returns
// the previous values, so testing/simulation.Host/Process can restore them when
// their body returns. The identity inherits to child goroutines at newproc1 (the
// labeled-subtree tree), so stamping the calling goroutine for the dynamic extent
// of a Host/Process body labels its whole subtree. The runtime carries integer
// ids; the string↔id interning is in testing/simulation. Reached via //go:linkname.
//
//go:linkname dstSetNode
func dstSetNode(host, proc uint32) (oldHost, oldProc uint32) {
	gp := getg()
	oldHost, oldProc = gp.dstHost, gp.dstProc
	gp.dstHost = host
	gp.dstProc = proc
	return
}

// dstCurrentNode returns the calling goroutine's DST host/process identity (0,0 is
// the root/default host and process). testing/simulation.Process reads it to detect
// whether it runs inside a Host body (host != 0); white-box tests read it directly.
//
//go:linkname dstCurrentNode
func dstCurrentNode() (host, proc uint32) {
	gp := getg()
	return gp.dstHost, gp.dstProc
}

// dstSetHostClockOffset stamps the calling goroutine's per-host wall-clock offset
// (nanoseconds) and returns the previous value, so testing/simulation.Host can
// restore it when its body returns. Like dstHost/dstProc the offset inherits to
// child goroutines at newproc1 (the labeled subtree), so stamping the Host body's
// goroutine applies the host's clock to its whole subtree; Process does not touch
// it (a process inherits its host's clock). It shifts only time.Now's wall reading
// inside an active run (runtime/time.go time_runtimeNow); monotonic time and timer
// deadlines, which read bubble.now directly, are unaffected — a static offset
// preserves durations. This is the L2 collapse of the per-host clock function
// wall = f_h(base) (docs/dst/faults.md "Per-host clock"): the constant case
// base + offset_h. Reached via //go:linkname.
//
//go:linkname dstSetHostClockOffset
func dstSetHostClockOffset(offset int64) (old int64) {
	gp := getg()
	old = gp.dstClockOffset
	gp.dstClockOffset = offset
	return
}

// dstClockOffsetNow returns the calling goroutine's host clock offset (the
// nanoseconds time.Now reads ahead of the universe base clock on this host).
// The simulated network reads it to convert a host-skewed wall reading into
// universe BASE time: link latency is a base-time duration (a skewed clock
// shifts time.Now but never durations — the per-host clock contract), so
// delivery must be gated in base time, consistently across hosts that disagree
// on wall time. Zero outside a host (and outside simulation). Reached via
// //go:linkname.
//
//go:linkname dstClockOffsetNow
func dstClockOffsetNow() int64 { return getg().dstClockOffset }

// dstHostSeededClockOffset returns a deterministic per-host wall-clock offset in
// [-bound, +bound] nanoseconds, a stateless function of the run seed and the host
// id (testing/simulation.BoundedSkew). It advances no RNG stream — neither a per-g
// tree (g.dstrand) nor the scheduling RNG (dstSchedRand) — so seeding a host's skew
// can neither perturb the program's interleaving nor shift any other draw, and the
// offset is stable across a host re-declaration (restart) because it depends only
// on (seed, host id). Sweeping the run seed sweeps the whole bounded
// skew-assignment space, which is how a bounded static skew is explored
// (faults.md "Per-host clock": "seeded within a bound"). A non-positive bound is no
// skew. Reached via //go:linkname.
//
//go:linkname dstHostSeededClockOffset
func dstHostSeededClockOffset(hostid uint32, bound int64) int64 {
	if bound <= 0 {
		return 0
	}
	// splitmix64 finalizer over (seed, salt, host id): stateless, advances nothing.
	x := dstSeed.Load() ^ 0xC10CC10CC10CC10C ^ (uint64(hostid) * 0x9e3779b97f4a7c15)
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	x ^= x >> 31
	// Map uniformly into [-bound, bound]. Compute the span and the centering in
	// uint64 so neither 2*bound nor the subtraction overflows int64; the signed
	// result lands in [-bound, +bound] for every bound in [1, MaxInt64] (for r <
	// bound the uint64 subtraction wraps to the correct two's-complement negative).
	span := 2*uint64(bound) + 1
	return int64(x%span - uint64(bound))
}

// dstHeapBase is the process-live heap snapshot taken at bubble entry (after a
// forced GC). The DST heap trigger (gcTrigger.test) fires on the bubble's growth
// relative to this baseline, so the process's pre-bubble heap history — which
// varies run to run — does not perturb the GC *set level* inside the bubble: the
// GC count and the total set of finalizers/weak refs discovered are a
// deterministic function of the bubble's own allocation (the contract; DST-GC-1).
//
// The trigger crossing itself is driven off per-object allocated bytes
// (dstHeapAlloc), not physical heapLive, so *which cycle* discovers a given object
// is also a deterministic function of the seed in normal AND -race builds (Phase
// 2a; see dstHeapAlloc and gc.md "How per-cycle discovery is made deterministic
// under -race"). dstHeapBase only enters the GOGC-scaled *target*
// ((heapMarked - dstHeapBase)*GOGC/100); it carries a rare sub-object residual
// (a pre-bubble transient captured at entry) that does not flip discovery and is
// the HeapAlloc/HeapInuse byte-noise class (DST-MEM-1). If a SUT drops references
// to pre-bubble objects so process-live falls below this baseline, the trigger
// degrades soundly to the heapMinimum floor (gcTrigger.test guards heapMarked >
// dstHeapBase).
var dstHeapBase atomic.Uint64

// dstFinqBase is the finqueued snapshot at bubble entry. finqueued is a
// process-cumulative counter, and the entry GC queues a run-to-run-varying number
// of pre-bubble finalizers, so only the delta from this baseline is
// bubble-deterministic.
var dstFinqBase atomic.Uint64

// dstHeapAlloc is the count of bytes allocated since the last GC, summed
// per-object at the mallocgc dispatcher under dstActive using each object's
// size-class size (elemsize). It is the deterministic, per-object analogue of
// heapLive's growth (heapLive - heapMarked) used to drive the DST heap trigger.
//
// heapLive advances *span-granularly* — gcController.update accounts a whole span
// (npages*pageSize) when it is grabbed (mcache.go), so heapLive - heapMarked jumps
// in span chunks and the allocation at which it crosses the GC trigger depends on
// the bubble's entry span-fill phase, which varies run to run. That moves *which*
// GC cycle discovers a given object (set-level totals are unaffected; per-cycle
// discovery is) — in normal builds, and worse under -race. Summing elemsize
// per-object instead advances exactly once per allocation, so the crossing lands
// at a deterministic allocation. elemsize is a deterministic function of the
// requested size (size classes are fixed) and is -race-invariant (the race
// detector uses shadow memory, not object redzones), and it is in heapMarked's
// units (the GC counts the same slot size), so the GOGC-scaled comparison below is
// exact, not merely proportional.
//
// Every DST heap-trigger crossing (gcTrigger.test) fires on this counter: the
// *floored* case (target == heapMinimum: GOGC=off, or a live set small enough that
// the GOGC target floors), the *GOGC-scaled* case (target ==
// (heapMarked - base)*GOGC/100), and the *Options.MemoryLimit* case (the bubble's
// net heap bubbleMarked + dstHeapAlloc vs the limit). The dispatcher checks the
// trigger on every allocation (not only at span grabs), so the cycle boundary lands
// at the exact per-object crossing — making per-cycle finalizer/weak discovery a
// deterministic function of the seed in normal AND -race builds, not merely the GC
// set level. (The bubbleMarked term in the GOGC-scaled and MemoryLimit targets
// carries a rare sub-object residual from the dstHeapBase process baseline; it
// cancels heapMarked's own baseline so it is build-invariant, and is the
// HeapAlloc/HeapInuse byte-noise class — sub-observable, see DST-MEM-1.)
//
// Reset to 0 at every GC (resetLive, the same point heapLive resets to
// heapMarked) and at bubble entry (synctestRun), so it measures the bubble's own
// net allocation since the last collection.
var dstHeapAlloc atomic.Uint64

// dstBubbleFinqFP returns the bubble-local count of finalizers queued so far
// (finqueued minus the bubble-entry baseline). It is the set-level observable
// (DST-GC-1): the GC count and the total set of discovered finalizers are
// deterministic under -race. Read at a fixed mid-run allocation it is also a
// *per-cycle* observable (how many finalizers the cycles so far discovered), which
// is deterministic too now that the trigger fires on per-object dstHeapAlloc
// (Phase 2a; TestDSTGCPerCycleDiscoveryDeterministic). Note it also counts
// pre-bubble stdlib finalizers that survive the entry GC and die in-bubble — a
// constant within one binary but build-varying, so the per-cycle test asserts
// within-build replay, not cross-build identity.
//
//go:linkname dstBubbleFinqFP
func dstBubbleFinqFP() uint64 { return finqueued - dstFinqBase.Load() }

// dstSchedRand is the per-bubble DST scheduling RNG state (splitmix64). Unlike
// the per-g streams (g.dstrand), scheduling decisions run on g0 — a system
// goroutine with no application-meaningful per-g stream — so seeded scheduling
// draws from this single dedicated stream. At GOMAXPROCS=1 the sequence of
// scheduling decisions is itself deterministic, so a single stream advanced once
// per decision is a deterministic function of the seed. It is rooted at
// activation (dstActivate) and re-rooted per synctest bubble (synctestRun), like
// the per-g tree, so a bubble's interleaving is reproducible in isolation. The
// 0x5C salt keeps it independent of the per-g root derived from the same seed.
var dstSchedRand uint64

// dstSchedRoot derives the scheduling RNG root from the DST seed, salted to be
// independent of dstBubbleRoot's per-g tree root for the same seed.
func dstSchedRoot(seed uint64) uint64 {
	return dstBubbleRoot(seed ^ 0x5C7ED000_5C7ED000)
}

// dstFaultRand is the per-bubble DST fault RNG state (splitmix64): the dedicated
// seeded stream for fault decisions (a jitter draw, later which victim / when in a
// window). Rooted at activation and re-rooted per bubble like dstSchedRand, so
// faults replay in isolation (DST-FAULT-REPLAY). Its salt makes it a stream
// INDEPENDENT of both the per-g tree and the scheduling RNG: a fault policy's draw
// count never shifts dstSchedRand's sequence, so the program's interleaving is the
// same whatever (and however often) faults draw — the stream-isolation discipline
// that keeps each policy's determinism independent of the others'. (A fault
// *changing* the execution — a longer delivery delay — is intended; only the fault
// *choices* are stream-isolated.) Unlike dstSchedRand (advanced on g0 at the
// scheduling seam, single-context), this is drawn from bubble goroutines (e.g. a
// conn's delivery push), so it is atomic: at GOMAXPROCS=1 the draws are serialized
// in the deterministic cooperative-schedule order, and the atomic supplies the
// happens-before that keeps the access -race-clean.
var dstFaultRand atomic.Uint64

// dstFaultRoot derives the fault RNG root from the DST seed, salted to be
// independent of both the per-g tree root (dstBubbleRoot) and the scheduling RNG
// root (dstSchedRoot) for the same seed.
func dstFaultRoot(seed uint64) uint64 {
	return dstBubbleRoot(seed ^ 0xFA017000_FA017000)
}

// dstFaultRandUint64 advances and returns the next fault-RNG value. Reached via
// //go:linkname from packages that inject faults (e.g. net's delivery jitter).
//
//go:linkname dstFaultRandUint64
func dstFaultRandUint64() uint64 {
	// CAS loop rather than Add: the splitmix64 increment does not fit the signed
	// delta of atomic.Uint64.Add. At GOMAXPROCS=1 with async preemption off the
	// Load→CAS window has no concurrent writer, so it succeeds first try and the
	// advance order is the deterministic cooperative-schedule order; the atomic
	// keeps it -race-clean across the per-stream locks that callers hold.
	var z uint64
	for {
		old := dstFaultRand.Load()
		z = old + 0x9e3779b97f4a7c15
		if dstFaultRand.CompareAndSwap(old, z) {
			break
		}
	}
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// dstFaultRandN returns a deterministic value in [0, n) from the fault RNG,
// advancing it; n <= 0 returns 0 WITHOUT drawing (so an inactive fault leaves the
// stream — and every later fault's draw position — untouched). Reached via
// //go:linkname.
//
//go:linkname dstFaultRandN
func dstFaultRandN(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(dstFaultRandUint64() % uint64(n))
}

// dstSchedRandUint64 advances and returns the next scheduling-RNG value.
//
//go:nosplit
func dstSchedRandUint64() uint64 {
	dstSchedRand += 0x9e3779b97f4a7c15
	z := dstSchedRand
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	dstSchedRNGDraws++
	return z ^ (z >> 31)
}

// Per-bubble scheduling counters underpinning the system-goroutine-isolation
// invariant (see dstFindRunnable): under the Random strategy, rngDraws ==
// decisions - sysScheds — the scheduling RNG advances once per bubble-goroutine
// selection and never for a system (bubble==nil) one, so the program's
// interleaving is a pure function of the seed, independent of timing-/composition-
// varying system-goroutine activity. (Under PCT the bubble selection is priority-
// driven and draws dstSchedRand at goroutine creation, not at selection, so
// rngDraws is lower; the isolation — no system selection draws — is what matters
// and holds for both.) Reset per bubble. Read by the regression test via linkname.
var dstSchedDecisions, dstSchedSysScheds, dstSchedRNGDraws uint64

// dstSchedOvfPuts counts puts routed to the DST order-preserving ring overflow
// (p.dstRunqOvf) this bubble. Observability for the overflow-order regression:
// the test asserts the overflow path actually fired, so the order assertion is
// not vacuously passing on a run that never overflowed.
var dstSchedOvfPuts uint64

func dstSchedStatsReset() {
	dstSchedDecisions, dstSchedSysScheds, dstSchedRNGDraws, dstSchedOvfPuts = 0, 0, 0, 0
}

//go:linkname dstSchedOvfPutsFP
func dstSchedOvfPutsFP() uint64 { return dstSchedOvfPuts }

//go:linkname dstSchedStatsFP
func dstSchedStatsFP() (decisions, sysScheds, rngDraws uint64) {
	return dstSchedDecisions, dstSchedSysScheds, dstSchedRNGDraws
}

// dstSchedRandPeekFP reads the scheduling RNG state without advancing it, so a
// test can assert that fault-RNG draws leave the scheduling stream untouched
// (stream isolation — DST-FAULT-REPLAY). Reached via //go:linkname.
//
//go:linkname dstSchedRandPeekFP
func dstSchedRandPeekFP() uint64 { return dstSchedRand }

// dstSchedRandn returns a deterministic value in [0,n) from the scheduling RNG,
// advancing it. Used by the random strategy at the unified scheduling choice
// point (dstFindRunnable) to pick which runnable goroutine proceeds next.
//
//go:nosplit
func dstSchedRandn(n uint32) uint32 {
	return uint32((uint64(uint32(dstSchedRandUint64())) * uint64(n)) >> 32)
}

// Scheduling strategy under DST. The unified seam (dstFindRunnable) consults the
// active strategy to choose which runnable goroutine proceeds next. Random is the
// default (Seq 5a); PCT (Seq 5b) is the priority-based directed search. The kind
// and parameters are set once per dst.Run via dstSetSchedStrategy (before the
// bubble starts) and are constant for the run; the per-bubble *state* (PCT step
// counter, change points, goroutine priorities) re-roots per bubble in
// synctestRun, like dstSchedRand, so a run reproduces in isolation.
const (
	dstSchedRandom uint8 = iota
	dstSchedPCT
	dstSchedScheduled // follow an explicit schedule prefix (exhaustive / DPOR exploration); see dst_explore.go
)

var (
	dstSchedKind uint8 // dstSchedRandom | dstSchedPCT
	dstPCTDepth  int32 // PCT bug depth d (>=1)
	dstPCTSteps  int32 // PCT step-count estimate K (change points fall in [1,K])
)

// dstSetSchedStrategy selects the scheduling strategy for the next dst.Run. Called
// by testing/simulation.RunWith before activation; Run (no options) leaves the default
// random strategy.
//
//go:linkname dstSetSchedStrategy
func dstSetSchedStrategy(kind uint8, depth, steps int32) {
	dstSchedKind = kind
	dstPCTDepth = depth
	dstPCTSteps = steps
}

// dstPCTMaxDepth bounds the change-point arrays so PCT state is fixed-size (no
// allocation that could perturb GC). Bug depths beyond this are clamped.
const dstPCTMaxDepth = 16

// dstPCT is the per-bubble PCT scheduler state. Priorities live per goroutine
// (g.dstPrio); this holds the global step counter and the d-1 change points.
type dstPCTState struct {
	step      uint32                 // scheduling decisions taken so far this bubble
	nchange   int32                  // number of active change points (d-1)
	changeAt  [dstPCTMaxDepth]uint32 // step indices at which a deprioritization fires
	changeLow [dstPCTMaxDepth]int64  // the low priority assigned at each change point
	applied   [dstPCTMaxDepth]bool   // whether each change point has fired
}

var dstPCT dstPCTState

// dstPCTBase is the floor for randomly-assigned goroutine base priorities, kept
// well above the change-point low band (1..d-1) so any deprioritized goroutine
// sorts below every not-yet-deprioritized one.
const dstPCTBase = 1 << 20

// dstSchedRootPCT re-roots the PCT state for a new bubble: it (re)assigns the
// d-1 change points to random steps in [1,K] with descending low priorities, and
// resets the step counter. Called from synctestRun under dstActive when PCT is
// the active strategy, after dstSchedRand is rooted.
func dstSchedRootPCT() {
	d := dstPCTDepth
	if d < 1 {
		d = 1
	}
	if d > dstPCTMaxDepth {
		d = dstPCTMaxDepth
	}
	k := dstPCTSteps
	if k < 1 {
		k = 1
	}
	dstPCT = dstPCTState{nchange: d - 1}
	for i := int32(0); i < d-1; i++ {
		// A change point at a random step in [1,K]; low priority i+1 (in 1..d-1,
		// all below dstPCTBase). Distinctness of steps is not required for the
		// probabilistic guarantee.
		dstPCT.changeAt[i] = 1 + dstSchedRandn(uint32(k))
		dstPCT.changeLow[i] = int64(i + 1)
	}
}

// dstPCTAssignPrio gives a freshly created goroutine a random base priority in
// [dstPCTBase, dstPCTBase+2^31), inducing a random total order (ties broken by
// goid). Drawn from the scheduling RNG (not the per-g stream): priority is a
// scheduling property, and the creation order is deterministic, so the draw
// sequence is deterministic. Called from newproc1 under DST when PCT is active.
func dstPCTAssignPrio(newg *g) {
	newg.dstPrio = dstPCTBase + int64(dstSchedRandUint64()&0x7fffffff)
}

// Simulated process identity. Under DST the testing/simulation package fixes
// os.Getpid and os.Hostname to deterministic values, closing the determinism
// hole a SUT that reads pid/hostname (for node IDs, temp names, pid-seeded RNGs)
// would otherwise have: the real machine's pid/hostname vary per run and per host.
//
// Identity splits by ownership (docs/dst/faults.md "Per-process identity"):
// hostname and NumCPU are per-HOST, pid is per-PROCESS. These globals hold the
// run-wide *defaults* — set by dstSetSimEnv *before* dstActivate from
// testing/simulation Options: dstSimHostname is host 0's hostname, dstSimNumCPU the
// default NumCPU, dstSimPID the root (host-0/proc-0 driver) pid and the base of the
// per-process pid counter. Per-host overrides live in dstHostIdent (keyed by
// g.dstHost); the per-process pid lives on g.dstPid. dstSimEnvSet is false on the
// white-box dstActivate path (no public Run), so the real identity is returned
// there. The remaining identity (ppid, uid/gid, the current user) is fixed to the
// deterministic constants below, which testing/simulation documents.
var (
	dstSimPID      int
	dstSimHostname string
	dstSimNumCPU   int // default simulated runtime.NumCPU(); 0 leaves NumCPU real
	dstSimEnvSet   bool
)

// dstHostIdentity is a host's simulated identity: its os.Hostname and
// runtime.NumCPU (0 = use the run default dstSimNumCPU). Per-host so co-located
// processes share a hostname/NumCPU while different hosts can differ.
type dstHostIdentity struct {
	hostname string
	numcpu   int32
	set      bool
}

// dstHostIdentTable is the per-host identity vector, indexed by host id (slot 0 is
// unused — host 0 uses the run defaults). It is immutable once published: Host
// installs a new copy via a CAS on dstHostIdent, so reads (os.Hostname,
// runtime.NumCPU) are lock-free atomic loads with no per-g string storage and no
// runtime lock. The string lives in this one process-global table, not on every g.
type dstHostIdentTable struct {
	ent []dstHostIdentity
}

// dstHostIdent publishes the current per-host identity table. nil between runs and
// until the first Host declares identity. Reset to nil at dstSetSimEnv.
var dstHostIdent atomic.Pointer[dstHostIdentTable]

// dstSimPidNext is the per-process pid allocator: dstAllocPid bumps it, so each
// Process invocation (including a restart) gets a fresh, deterministic pid. Reset
// to the root pid (dstSimPID) at dstSetSimEnv, so the first process is root pid + 1.
var dstSimPidNext atomic.Int32

// Fixed simulated identity returned during a run for the parts testing/simulation
// does not make configurable. Deterministic constants so a SUT that derives state
// from them (file modes, default config/data dirs, uid-keyed maps) replays
// identically. uid/gid are a single int source of truth — os/user formats the
// string forms it reports via strconv.Itoa, so os.Getuid and os/user.Current
// cannot disagree. uid/gid are a distinctive value (not the ubiquitous 1000) so
// the simulated identity is observably an override rather than coinciding with a
// host's first interactive user.
const (
	dstSimPPID     = 1
	dstSimUID      = 7777
	dstSimGID      = 7777
	dstSimUsername = "sim"
	dstSimUserName = "sim" // full ("GECOS") name
	dstSimHomeDir  = "/home/sim"
)

// dstSetSimEnv records the simulated process identity for the next run. Called by
// testing/simulation.run before dstActivate; cleared by dstClearSimEnv on return.
//
//go:linkname dstSetSimEnv
func dstSetSimEnv(hostname string, pid, numcpu int) {
	dstSimHostname = hostname
	dstSimPID = pid
	dstSimNumCPU = numcpu
	dstSimEnvSet = true
	// Fresh per-host table; pid counter based at the root pid so the first process
	// gets root pid + 1; fresh per-process allocation table. All reset here (start of
	// run) rather than at clear so a re-run starts clean even if a prior run's defer
	// was skipped.
	dstHostIdent.Store(nil)
	dstSimPidNext.Store(int32(pid))
	dstProcAlloc.Store(nil)
}

// dstClearSimEnv stops simulating process identity (run end).
//
//go:linkname dstClearSimEnv
func dstClearSimEnv() {
	dstSimEnvSet = false
	dstSimHostname = ""
	dstSimPID = 0
	dstSimNumCPU = 0
	dstHostIdent.Store(nil)
	dstSimPidNext.Store(0)
	dstProcAlloc.Store(nil)
}

// dstSetHostIdent records host's simulated hostname and NumCPU (numcpu 0 = use the
// run default), called by testing/simulation.Host. It publishes a new immutable
// table via a CAS loop, so concurrent Host declarations on different hosts cannot
// lose an update and readers stay lock-free. Reached via //go:linkname.
//
//go:linkname dstSetHostIdent
func dstSetHostIdent(host uint32, hostname string, numcpu int) {
	for {
		old := dstHostIdent.Load()
		n := int(host) + 1
		if old != nil && len(old.ent) > n {
			n = len(old.ent)
		}
		ent := make([]dstHostIdentity, n)
		if old != nil {
			copy(ent, old.ent)
		}
		ent[host] = dstHostIdentity{hostname: hostname, numcpu: int32(numcpu), set: true}
		if dstHostIdent.CompareAndSwap(old, &dstHostIdentTable{ent: ent}) {
			return
		}
	}
}

// dstHostIdentFor returns host's recorded identity (host 0 and any host that never
// declared identity report not-found, so callers fall back to the run defaults). A
// lock-free atomic load of the immutable published table.
func dstHostIdentFor(host uint32) (dstHostIdentity, bool) {
	if host == 0 {
		return dstHostIdentity{}, false
	}
	t := dstHostIdent.Load()
	if t != nil && int(host) < len(t.ent) && t.ent[host].set {
		return t.ent[host], true
	}
	return dstHostIdentity{}, false
}

// dstAllocPid returns the next per-process pid (deterministic: the Process call
// order is a function of the seed). Reached via //go:linkname.
//
//go:linkname dstAllocPid
func dstAllocPid() int32 {
	return dstSimPidNext.Add(1)
}

// dstSetProcessPid stamps the calling goroutine's pid and returns the previous
// value, so testing/simulation.Process can restore it when its body returns. The
// pid inherits to child goroutines at newproc1 (the labeled subtree), so the whole
// process subtree reports one pid. Reached via //go:linkname.
//
//go:linkname dstSetProcessPid
func dstSetProcessPid(pid int32) (old int32) {
	gp := getg()
	old = gp.dstPid
	gp.dstPid = pid
	return
}

// Per-process allocation accounting (docs/dst/faults.md "Memory accounting"): each
// heap allocation by a process's goroutine adds its size-class size (elemsize) to
// the process's counter at the mallocgc hook (malloc.go), attributed to the
// allocating goroutine's process id (g.dstProc). It is allocation *flow*, not
// RSS/live-set — deterministic and -race-invariant (elemsize is, and the program's
// allocations are a function of the seed), the metric the L3 OOM fault thresholds.
//
// The table is a FIXED-size counter vector, allocated once per run and never grown.
// Fixed by design: a grow-on-demand (copy-on-write) table would mutate its shape
// while the hot path reads it on every allocation — a data race between the copy
// and concurrent declarations/reads. A stable backing array makes that race
// unrepresentable: the hot path indexes the array and does a single atomic add, and
// declarations only flip an already-present counter. Keyed by g.dstProc (the
// interned process id), so a logical process accumulates across a restart —
// faults.md attributes by dstProc; per-instance reset, if the OOM fault wants it, is
// a later increment over this seam.

// dstMaxSimProcs bounds the distinct processes (Process names) a single run may
// declare for allocation accounting. It fixes the counter table's size so the hot
// path never races a table growth; a run that declares more panics loudly (no
// silent drop), as net's routable-IP space does past its host bound. Generous:
// realistic simulations declare a handful to dozens of processes (a restart reuses
// the name's id, so restarts do not consume the budget).
const dstMaxSimProcs = 4096

// dstProcAllocTable is the per-run per-process allocation counters, indexed by
// process id (slot 0 unused: proc 0 is the un-budgeted root). Allocated once
// (dstProcAllocEnsure) and never grown, so its backing array is stable for the run.
type dstProcAllocTable struct {
	ctr [dstMaxSimProcs]atomic.Int64
}

var dstProcAlloc atomic.Pointer[dstProcAllocTable]

// dstProcAllocEnsure allocates the per-run counter table on the first Process
// declaration and bounds the process id. The table is fixed-size, so this never
// grows or copies it; concurrent first declarations each build a fresh zero table
// and CAS-publish, the losers discarding theirs (no copy → no race). Called by
// testing/simulation.Process before the process body runs. Reached via //go:linkname.
//
//go:linkname dstProcAllocEnsure
func dstProcAllocEnsure(procid uint32) {
	if procid == 0 {
		return
	}
	if procid >= dstMaxSimProcs {
		var buf [20]byte
		panic("testing/simulation: too many distinct processes for per-process allocation accounting (limit " + string(itoa(buf[:], dstMaxSimProcs)) + ")")
	}
	if dstProcAlloc.Load() == nil {
		// First Process of the run allocates the table once. This is a normal
		// in-bubble allocation (it flows through the mallocgc hook and is attributed
		// to the declaring goroutine's process, usually the root), deterministic
		// because the first Process is at a seed-deterministic point. Concurrent
		// first declarations each build a fresh zero table; the CAS losers discard
		// theirs — no copy, so no race with the hot path.
		dstProcAlloc.CompareAndSwap(nil, new(dstProcAllocTable))
	}
}

// dstProcAllocAdd attributes nbytes to process procid's counter (no-op for proc 0 or
// before the table exists). Called from the mallocgc hook under the simulation-bubble
// gate. nosplit: it runs on the allocation path; the procid < dstMaxSimProcs guard
// lets the compiler elide the array bounds check (no panicIndex on the nosplit path).
// The over-bound case cannot occur with a live table — dstProcAllocEnsure is the
// single choke point every Process passes and it panics on an over-bound id, so the
// guard here exists only to license bounds-check elision, not as a second check.
//
//go:nosplit
func dstProcAllocAdd(procid uint32, nbytes int64) {
	if procid == 0 || procid >= dstMaxSimProcs {
		return
	}
	if t := dstProcAlloc.Load(); t != nil {
		t.ctr[procid].Add(nbytes)
	}
}

// dstProcAllocBytes returns the bytes process procid has allocated this run (0 if
// none recorded). Read by tests and the OOM fault. Reached via //go:linkname.
//
//go:linkname dstProcAllocBytes
func dstProcAllocBytes(procid uint32) int64 {
	if procid == 0 || procid >= dstMaxSimProcs {
		return 0
	}
	if t := dstProcAlloc.Load(); t != nil {
		return t.ctr[procid].Load()
	}
	return 0
}

// The accessors below are read by os.Getpid/Getppid/Getuid/.../os.Hostname and
// os/user.Current (via linkname) to return the simulated identity during a run;
// the bool reports whether process identity is being simulated. runtime.NumCPU
// reads the per-host identity directly (same package).
//
// dstSimGetpid returns the calling goroutine's per-process pid (g.dstPid, stamped
// by Process and inherited by its subtree; the root pid for proc 0).
//
//go:linkname dstSimGetpid
func dstSimGetpid() (int, bool) { return int(getg().dstPid), dstSimEnvSet }

// dstSimGethostname returns the calling goroutine's host hostname: its per-host
// override if the host declared one, else the run default (host 0 / unconfigured).
//
//go:linkname dstSimGethostname
func dstSimGethostname() (string, bool) {
	// A declared host (set) reports its recorded hostname; only host 0 / an
	// undeclared host (dstHostIdentFor returns false) falls back to the run default.
	// "declared" is the single source of truth (id.set), so an explicitly-empty
	// hostname is not silently re-routed to the default.
	if id, ok := dstHostIdentFor(getg().dstHost); ok {
		return id.hostname, dstSimEnvSet
	}
	return dstSimHostname, dstSimEnvSet
}

//go:linkname dstSimGetppid
func dstSimGetppid() (int, bool) { return dstSimPPID, dstSimEnvSet }

//go:linkname dstSimGetuid
func dstSimGetuid() (int, bool) { return dstSimUID, dstSimEnvSet }

//go:linkname dstSimGetgid
func dstSimGetgid() (int, bool) { return dstSimGID, dstSimEnvSet }

// Effective uid/gid equal the simulated uid/gid (euid==uid, gid==egid, as in a
// non-setuid process).
//
//go:linkname dstSimGeteuid
func dstSimGeteuid() (int, bool) { return dstSimUID, dstSimEnvSet }

//go:linkname dstSimGetegid
func dstSimGetegid() (int, bool) { return dstSimGID, dstSimEnvSet }

// dstSimUser is read by os/user.Current to return the simulated current user. It
// returns uid/gid as ints from the single source of truth; os/user formats them.
//
//go:linkname dstSimUser
func dstSimUser() (uid, gid int, username, name, home string, ok bool) {
	return dstSimUID, dstSimGID, dstSimUsername, dstSimUserName, dstSimHomeDir, dstSimEnvSet
}

// dstMemLimit is the per-run deterministic bubble-local heap-growth budget
// (testing/simulation Options.MemoryLimit; 0 = no limit). Unlike GOMEMLIMIT —
// whose total-RSS semantics are not bubble-local and so not deterministic under
// DST (the scavenger is parked and total mapped memory carries process history) —
// this bounds the bubble's *own* heap growth (heapLive - dstHeapBase), which is
// deterministic, so the GC count under a memory limit is reproducible. Set before
// dstActivate, read by the heap trigger (gcTrigger.test).
var dstMemLimit int64

// dstSetMemLimit records the per-run bubble-local memory budget. Called by
// testing/simulation.run before dstActivate; reset to 0 on return.
//
//go:linkname dstSetMemLimit
func dstSetMemLimit(limit int64) { dstMemLimit = limit }

// dstNetCrossHostLatency is the per-run base (always-on) one-way latency in
// nanoseconds applied to every cross-host simulated TCP connection; 0 (the
// default, and same-host/loopback always) means instant delivery — byte-identical
// to a connection with no latency machinery. Set once before the bubble starts
// (from Options.Network.CrossHostLatency) and reset to 0 after the run, exactly
// like dstMemLimit, so it is read-only for the run's duration and safe for the
// simulated network's bubble goroutines to read without synchronization.
var dstNetCrossHostLatency int64

// dstSetNetCrossHostLatency sets the per-run cross-host base latency. Reached via
// //go:linkname from testing/simulation's run envelope.
//
//go:linkname dstSetNetCrossHostLatency
func dstSetNetCrossHostLatency(ns int64) { dstNetCrossHostLatency = ns }

// dstNetCrossHostLatencyNs reports the per-run cross-host base latency. Reached
// via //go:linkname from net (which has no import of runtime).
//
//go:linkname dstNetCrossHostLatencyNs
func dstNetCrossHostLatencyNs() int64 { return dstNetCrossHostLatency }

// dstNetCrossHostJitter is the per-run max delivery jitter in nanoseconds for
// cross-host connections: each segment's delivery is delayed by the base latency
// plus a value drawn from [0, dstNetCrossHostJitter) from the fault RNG. 0 (the
// default, and same-host always) means no jitter. Set once before the bubble and
// reset after, like dstNetCrossHostLatency.
var dstNetCrossHostJitter int64

// dstSetNetCrossHostJitter sets the per-run cross-host jitter. Reached via
// //go:linkname from testing/simulation's run envelope.
//
//go:linkname dstSetNetCrossHostJitter
func dstSetNetCrossHostJitter(ns int64) { dstNetCrossHostJitter = ns }

// dstNetCrossHostJitterNs reports the per-run cross-host jitter. Reached via
// //go:linkname from net.
//
//go:linkname dstNetCrossHostJitterNs
func dstNetCrossHostJitterNs() int64 { return dstNetCrossHostJitter }

// dstDeactivate turns DST off. Used by testing/simulation.Run to restore normal
// behavior after a run.
//
//go:linkname dstDeactivate
func dstDeactivate() {
	dstSeed.Store(0)
	dstSimRootG = nil
	dstSimBubble = nil
	// Hand any goroutines still in the DST ring-overflow queue (foreign
	// goroutines can legitimately be runnable at run end) back to the normal
	// scheduler, which does not look at p.dstRunqOvf. The store above already
	// turned the put-side gate off, and at GOMAXPROCS=1 the single P belongs to
	// the M running this code, so no put can race the flush.
	dstFlushRunqOvf()
	dstReleaseDeferredFinq()
	dstReleaseDeferredCleanups()
	dstWakeBlockedCleanupWorkers()
}

// dstFlushRunqOvf moves every P's DST overflow queue to the global run queue.
// See dstDeactivate. All Ps, not just the caller's: the run executes at one
// pinned P, but a foreign GOMAXPROCS call racing run entry can procresize
// mid-run (the setters' dstActive gates are check-then-act; the re-checks
// under their STWs close the public window, but a pin-less white-box run has
// no entry verify), after which the entries' P need not be the deactivator's;
// flushing
// every P bounds that race's damage to the run that suffered it instead of
// stranding goroutines forever.
func dstFlushRunqOvf() {
	lock(&sched.lock)
	for _, pp := range allp {
		if pp == nil || pp.dstRunqOvf.empty() {
			continue
		}
		var q gQueue
		q = pp.dstRunqOvf
		pp.dstRunqOvf = gQueue{}
		globrunqputbatch(&q)
	}
	unlock(&sched.lock)
}

// dstSimBubble is the active simulation's own synctest bubble: the goroutines
// the seeded scheduler treats as the simulated program. Goroutines outside any
// bubble — and goroutines of a FOREIGN bubble (a plain synctest bubble live
// concurrently with the simulation) — are scheduled RNG-free as infrastructure
// (see firstSystemG): letting a foreign bubble consume seed draws would make
// the simulation's schedule depend on unrelated process activity, breaking
// reproduction in isolation. dstSimRootG is the goroutine that ran dstActivate; only a
// synctest bubble created BY that goroutine claims dstSimBubble (identity, not
// creation order — dstActivate's setup GCs block, so foreign bubbles can be
// created in between). Written by the simulation's own goroutine and read by
// the single-P scheduler; cooperative scheduling serializes access.
var (
	dstSimBubble *synctestBubble
	dstSimRootG  *g
)

// dstCallbackEpoch returns the ownership stamp recorded on a finalizer/cleanup
// special at registration (SetFinalizer/AddCleanup): the current run epoch if
// the registering goroutine belongs to the active simulation's bubble, else 0.
// Queue-time routing (queuefinalizer, cleanupQueue.enqueue) compares the stamp
// against the then-current epoch: only a callback registered by THIS run's
// bubble goroutines is the run's own work and may run on the bubble drain;
// anything else — registered before the run, by a goroutine outside any bubble,
// by a FOREIGN synctest bubble, or by a previous run — is process-level work
// and is deferred past dstDeactivate exactly like the pre-bubble queues. The
// run's drained set is then a pure function of the run's own activity
// (reproducible in isolation); a foreign callback can never advance the drain
// goroutine's per-g RNG stream mid-run. May run on the system stack (uses
// m.curg). Folds to a constant 0 in non-dst builds.
func dstCallbackEpoch() uint64 {
	if !dstActive() {
		return 0
	}
	if gp := getg().m.curg; gp != nil && gp.bubble != nil && gp.bubble == dstSimBubble {
		return dstRunEpoch.Load()
	}
	return 0
}

// dstBubbleMainRoot derives bubble.main's per-bubble re-root from the seed,
// salted to be independent of dstBubbleRoot's activation root for the same
// seed. Without the salt, bubble.main replays the run caller's draw sequence —
// whose first two draws seeded bubble.main (overwritten) and bubble.gcDrain —
// so the second goroutine the SUT spawns would get a per-g stream
// bit-identical to the finalizer drain's (identical map seeds, math/rand and
// crypto/rand outputs).
func dstBubbleMainRoot(seed uint64) uint64 {
	return dstBubbleRoot(seed ^ 0xB1BB1E00_B1BB1E00)
}

// dstSetAsyncPreemptOff sets debug.asyncpreemptoff and returns its previous
// value, so testing/simulation.Run can disable asynchronous (signal-based) preemption
// for the duration of a run and restore it afterward. Disabling it (together
// with the sysmon gates, which key on dstActive) leaves preemption only at
// deterministic cooperative points.
//
//go:linkname dstSetAsyncPreemptOff
func dstSetAsyncPreemptOff(off bool) (old bool) {
	old = debug.asyncpreemptoff != 0
	if off {
		debug.asyncpreemptoff = 1
	} else {
		debug.asyncpreemptoff = 0
	}
	return old
}
