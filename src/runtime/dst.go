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
	"internal/abi"
	"internal/runtime/atomic"
	"unsafe" // race annotations + go:linkname
)

// dstInternalPooledTypes caches the type descriptors of the runtime-internal pooled
// structs the DST heap trigger EXCLUDES from its per-object counter (dstHeapAlloc):
// whether one is freshly allocated or reused from its cross-run-surviving cache
// (g→gFree, sudog→sudogcache, _defer→deferpool) is a pooling artifact of pre-run
// process history, not the SUT's heap growth, so counting it would move the GC cycle
// boundary run-to-run for channel/goroutine-heavy SUTs (gc.md M4). Set once per run in
// dstActivate (before any bubble allocation), read in mallocgc's DST gate; the values
// are the same every run, and a run is single-P, so there is no race.
var dstInternalPooledTypes struct {
	g, sudog, defr *_type
}

// dstIsInternalPooledType reports whether typ is one of the pooled internal structs
// the DST heap trigger excludes (see dstInternalPooledTypes). A nil typ (untyped raw
// allocation) is a SUT allocation and counts.
func dstIsInternalPooledType(typ *_type) bool {
	return typ != nil && (typ == dstInternalPooledTypes.g ||
		typ == dstInternalPooledTypes.sudog ||
		typ == dstInternalPooledTypes.defr)
}

// dstSeed is the process DST seed: 0 means DST is off, non-zero means on and is
// the root seed. It is set at runtime by dstActivate (not at startup), read by
// the per-g routing hot paths and by sysmon.
var dstSeed atomic.Uint64

// dstRunEpoch is a monotonic counter bumped once per run (dstActivate), so a
// consumer can detect a new run and reset per-run in-memory state.
var dstRunEpoch atomic.Uint64

// dstCallbackSeq is a per-run registration sequence stamped on each finalizer/cleanup
// at SetFinalizer/AddCleanup by a bubble goroutine (dstNextCallbackSeq), by which the
// synchronous drain orders its queued callbacks before running them (gc.md D4). Reset
// to 0 each run in dstActivate. Discovery hands the drain a SET in heap-address-
// dependent sweep order; executing in that order would make two same-cycle callbacks
// with interacting side effects an unseeded schedule fork. Registration order is a
// pure function of the run's own activity, so it is the replay-stable execution order.
// uintptr (not uint64) so it is exactly ONE word on every arch: finalizer.dstSeq rides
// in the layout-masked finalizer array the GC scan assumes is 6 words, and a run cannot
// register 2^32 callbacks.
var dstCallbackSeq atomic.Uintptr

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
// folds to a constant false and every `if dstActive()`/`if dstBuild` guard — on
// the rand(), scheduler, panic, finalizer, and synctest paths included — is
// dead-code-eliminated: zero CODE footprint unless the build opted in
// (TestDSTUntaggedCodeFootprint pins the fold; a generic like AddCleanup
// instantiates in user packages where dstActive does not inline, so such
// guards lead with the exported constant). The DATA layout is not
// zero-footprint, deliberately, in every build: g carries thirteen
// per-goroutine DST words (identity/RNG stamps and race-access staging), p a
// run-queue overflow flag, synctestBubble the drain bookkeeping,
// specialfinalizer/specialCleanup epoch/seq words, and finalizer/cleanupFn one
// registration-sequence word each. Splitting
// those by build tag would fork the runtime's central g struct and the
// hand-maintained GC bitmap constants (finalizer1's word pattern;
// cleanupBlockPtrMask, whose two-per-byte packing is load-bearing on the
// 4-word cleanupFn) into per-tag variants — an unsafe-critical duplication for
// a few words per object (design.md, "Untagged footprint"). In a `-tags dst`
// build the guard is true and this is the runtime seed load as before.
// (dstSeed is never set without dstBuild anyway: simulation.Run requires the
// tag, and dstActivate is an unexported test-only linkname.)
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
	if r := dstBubbleRoot(seed); r != 0 {
		getg().dstrand = r
	} else {
		getg().dstrand = 1 // keep a seeded root nonzero (dstReadRandom's unseeded sentinel is dstrand==0)
	}
	// Root the caller's host/process identity at the default (0,0); the bubble
	// main re-roots to (0,0) too (synctestRun), and Host/Process stamp subtrees
	// from there. Only bubble goroutines are attributed: a goroutine that already
	// existed before activation keeps its current (host,proc), and the white-box
	// dstActivate path (GOMAXPROCS>1, no bubble) never reads identity — mirroring
	// how it leaves the simulated process identity unset.
	getg().dstHost = 0
	getg().dstProc = 0
	getg().dstPid = int32(dstSimPID) // root pid; dstSetSimEnv ran before activation
	dstSchedRand = dstSchedRoot(seed)
	dstSchedPrevSys = false
	dstFaultRand.Store(dstFaultRoot(seed))
	// Queue process-level finalizers/cleanups before DST is active and detach them
	// from the queues the bubble drain observes. They are not part of this run's
	// deterministic universe: running them here could block Run entry or consume
	// seeded DST state, and leaving them queued would let them run in the first
	// bubble drain. They are released back to the ordinary async pools at
	// dstDeactivate.
	dstPreparing.Store(true)
	for range 2 {
		gcForce()
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
	dstCallbackSeq.Store(0) // per-run registration sequence for the drain's reg-order sort
	// Cache the internal pooled-struct type descriptors the heap trigger excludes,
	// BEFORE dstSeed.Store makes dstActive() true — so mallocgc's DST gate never reads a
	// nil cache (a nil would wrongly COUNT a g/sudog/_defer alloc).
	dstPooledAlloc.Store(0)
	dstPooledMarked.Store(0)
	dstPooledGBytes = uint64(roundupsize(abi.TypeFor[g]().Size_, false))
	dstInternalPooledTypes.g = abi.TypeFor[g]()
	dstInternalPooledTypes.sudog = abi.TypeFor[sudog]()
	dstInternalPooledTypes.defr = abi.TypeFor[_defer]()
	dstSeed.Store(seed)
	// Establish the per-bubble heap baseline: a full GC here (STW now that DST is
	// active) collects pre-bubble garbage so gcController.heapMarked is the
	// process *live* set, and we snapshot it as the baseline the relative heap
	// trigger subtracts out. Without the GC the baseline would include pre-bubble
	// garbage that the first in-bubble GC then frees, driving heapMarked below the
	// baseline and breaking the relative computation. See docs/dst/gc.md
	// (Tier 2, per-bubble relative trigger).
	gcForce()
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

//go:linkname dstVirtualMonotonicNow
//go:nosplit
func dstVirtualMonotonicNow() (int64, bool) {
	gp := getg()
	if gp.bubble == nil {
		return 0, false
	}
	return gp.bubble.now, true
}

// dstMaxSimHosts bounds the distinct hosts (Host names) a single run may declare
// for per-host clock state. Like dstMaxSimProcs it fixes the clock table's size so
// the time.Now read path never races a table growth; a run that declares more panics
// loudly (no silent drop). Generous: realistic simulations declare a handful to
// dozens of hosts (a restart reuses the name's id, so it does not consume the budget).
const dstMaxSimHosts = 4096

// dstHostClockTable is the per-run per-host clock vector, indexed by host id (slot 0
// unused: host 0 is the universe root/driver, always in sync with the base clock).
// Allocated once (dstHostClockEnsure) and never grown, so its backing array is stable
// for the run — the time.Now wall split reads it on every reading under simulation and
// a clock step/drift mutates one entry with atomic ops, so a fixed array keeps the read
// path race-free (a grow-on-demand table would race the copy against a concurrent
// read). Each entry is MUTABLE, unlike the immutable identity table dstHostIdent:
// StepClock adds to offset mid-run and DriftClock changes the rate mid-run.
// That is why the per-host clock lives here and not on g — a per-g snapshot, set at goroutine creation,
// could not be moved for a host's already-running goroutines. Keying reads by g.dstHost
// (inherited at newproc1) makes every goroutine of a host observe its host's CURRENT
// clock.
//
// A host's wall reading is wall = base + offset + drift, where the drift term
// (base − driftT0)·driftPPB/1e9 accumulates a rate departure of driftPPB parts-per-billion
// from base time, anchored at driftT0 (the base time the rate took effect). driftPPB 0
// is rate 1 (no drift): the drift term is 0 and an entry behaves as the pure skew/step
// offset, byte-identical to before drift existed.
type dstHostClockEntry struct {
	offset   atomic.Int64 // wall skew/step in ns (Skew/BoundedSkew set it, StepClock adds)
	driftPPB atomic.Int64 // clock-rate departure in parts-per-billion (0 = rate 1); rate = 1 + driftPPB/1e9
	driftT0  atomic.Int64 // base-time anchor (ns) from which the drift term accumulates
}

type dstHostClockTable struct {
	ent [dstMaxSimHosts]dstHostClockEntry
}

var dstHostClock atomic.Pointer[dstHostClockTable]

// dstDriftPPBBase is the parts-per-billion scale of a clock rate: rate =
// (dstDriftPPBBase + driftPPB) / dstDriftPPBBase. driftPPB 0 is rate 1, +dstDriftPPBBase
// is rate 2. driftPPB must stay > -dstDriftPPBBase so the rate stays positive — a stopped
// or reversed clock is a step, not drift (DST-CLOCK-DRIFT-MONOTONIC) — and ≤ dstMaxDriftPPB
// so the integer rate arithmetic (dstDriftAccum / dstDriftToBase) cannot overflow.
const (
	dstDriftPPBBase = 1_000_000_000   // 1e9
	dstMaxDriftPPB  = dstDriftPPBBase // rate in (0, 2]
)

// dstHostClockEnsure allocates the per-run clock table on first use and bounds the
// host id. Fixed-size, so this never grows or copies it; concurrent first uses each
// build a fresh zero table and CAS-publish, the losers discarding theirs (no copy →
// no race with the read path). Mirrors dstProcAllocEnsure. Linknamed so the bound
// test can drive the choke point directly (like dstProcAllocEnsure).
//
//go:linkname dstHostClockEnsure
func dstHostClockEnsure(host uint32) {
	if host >= dstMaxSimHosts {
		var buf [20]byte
		panic("testing/simulation: too many distinct hosts for per-host clock (limit " + string(itoa(buf[:], dstMaxSimHosts)) + ")")
	}
	if dstHostClock.Load() == nil {
		dstHostClock.CompareAndSwap(nil, new(dstHostClockTable))
	}
}

// dstDriftAccum returns the wall drift accumulated over an elapsed base interval at a
// rate departure of ppb parts-per-billion: elapsed·ppb/1e9, computed exactly in int64
// without overflow. elapsed·ppb can exceed int64 over a long run, so it is split as
// (elapsed/1e9)·ppb + (elapsed%1e9)·ppb/1e9 — each term in range for |ppb| ≤ dstMaxDriftPPB.
// elapsed < 0 (a reading before the anchor, not reachable in-spec) yields 0.
func dstDriftAccum(elapsed, ppb int64) int64 {
	if ppb == 0 || elapsed <= 0 {
		return 0
	}
	q := elapsed / dstDriftPPBBase
	r := elapsed % dstDriftPPBBase
	return q*ppb + r*ppb/dstDriftPPBBase
}

// dstMulDivClampCeil returns ⌈x·num/den⌉ exactly, overflow-safe in int64, for x ≥ 0 and
// 0 < num,den ≤ 2·dstDriftPPBBase (the rate-numerator range). x·num can exceed int64, so
// it is split as (x/den)·num + ⌈(x%den)·num/den⌉. A result that would still overflow —
// reachable only with an extreme slow target rate over a multi-century duration — is
// clamped to maxWhen, never wrapping negative. BOTH terms of the sum are checked, not
// just the high one. The clamp bounds only this product: a caller that ADDS the result
// to a base time must clamp that addition itself (dstTimerArmForDrift, dstDriftHostClock
// do), since now + maxWhen wraps.
//
// Rounding UP is the drift rounding contract (docs/dst/faults.md "Clock faults"): both
// callers convert host-perceived spans to the base spans timers wait out, and the wall
// read-back accumulates drift rounding DOWN, so ceil here composes to a host-perceived
// elapsed ≥ the requested duration — floor(ceil(d/r)·r) ≥ d — where a floor would let
// Sleep(d) observably return early in the host's own clock at non-dividing rates (the
// Soundness invariant's "timer before its deadline" false positive).
func dstMulDivClampCeil(x, num, den int64) int64 {
	if x <= 0 {
		return 0
	}
	q := x / den
	r := x % den
	if q > maxWhen/num {
		return maxWhen // q·num alone would overflow
	}
	hi := q * num
	lo := r * num / den // < num, since r < den
	if r*num%den != 0 {
		lo++ // ceil; lo ≤ num still, so the sum check below stays sufficient
	}
	if hi > maxWhen-lo {
		return maxWhen // hi + lo would overflow
	}
	return hi + lo
}

// dstMulDivClampFloor is dstMulDivClampCeil rounding DOWN: ⌊x·num/den⌋,
// clamped the same way. The overdue-timer re-anchor uses it so the anchor is
// never EARLIER than the true boundary (a floored remainder puts the anchor
// at or after the boundary, keeping every re-armed fire never-early — the
// same direction the forward remap's ceil protects).
func dstMulDivClampFloor(x, num, den int64) int64 {
	if x <= 0 {
		return 0
	}
	q := x / den
	r := x % den
	if q > maxWhen/num {
		return maxWhen // q·num alone would overflow
	}
	hi := q * num
	lo := r * num / den // < num, since r < den
	if hi > maxWhen-lo {
		return maxWhen // hi + lo would overflow
	}
	return hi + lo
}

// dstDriftRemapFloor is dstDriftRemap rounding down (see dstMulDivClampFloor
// for why the overdue re-anchor floors). Linknamed for testing.
//
//go:linkname dstDriftRemapFloor
func dstDriftRemapFloor(x, ppbOld, ppbNew int64) int64 {
	if x <= 0 || ppbOld == ppbNew {
		return x
	}
	return dstMulDivClampFloor(x, dstDriftPPBBase+ppbOld, dstDriftPPBBase+ppbNew)
}

// dstDriftToBase converts a host-perceived duration d (ns) to the base-time duration it
// occupies on a clock with rate departure ppb: ⌈d·1e9/(1e9+ppb)⌉ = ⌈d/rate⌉ (ceil — the
// rounding contract above). The divisor 1e9 + ppb is > 0 for ppb > -1e9. Linknamed for
// direct testing.
//
//go:linkname dstDriftToBase
func dstDriftToBase(d, ppb int64) int64 {
	if ppb == 0 || d <= 0 {
		return d
	}
	return dstMulDivClampCeil(d, dstDriftPPBBase, dstDriftPPBBase+ppb)
}

// dstDriftRemap converts a base-time duration that was measured under rate 1+ppbOld/1e9
// to the base-time duration the SAME host-perceived span occupies under rate 1+ppbNew/1e9:
// ⌈x·(1e9+ppbOld)/(1e9+ppbNew)⌉ (ceil — never early in host-perceived time; at most 1 ns
// late per re-map). DriftClock uses it to re-map a pending timer's remaining base time
// (and its period) when a host's rate changes mid-run. Linknamed for testing.
//
//go:linkname dstDriftRemap
func dstDriftRemap(x, ppbOld, ppbNew int64) int64 {
	if x <= 0 || ppbOld == ppbNew {
		return x
	}
	return dstMulDivClampCeil(x, dstDriftPPBBase+ppbOld, dstDriftPPBBase+ppbNew)
}

// dstSatAdd returns a + b saturated to the int64 range instead of wrapping — the
// wall-clock composition helper: an extreme skew/step offset (±centuries) plus the base
// reading, or plus accumulated drift, must degrade to the farthest representable time,
// never to a wrapped sign flip (a wall clock that jumps sign is a state no mis-set real
// clock produces).
func dstSatAdd(a, b int64) int64 {
	s := a + b
	if b > 0 && s < a {
		return int64(1<<63 - 1)
	}
	if b < 0 && s > a {
		return -1 << 63
	}
	return s
}

// dstHostWallAdjust returns the nanoseconds host's wall clock reads ahead of base time
// at base time `base`: the skew/step offset plus the accumulated drift. Host 0 and a nil
// table read 0 (in sync). A lock-free atomic load of the fixed table; on the time.Now
// wall-split path under an active simulation.
func dstHostWallAdjust(host uint32, base int64) int64 {
	if host == 0 || host >= dstMaxSimHosts {
		return 0
	}
	t := dstHostClock.Load()
	if t == nil {
		return 0
	}
	e := &t.ent[host]
	adj := e.offset.Load()
	if ppb := e.driftPPB.Load(); ppb != 0 {
		adj = dstSatAdd(adj, dstDriftAccum(base-e.driftT0.Load(), ppb))
	}
	return adj
}

// dstHostDriftPPB returns host's clock-rate departure in parts-per-billion (0 = rate 1),
// read by the timer-arm conversion (dstTimerArmForDrift). Host 0 / nil table is rate 1.
func dstHostDriftPPB(host uint32) int64 {
	if host == 0 || host >= dstMaxSimHosts {
		return 0
	}
	if t := dstHostClock.Load(); t != nil {
		return t.ent[host].driftPPB.Load()
	}
	return 0
}

// dstStepHostClock applies an instantaneous wall-clock step of delta nanoseconds to
// host's clock (testing/simulation.StepClock) — a positive delta jumps the host's
// time.Now forward, a negative delta backward (an NTP slew/correction; a backward step
// is the HLC adversary). It ADDS to the host's current offset, so successive steps
// accumulate, and shifts only the wall reading: timer deadlines and the synctest
// advance read bubble.now directly and are untouched, so relative timers (time.After,
// context deadlines) fire at the same base time regardless of a step. It affects
// exactly host's subtree (keyed by host id) and no other host (DST-FAULT-VICTIM). A
// no-op outside a run or for a zero step. Reached via //go:linkname.
//
// It reports whether the step was applied: a step that would take the host's wall
// before the epoch is REJECTED (false, nothing applied) — settimeofday rejects a
// pre-epoch wall clock, so no real machine can hold one (the wall-representability
// boundary, docs/dst/faults.md "Clock faults"); the caller panics loud. With every
// application point validated, the composed wall is non-negative by construction:
// wall(base) = base + offset + drift-accum grows at the host's strictly positive rate.
//
//go:linkname dstStepHostClock
func dstStepHostClock(host uint32, delta int64) bool {
	gp := getg()
	if delta == 0 || gp.bubble == nil {
		return true
	}
	dstHostClockEnsure(host)
	e := &dstHostClock.Load().ent[host]
	newOff := dstSatAdd(e.offset.Load(), delta)
	adj := newOff
	if ppb := e.driftPPB.Load(); ppb != 0 {
		adj = dstSatAdd(adj, dstDriftAccum(gp.bubble.now-e.driftT0.Load(), ppb))
	}
	if dstSatAdd(gp.bubble.now, adj) < 0 {
		return false // pre-epoch wall: reject, apply nothing
	}
	e.offset.Store(newOff)
	return true
}

// dstFakeTimers is the per-run set of fake (synctest-bubble) timers that have been
// armed, the enumeration DriftClock re-maps on a mid-run rate change. A channel timer is
// in the bubble's heap only while a goroutine is blocked on its channel (needsAdd), so
// the heap is NOT a complete set of armed timers — a NewTimer/NewTicker held without a
// pending receive, or a ticker between ticks, is armed yet unheaped. This list is the
// complete set: every fake timer registers here at its first arm (modify), so the
// re-walk reaches heaped and unheaped timers alike. Entries are deduped by the run epoch
// stamped on the timer (t.dstReg), which also discards a timer object reused from a prior
// run. The list keeps its timers alive until the run ends (bounded per run, reset by
// epoch) — acceptable for the simulation path, where it removes the need to hook every
// timer disarm.
//
// It is a LOCK-FREE intrusive prepend stack (head is an atomic pointer, timers linked by
// t.dstFakeNext): the single-P Run path never contends, but the white-box dstActivate
// path runs at GOMAXPROCS>1 (its documented purpose), where two goroutines in different
// synctest bubbles on different Ms could arm fake timers concurrently and race a plain
// slice append. CAS-prepend serializes it with no lock, so it adds no lock-rank edge (the
// register path is reached from (*timer).modify BEFORE modify takes the timer's own
// lock). A given timer is armed by one goroutine, so its own t.dstReg/t.dstFakeNext are
// never concurrently written; only the shared head races, which the CAS handles.
var dstFakeTimers struct {
	epoch atomic.Uint64
	head  atomic.Pointer[timer]
}

// dstFakeTimersRoll resets the stack at the start of a new run (epoch change). The CAS
// makes the reset fire exactly once across concurrent arrivals at a new epoch.
func dstFakeTimersRoll() {
	e := dstRunEpoch.Load()
	old := dstFakeTimers.epoch.Load()
	if e != old && dstFakeTimers.epoch.CompareAndSwap(old, e) {
		dstFakeTimers.head.Store(nil)
	}
}

// dstFakeTimersReset drops the whole stack at a Run boundary (dstSetSimEnv/
// dstClearSimEnv, both single-threaded), clearing each timer's dstFakeNext so a timer
// that outlives the run does NOT transitively pin the timers registered before it in
// the chain (a reused timer overwrites its own dstFakeNext at next registration, so
// clearing here only matters for timers not re-armed).
func dstFakeTimersReset() {
	t := dstFakeTimers.head.Load()
	dstFakeTimers.head.Store(nil) // single-threaded Run boundary: no concurrent prepend
	for t != nil {
		next := t.dstFakeNext
		t.dstFakeNext = nil
		t = next
	}
}

// dstRegisterFakeTimer records a fake timer in the current run's stack on its first arm.
// Called from (*timer).modify under dstActive && t.isFake (before modify takes t's own
// lock). On the Run path, single-P cooperative scheduling serializes both the t.dstReg
// dedup and the head prepend; the only concurrency is the white-box GOMAXPROCS>1 arm
// path, where distinct timers race the shared head — a CAS-prepend serializes that
// lock-free (a given timer is armed by one goroutine, so its own dstReg/dstFakeNext are
// not concurrently written).
func dstRegisterFakeTimer(t *timer) {
	dstFakeTimersRoll()
	e := uint32(dstFakeTimers.epoch.Load())
	if t.dstReg == e {
		return
	}
	t.dstReg = e
	for {
		head := dstFakeTimers.head.Load()
		t.dstFakeNext = head
		if dstFakeTimers.head.CompareAndSwap(head, t) {
			return
		}
	}
}

// dstDriftHostClock changes host's clock rate to ppb parts-per-billion mid-run
// (testing/simulation.DriftClock) — drift over a sub-window (start, change, or re-sync a
// rate), the dynamic complement of the declared Drift. Two effects at the change instant
// T = bubble.now:
//
//  1. Re-anchor the wall so it stays continuous: fold the drift accumulated so far under
//     the old rate into offset, then reset the anchor and rate. wall(T) is unchanged; for
//     base > T the wall drifts at the new rate.
//  2. Re-map host's armed fake timers: each was armed with its base when computed under
//     the OLD rate, so the remaining host-perceived time now occupies a different base
//     span — when' = T + (when−T)·r_old/r_new, period' = period·r_old/r_new. Every armed
//     fake timer (heaped or not) is in dstFakeTimers; for those this host owns and that
//     are still pending (when > T), the when/period are re-mapped IN PLACE under the
//     timer's lock, marking a heaped timer modified so the heap re-adjusts (but never
//     clearing a zombie bit — an unblocked channel timer must stay un-runnable until its
//     receiver returns). An unheaped timer simply carries its re-mapped when until it is
//     next added to the heap.
//
// No reader observes an intermediate state: DriftClock runs on one goroutine under the
// cooperative single-P DST schedule and the re-map never blocks on a timer or yields, so
// no other goroutine (and no clock read or timer fire) interleaves. A no-op outside a run
// or for no change. Reached via //go:linkname.
//
//go:linkname dstDriftHostClock
func dstDriftHostClock(host uint32, ppb int64) {
	b := getg().bubble
	if b == nil {
		return
	}
	if ppb <= -dstDriftPPBBase {
		ppb = -dstDriftPPBBase + 1
	} else if ppb > dstMaxDriftPPB {
		ppb = dstMaxDriftPPB
	}
	dstHostClockEnsure(host)
	e := &dstHostClock.Load().ent[host]
	ppbOld := e.driftPPB.Load()
	if ppbOld == ppb {
		return
	}
	now := b.now
	// (1) Re-anchor: fold drift-so-far into offset, reset anchor + rate (wall continuous).
	// Saturating: this fold is a wall-application point like any other, and a plain Add
	// can wrap an extreme accepted offset (Skew near MaxInt64 + accumulated drift),
	// resurrecting the negative wall the representability boundary forbids. Saturation
	// keeps the wall pinned at the far end instead — the recorded stance.
	e.offset.Store(dstSatAdd(e.offset.Load(), dstDriftAccum(now-e.driftT0.Load(), ppbOld)))
	e.driftT0.Store(now)
	e.driftPPB.Store(ppb)

	// (2) Re-map this host's armed fake timers in place.
	dstRemapHostTimers(host, now, ppbOld, ppb)
}

// dstRemapHostTimers re-maps every armed fake timer of host from rate ppbOld to rate
// ppbNew at change instant now: each timer's base when was computed under the old rate,
// so the remaining host-perceived time occupies a different base span under the new —
// when' = now + (when−now)·r_old/r_new, period' = period·r_old/r_new. Every armed fake
// timer (heaped or not) is in dstFakeTimers; for those this host owns, the period is
// re-mapped IN PLACE under the timer's lock, and the when additionally for those still
// pending (when > now), marking a heaped timer modified so the heap re-adjusts (but
// never clearing a zombie bit — an unblocked channel timer must stay un-runnable until
// its receiver returns).
// An unheaped timer simply carries its re-mapped when until it is next added to the heap.
func dstRemapHostTimers(host uint32, now, ppbOld, ppbNew int64) {
	if ppbOld == ppbNew {
		return // identity re-map
	}
	dstFakeTimersRoll()
	epoch := uint32(dstFakeTimers.epoch.Load())
	// DriftClock runs only on the single-P Run path, so the stack is not being
	// prepended concurrently here; walk it directly from the atomic head. (The
	// lock-free head exists for the white-box GOMAXPROCS>1 arm path, which never
	// calls DriftClock.) Each timer is re-mapped under its own lock.
	for t := dstFakeTimers.head.Load(); t != nil; t = t.dstFakeNext {
		t.lock()
		if t.dstReg == epoch && t.dstHost == host {
			// The period is re-mapped for EVERY owned timer, not only pending
			// ones: a periodic timer due exactly at the change instant
			// (when == now, unfired — channel timers fire lazily on access)
			// or overdue has its period reused by the next re-arm, so an
			// unconverted period would keep it firing at the old rate for the
			// rest of the run. An exactly-due when needs no move — firing
			// "now" is correct under any rate.
			oldPeriod := t.period
			t.period = dstDriftRemap(t.period, ppbOld, ppbNew)
			if t.when > now {
				rem := dstDriftRemap(t.when-now, ppbOld, ppbNew)
				when := int64(maxWhen)
				if rem <= maxWhen-now { // avoid now+rem overflowing int64
					when = now + rem
				}
				t.when = when
				if t.state&timerHeaped != 0 {
					// Mark the heap entry stale so timers.adjust re-positions it at the next
					// check; preserve timerZombie (do not resurrect an unblocked channel
					// timer). Mirrors (*timer).modify's heap-marking minus the zombie clear.
					t.state |= timerModified
					if min := t.ts.minWhenModified.Load(); min == 0 || when < min {
						t.astate.Store(t.state)
						t.ts.updateMinWhenModified(when)
					}
				}
			} else if t.when > 0 && t.when < now {
				// OVERDUE — reachable for a never-heaped channel timer, which
				// fires lazily on the next receive: the remap formula's
				// negative remainder. RE-ANCHOR at the last host-period
				// boundary before the change: the periodic remainder is
				// computed in the OLD regime ((now−when) mod the pre-remap
				// period, where host scaling cancels, so the boundary index
				// is exact) and floor-remapped into the new (a floored
				// remainder can only move the anchor LATER than the true
				// boundary, keeping every re-armed fire never-early; late by
				// under a nanosecond per rounding step, the forward remap's
				// contract mirrored). The re-arm catch-up
				// (next = when + period·(1+delay/period), time.go) then
				// counts whole new-regime periods from a boundary-aligned
				// anchor. Anchoring on the raw overdue span instead
				// double-rounds — ceil'd span vs ceil'd period — and an
				// exact-multiple overdue span undercounts the catch-up index,
				// duplicating a boundary's tick almost a full period early in
				// host time. A one-shot (period 0) keeps the full span: only
				// "still overdue" matters, it never re-arms.
				//
				// The when == 0 "not running" sentinel is skipped (converting
				// it would resurrect a stopped timer), and the result is
				// clamped above it: an extreme slowdown can remap the
				// remainder past the whole base epoch, and a when of exactly
				// 0 reads as that sentinel on the lazy-fire path, wedging the
				// receive into a manufactured bubble deadlock. The clamp
				// costs a one-shot phase error anchored at base 1, in a
				// regime where the host clock has effectively stopped. An
				// overdue timer is never heaped under bubble time (the heap
				// runs due timers as time advances), so no heap re-marking is
				// needed.
				if oldPeriod > 0 {
					rem := (now - t.when) % oldPeriod
					rem = dstDriftRemapFloor(rem, ppbOld, ppbNew)
					when := now - rem
					if when < 1 {
						when = 1
					}
					// The delivered timestamp must keep the ORIGINAL due
					// time (sendTime derives it from the fire-time delay,
					// now − when): the re-anchor moves when TOWARD now (the
					// last pre-change boundary is later than the original
					// due instant), shrinking that delay, so the move
					// (new − old, positive) is recorded and added back to
					// the delay at fire (unlockAndRun); only the re-arm
					// catch-up sees the converted anchor. Accumulates across
					// successive rate changes before the fire.
					t.dstWhenShift += when - t.when
					t.when = when
				}
				// A one-shot (period 0) is left untouched: it never re-arms,
				// only "still overdue" matters for its lazy fire, and an
				// unconverted when keeps its delivered timestamp exact.
			}
		}
		t.unlock()
	}
}

// dstReestablishHostClock re-establishes host's clock to the declared configuration —
// testing/simulation.Host's re-declaration (restart) semantics (docs/dst/faults.md
// "Clock faults", Host re-declaration): armed timers re-map from the surviving rate to
// the declared one, the offset is OVERWRITTEN to the declared value (discarding prior
// steps and accumulated drift — a restarted host's clock is the declared clock, not a
// continuation), and the anchor resets to now so no stale accumulation survives. The
// unconditional anchor reset is what a fold-then-overwrite composition misses: on a
// same-rate re-declare the fold's Add is discarded by the overwrite but a stale anchor
// would keep pre-re-declare drift in every subsequent wall read. A no-op outside a run.
// Reached via //go:linkname.
//
// It reports whether the clock was applied: a declared skew that would take the
// host's wall before the epoch is REJECTED (false, nothing applied — not even the
// re-map), per the wall-representability boundary (see dstStepHostClock); the caller
// panics loud.
//
//go:linkname dstReestablishHostClock
func dstReestablishHostClock(host uint32, offset, ppb int64) bool {
	b := getg().bubble
	if b == nil {
		return true
	}
	if ppb <= -dstDriftPPBBase {
		ppb = -dstDriftPPBBase + 1 // rate stays > 0
	} else if ppb > dstMaxDriftPPB {
		ppb = dstMaxDriftPPB
	}
	now := b.now
	if dstSatAdd(now, offset) < 0 {
		return false // pre-epoch wall: reject, apply nothing
	}
	dstHostClockEnsure(host)
	e := &dstHostClock.Load().ent[host]
	dstRemapHostTimers(host, now, e.driftPPB.Load(), ppb)
	e.offset.Store(offset)
	e.driftT0.Store(now)
	e.driftPPB.Store(ppb)
	return true
}

// dstClockOffsetNow returns the calling goroutine's host wall adjustment now (the
// nanoseconds time.Now reads ahead of the universe base clock on this host, skew plus
// accumulated drift). The simulated network reads it to convert a host-skewed wall
// reading into universe BASE time: link latency is a base-time duration, so delivery
// must be gated in base time consistently across hosts that disagree on wall time. It
// reads the host's CURRENT adjustment, so a step's wall jump (and a drifting host's
// accumulated wall) cancel against the matching reading — base = wall − adjustment stays
// base-invariant, as it must (a clock fault moves wall, not base time). Zero outside a
// bubble. Reached via //go:linkname.
//
//go:linkname dstClockOffsetNow
func dstClockOffsetNow() int64 {
	gp := getg()
	b := gp.bubble
	if b == nil {
		return 0
	}
	return dstHostWallAdjust(gp.dstHost, b.now)
}

// dstTimerArmForDrift converts a timer's absolute fire time `when` and repeat `period`
// from the arming host's perceived time to universe base time, for a host whose clock
// drifts (rate ≠ 1). A relative arm reaches the runtime as when = bubble.now + d and
// period = the interval, both in the host's perceived ns; on a rate-r host that duration
// occupies d/r of base time, so the timer must fire at when = now + (when−now)/r and
// repeat every period/r of base. Identity for rate 1 (driftPPB 0) and outside a bubble,
// so non-drifting timers and non-dst builds are unaffected. Called at the single timer
// choke point (*timer).modify, through which every Sleep / After / NewTimer / NewTicker /
// AfterFunc and context deadline funnels (the periodic re-arm reuses the converted
// period, so ticks stay rate-correct without re-entering modify).
func dstTimerArmForDrift(when, period int64) (int64, int64) {
	gp := getg()
	b := gp.bubble
	if b == nil {
		return when, period
	}
	ppb := dstHostDriftPPB(gp.dstHost)
	if ppb == 0 {
		return when, period
	}
	if now := b.now; when > now {
		// Clamp the addition, not just the conversion: dstDriftToBase clamps its
		// product to maxWhen, and now + maxWhen wraps negative (bubble base time is
		// ~9.47e17 ns). A wrapped when fails needsAdd, so the timer would silently
		// never be heaped and never fire — Sleep(math.MaxInt64) (clamped to maxWhen
		// by timeSleep) on any slow-drifting host would park its goroutine forever
		// and the run would report a deadlock neither real hardware nor the
		// un-drifted simulation exhibits. Mirrors dstDriftHostClock's re-map clamp.
		conv := dstDriftToBase(when-now, ppb)
		when = int64(maxWhen)
		if conv <= maxWhen-now {
			when = now + conv
		}
	}
	if period > 0 {
		period = dstDriftToBase(period, ppb)
	}
	return when, period
}

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

// dstHostSeededDriftPPB returns a deterministic per-host clock-rate departure in
// [-maxPPB, +maxPPB] parts-per-billion, a stateless function of the run seed and the
// host id (testing/simulation.BoundedDrift) — the drift analogue of
// dstHostSeededClockOffset. It uses an INDEPENDENT salt, so a host's seeded rate and
// its seeded skew (BoundedSkew) are drawn independently; and it advances no RNG stream
// — neither a per-g tree nor the scheduling RNG — so seeding a rate can neither perturb
// the interleaving nor shift any other draw, and the rate is stable across a host
// re-declaration (restart) because it depends only on (seed, host id). Sweeping the run
// seed sweeps the whole bounded rate-assignment space. The caller (BoundedDrift) bounds
// maxPPB to [0, dstDriftPPBBase) so -maxPPB > -dstDriftPPBBase and every drawn rate
// stays positive. A non-positive maxPPB is no drift. Reached via //go:linkname.
//
//go:linkname dstHostSeededDriftPPB
func dstHostSeededDriftPPB(hostid uint32, maxPPB int64) int64 {
	if maxPPB <= 0 {
		return 0
	}
	// splitmix64 finalizer over (seed, drift-salt, host id): stateless, advances nothing.
	// The salt differs from dstHostSeededClockOffset's so rate and skew draw independently.
	x := dstSeed.Load() ^ 0xD817D817D817D817 ^ (uint64(hostid) * 0x9e3779b97f4a7c15)
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	x ^= x >> 31
	// Map uniformly into [-maxPPB, maxPPB]: compute the span and centering in uint64 so
	// neither 2*maxPPB nor the subtraction overflows (maxPPB < 1e9, so the result lands
	// in range for r < maxPPB the uint64 subtraction wraps to the correct negative).
	span := 2*uint64(maxPPB) + 1
	return int64(x%span - uint64(maxPPB))
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

// dstPooledAlloc is the count of bytes of runtime-internal pooled structs
// allocated since activation on behalf of simulation-bubble goroutines:
// sudog/_defer by allocation type (user stack, acquireSudog/newdefer), g by
// the allgadd publication (systemstack, so only allgs-pinned g's enter — an
// allocm-created g0/gsignal also carries a bubble m.curg but can die via
// sched.freem, and counting one would break counted-implies-live; it stays
// in the recorded residual band). These bytes are excluded from the heap
// trigger counter (dstHeapAlloc, gc.md M4) because whether one is allocated
// or reused from its cross-run pool is pre-run process history — but the
// counted ones stay LIVE at every in-run mark (allgs pins its g's; sudog/
// _defer sit referenced in their caches, and clearpools leaves the central
// caches alone while a run is active), so they inflate heapMarked and,
// uncorrected, the GOGC-scaled target: a cold run that allocates 1500 g's
// carries a ~900KB larger target than a warmed rerun that reuses them,
// shifting the last cycle's boundary and its discovery tail. The trigger's
// target computation subtracts the snapshot below, so the nondeterministic
// term cancels exactly.
var dstPooledAlloc atomic.Uint64

// dstPooledGBytes is the size-class-rounded size of the g struct — the exact
// bytes a fresh g contributes to heapMarked while sizeof(g) stays at or
// under MinSizeForMallocHeader (512): past it, roundupsize returns the user
// size (it subtracts the malloc header back out) while heapMarked counts
// full elemsize, an 8-byte-per-g undercount. Not silent: the mismatch times
// the goroutine phase's fresh-g count splits TestDSTGCSysstackAlloc's
// asserted totals. Set at activation; added to dstPooledAlloc per allgadd
// of a bubble-attributed g.
var dstPooledGBytes uint64

// dstPooledMarked is dstPooledAlloc snapshotted at each mark termination
// (resetLive), i.e. the pooled bytes contained in the current heapMarked.
// The GOGC target and memory-limit crossings subtract it from
// heapMarked - dstHeapBase so both remain functions of the bubble's own SUT
// live set. Snapshotting at the same STW point that sets heapMarked keeps
// the two consistent.
var dstPooledMarked atomic.Uint64

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

// dstSchedPrevSys records whether the previous scheduling decision picked an
// infrastructure candidate. dstFindRunnable's starvation fairness reads it: an
// infrastructure pick with a simulation candidate runnable hands the NEXT
// decision to the simulation, so a persistently-runnable foreign goroutine
// cannot starve the bubble. Pure pacing state — it decides when a simulation
// decision happens, never which goroutine it picks — so it does not need to
// replay: the simulation's decision sequence is identical however
// infrastructure picks interleave. That holds only because the sim bubble's
// drain is exempt from the alternation (dstFindRunnable's sysDrain): the
// drain is the one infra-classified goroutine with sim-visible effects, so
// pacing it would change which enabled sets the simulation's decisions see.
// Reset at activation.
var dstSchedPrevSys bool

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

// dstSimMainPrioFP returns the active simulation bubble's main goroutine PCT priority
// (0 if none / not PCT). A test asserts it is nonzero under PCT — bubble.main is created
// before the bubble is claimed, so it must still be assigned a priority. Via //go:linkname.
//
//go:linkname dstSimMainPrioFP
func dstSimMainPrioFP() int64 {
	if dstSimBubble != nil && dstSimBubble.main != nil {
		return dstSimBubble.main.dstPrio
	}
	return 0
}

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

// dstPidLive is the simulated pid liveness table consulted by syscall.Kill(pid, 0).
// The root pid is live for the run; Process pids are live for the dynamic extent of
// their body. Pids are allocated monotonically and never reused within a run, so a
// completed process's old pid cannot become live again by accident. The map is
// copy-on-write: readers get an immutable snapshot and writers publish a new table.
type dstPidLiveTable struct {
	live map[int32]bool
}

var dstPidLive atomic.Pointer[dstPidLiveTable]

const dstMaxSimPID = 1<<31 - 1

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
	dstPidLiveReset(int32(pid))
	dstProcAlloc.Store(nil)
	dstHostClock.Store(nil)
	dstFakeTimersReset()
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
	dstPidLiveReset(0)
	dstProcAlloc.Store(nil)
	dstHostClock.Store(nil)
	dstFakeTimersReset()
}

// dstPidLivePublishRace and dstPidLiveLoadRace bracket every publish/load of
// dstPidLive for the race detector: the table pointer moves through
// runtime-internal atomics, which carry no TSan happens-before annotation,
// while the Go-map operations INSIDE the table do fire the map runtime's race
// hooks — so without an explicit release edge at each publish, matched by an
// acquire at each load, TSan reports every cross-goroutine publish/read pair
// as a race the CPU-level atomics have already ordered.
func dstPidLivePublishRace() {
	if raceenabled {
		racereleasemerge(unsafe.Pointer(&dstPidLive))
	}
}

func dstPidLiveLoadRace() {
	if raceenabled {
		raceacquire(unsafe.Pointer(&dstPidLive))
	}
}

func dstPidLiveReset(root int32) {
	dstPidLivePublishRace()
	if root > 0 {
		dstPidLive.Store(&dstPidLiveTable{live: map[int32]bool{root: true}})
	} else {
		dstPidLive.Store(nil)
	}
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
	for {
		old := dstSimPidNext.Load()
		if old == dstMaxSimPID {
			panic("testing/simulation: simulated pid allocation overflows OS pid field")
		}
		if dstSimPidNext.CompareAndSwap(old, old+1) {
			return old + 1
		}
	}
}

// dstSetPidLive marks a simulated pid live or dead for Kill(pid, 0). Reached from
// testing/simulation.Process by //go:linkname.
//
//go:linkname dstSetPidLive
func dstSetPidLive(pid int32, live bool) {
	if pid <= 0 {
		return
	}
	for {
		old := dstPidLive.Load()
		if old == nil {
			return
		}
		dstPidLiveLoadRace()
		if old.live[pid] == live {
			return
		}
		next := make(map[int32]bool, len(old.live)+1)
		for p := range old.live {
			next[p] = true
		}
		if live {
			next[pid] = true
		} else {
			delete(next, pid)
		}
		dstPidLivePublishRace()
		if dstPidLive.CompareAndSwap(old, &dstPidLiveTable{live: next}) {
			return
		}
	}
}

// dstPidAlive reports whether pid is live in the current simulated pid registry.
// Reached from syscall.Kill's DST hook by //go:linkname.
//
//go:linkname dstPidAlive
func dstPidAlive(pid int32) bool {
	if pid <= 0 || !dstSimEnvSet {
		return false
	}
	t := dstPidLive.Load()
	if t == nil {
		return false
	}
	dstPidLiveLoadRace()
	return t.live[pid]
}

// dstPidStarttime reports the deterministic procfs starttime for a live simulated
// pid. The value is derived from the pid itself because pids are monotonic and never
// reused within a run; completed pids have no procfs entry. Reached from os's
// synthetic /proc support by //go:linkname.
//
//go:linkname dstPidStarttime
func dstPidStarttime(pid int32) (start uint64, ok bool) {
	if !dstPidAlive(pid) {
		return 0, false
	}
	return uint64(pid), true
}

// dstCrashProcessPid marks one process invocation dead and removes its goroutine
// subtree from synctest liveness accounting. The logical process id (dstProc) is
// stable across restarts, so crash scheduling keys by the per-invocation pid: a
// restarted process gets a fresh pid and must not be suppressed with the old one.
// Reached from testing/simulation's process-teardown path by //go:linkname.
//
//go:linkname dstCrashProcessPid
func dstCrashProcessPid(pid int32) {
	if pid <= 0 {
		return
	}
	dstSetPidLive(pid, false)
	dstMarkProcessGoroutinesCrashed(pid)
}

// dstSelfCrashed reports whether the CALLING goroutine belongs to a crashed
// process invocation (the crash mark flipped its pid negative). A self-crash
// — the OOM shape: the victim's own goroutine triggers the fault — leaves the
// caller running past the mark; it must park forever instead of returning
// (dstParkCrashedSelf). Reached from testing/simulation.Crash by //go:linkname.
//
//go:linkname dstSelfCrashed
func dstSelfCrashed() bool {
	return getg().dstPid < 0
}

// dstParkCrashedSelf parks the calling goroutine forever: it belongs to a
// crashed (or exited) simulated process invocation, whose threads never run
// again — and never unwind (a killed process runs no defers; the caller
// forfeits its own by parking here). The scheduler never selects a crashed
// goroutine (dstFindRunnable drops them; the chan and sema dequeues skip
// them), so the park is permanent by construction.
//
// The caller may or may not already carry the crash mark: a self-crash was
// marked by dstMarkProcessGoroutinesCrashed (which also settled its bubble
// accounting), while a goroutine that ESCAPED the mark inside a nested
// Process body still carries a positive pid. Mark and de-count the latter
// here, mirroring the mark path exactly — otherwise the bubble counts a
// goroutine that can never run again as running, and the run never completes.
// Reached from testing/simulation by //go:linkname.
//
//go:linkname dstParkCrashedSelf
func dstParkCrashedSelf() {
	gp := getg()
	if gp.dstPid > 0 {
		gp.dstPid = -gp.dstPid
		dstUncountCrashedRunningG(gp)
	}
	gopark(nil, nil, waitReasonDSTProcessCrashed, traceBlockForever, 1)
	throw("dst: a crashed process's goroutine resumed")
}

// dstUncountCrashedRunningG removes a just-marked, currently-RUNNING goroutine
// from its bubble's liveness accounting — the mark-then-uncount sequence
// dstMarkProcessGoroutinesCrashed performs in bulk, here for a single g. The
// mark is a PRECONDITION, asserted rather than assumed: an unmarked goroutine
// is one changegstatus still counts and the scheduler and wait-queue filters
// (dstFindRunnable, the chan and sema dequeues) still treat as live, so
// uncounting it silently would desynchronize the bubble's ledger from the set
// of goroutines that can actually run.
func dstUncountCrashedRunningG(gp *g) {
	if gp.dstPid >= 0 {
		throw("dst: uncounting a goroutine that carries no crash mark")
	}
	bubble := gp.bubble
	if bubble == nil {
		return
	}
	// The caller is running, so it counted in both totals.
	lock(&bubble.mu)
	bubble.total--
	bubble.running--
	if bubble.total < 0 {
		fatal("total < 0")
	}
	if bubble.running < 0 {
		fatal("running < 0")
	}
	wake := bubble.maybeWakeLocked()
	unlock(&bubble.mu)
	if wake != nil {
		goready(wake, 0)
	}
}

func dstSynctestRunningStatus(gp *g, status uint32) bool {
	if status == _Gdead || status == _Gdeadextra {
		return false
	}
	if status == _Gwaiting && gp.waitreason.isIdleInSynctest() {
		return false
	}
	return true
}

// dstCrashKillsBubbleMain reports whether pid's goroutine set includes the
// bubble's main goroutine — the one running the simulation body. Killing it
// leaves the universe with no driver: the body's remaining statements (a
// test's assertions among them) never run and the bubble never completes, so
// the crash must be refused BEFORE any goroutine is marked, not diagnosed as a
// hang afterwards. It happens when a Process body runs directly on the run's
// own goroutine (`Run(f)` calling `Process(name, …)` inline) and that process
// is crashed; declaring such a process on a child goroutine
// (`go Process(name, …)`) models a crashable process faithfully.
func dstCrashKillsBubbleMain(bubble *synctestBubble, pid int32) bool {
	main := bubble.main
	return main != nil && (main.dstPid == pid || main.dstPid == -pid)
}

// dstPidOwnsBubbleMain reports whether pid's goroutine set contains the run's
// main goroutine. Callers pre-scan every victim with it and refuse BEFORE any
// teardown, so a multi-victim fault (a host crash) cannot tear part of the
// universe down and only then panic. The mark path keeps its own check as a
// backstop. Reached from testing/simulation by //go:linkname.
//
//go:linkname dstPidOwnsBubbleMain
func dstPidOwnsBubbleMain(pid int32) bool {
	bubble := dstSimBubble
	return bubble != nil && dstCrashKillsBubbleMain(bubble, pid)
}

// dstHostOwnsBubbleMain reports whether the run's main goroutine runs on host.
// Crashing that host would destroy the machine the simulation driver runs on —
// its filesystem, locks, and sockets would go while the driver kept running, a
// state no power loss produces. Reached from testing/simulation by //go:linkname.
//
//go:linkname dstHostOwnsBubbleMain
func dstHostOwnsBubbleMain(host uint32) bool {
	bubble := dstSimBubble
	return bubble != nil && bubble.main != nil && bubble.main.dstHost == host
}

// dstMarkHostGoroutinesCrashed deschedules permanently every goroutine running
// on host — the machine lost power, so every thread on it stops, whichever
// process it belonged to. That includes the ROOT process's goroutines running a
// Host body: they are threads on the dead machine, even though the root process
// itself lives on (its pid stays live; it has goroutines on other hosts). The
// pid-keyed kill cannot express this — a host is not a process — so the host
// crash marks by host. Reached from testing/simulation.CrashHost by //go:linkname.
//
//go:linkname dstMarkHostGoroutinesCrashed
func dstMarkHostGoroutinesCrashed(host uint32) {
	bubble := dstSimBubble
	if bubble == nil {
		return
	}
	if bubble.main != nil && bubble.main.dstHost == host {
		panic("testing/simulation: CrashHost would kill the run's main goroutine")
	}
	var total, running int
	var waiterCrashed bool
	forEachG(func(gp *g) {
		if gp.bubble != bubble || gp.dstHost != host || gp.dstPid < 0 {
			return
		}
		status := readgstatus(gp) &^ _Gscan
		gp.dstPid = -gp.dstPid
		if gp.dstPid == 0 {
			// A goroutine that never carried a pid still must never run again;
			// the scheduler filter keys on dstPid < 0, so give it the sentinel
			// the root pid can never take (pids are positive).
			gp.dstPid = -1
		}
		if gp == bubble.waiter {
			waiterCrashed = true
		}
		if status == _Gdead || status == _Gdeadextra {
			return
		}
		total++
		if dstSynctestRunningStatus(gp, status) {
			running++
		}
	})
	if total == 0 && !waiterCrashed {
		return
	}
	lock(&bubble.mu)
	if waiterCrashed {
		bubble.waiter = nil // see dstMarkProcessGoroutinesCrashed
	}
	bubble.total -= total
	bubble.running -= running
	if bubble.total < 0 {
		fatal("total < 0")
	}
	if bubble.running < 0 {
		fatal("running < 0")
	}
	wake := bubble.maybeWakeLocked()
	unlock(&bubble.mu)
	if wake != nil {
		goready(wake, 0)
	}
}

func dstMarkProcessGoroutinesCrashed(pid int32) {
	bubble := dstSimBubble
	if bubble == nil {
		return
	}
	if dstCrashKillsBubbleMain(bubble, pid) {
		panic("testing/simulation: Crash would kill the run's main goroutine — declare a crashable process on its own goroutine (go Process(name, f))")
	}
	var total, running int
	var waiterCrashed bool
	forEachG(func(gp *g) {
		if gp.bubble != bubble || gp.dstPid != pid {
			return
		}
		status := readgstatus(gp) &^ _Gscan
		gp.dstPid = -pid
		if gp == bubble.waiter {
			waiterCrashed = true
		}
		if status == _Gdead || status == _Gdeadextra {
			return
		}
		total++
		if dstSynctestRunningStatus(gp, status) {
			running++
		}
	})
	if total == 0 && !waiterCrashed {
		return
	}
	lock(&bubble.mu)
	if waiterCrashed {
		// The victim was blocked in synctest.Wait. maybeWakeLocked would hand
		// the wake to it and bump bubble.active, but the scheduler drops
		// crashed goroutines (dstFindRunnable), so nothing would ever decrement
		// active again and the bubble could never idle. A crashed waiter is not
		// waiting for anything: clear it, and let the wake fall to the root.
		bubble.waiter = nil
	}
	bubble.total -= total
	bubble.running -= running
	if bubble.total < 0 {
		fatal("total < 0")
	}
	if bubble.running < 0 {
		fatal("running < 0")
	}
	wake := bubble.maybeWakeLocked()
	unlock(&bubble.mu)
	if wake != nil {
		goready(wake, 0)
	}
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

// dstSimEnvProc returns the calling goroutine's simulated process id (g.dstProc)
// and whether a run's environment is active (dstSimEnvSet). The syscall package
// pulls it to key its per-process copy-on-write environment view: under a run,
// os/syscall.Getenv/Setenv/… operate on the calling process's isolated copy of
// the host environment instead of the shared host env (see design.md
// "Environment surface"). Like identity, env gates on dstSimEnvSet (process-
// global while set), not per-goroutine — a non-bubble goroutine reading env
// during a run sees the root process (dstProc 0) view — and the run epoch
// (dstNetEpoch) drives the per-run reset of those copies.
//
//go:linkname dstSimEnvProc
func dstSimEnvProc() (proc uint32, ok bool) { return getg().dstProc, dstSimEnvSet }

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

// dstNetCrossHostBandwidth is the per-run bandwidth limit in bytes per second for
// each cross-host connection direction: the link transmits one segment after
// another at this rate (a segment of S bytes occupies the link S/bandwidth before
// it propagates), so a receiver gets bytes no faster than the rate. 0 (the
// default, and same-host always) means unlimited. Set once before the bubble and
// reset after, like the latency/jitter globals.
var dstNetCrossHostBandwidth int64

// dstSetNetCrossHostBandwidth sets the per-run cross-host bandwidth limit. Reached
// via //go:linkname from testing/simulation's run envelope.
//
//go:linkname dstSetNetCrossHostBandwidth
func dstSetNetCrossHostBandwidth(bytesPerSec int64) { dstNetCrossHostBandwidth = bytesPerSec }

// dstNetCrossHostBandwidthBps reports the per-run cross-host bandwidth limit.
// Reached via //go:linkname from net.
//
//go:linkname dstNetCrossHostBandwidthBps
func dstNetCrossHostBandwidthBps() int64 { return dstNetCrossHostBandwidth }

// dstNetSendBuffer is the per-run per-direction send-buffer capacity in bytes: a Write
// blocks once this many written-but-undelivered bytes are outstanding (backpressure).
// 0 means unbounded (writes never block; SendBuffer<0). dstNetRetransmitNs is the
// virtual-time horizon after which an undeliverable write/dial fails ETIMEDOUT; 0 means
// no horizon. Both resolved (defaults applied) by testing/simulation before the bubble
// and reset after, like the latency/jitter/bandwidth globals.
var (
	dstNetSendBuffer   int64
	dstNetRetransmitNs int64
)

//go:linkname dstSetNetSendBuffer
func dstSetNetSendBuffer(bytes int64) { dstNetSendBuffer = bytes }

//go:linkname dstNetSendBufferBytes
func dstNetSendBufferBytes() int64 { return dstNetSendBuffer }

//go:linkname dstSetNetRetransmitTimeout
func dstSetNetRetransmitTimeout(ns int64) { dstNetRetransmitNs = ns }

//go:linkname dstNetRetransmitTimeoutNs
func dstNetRetransmitTimeoutNs() int64 { return dstNetRetransmitNs }

// dstNetPartitionHook is net's handler for partition targeting (Partition / Heal /
// Isolate / HealHost), registered by net at init when built with -tags dst. The
// partition state and its blocked-reader wakeups live in net (next to the conns
// and with sync/chan available); runtime is only the always-linked rendezvous, so
// testing/simulation can drive partitioning without a fragile simulation->net
// linkname (net is not in a simulation binary unless the SUT uses it). The op
// codes are net's contract (see net/dst_partition.go), mirrored by the caller in
// testing/simulation; b is ignored for the host-level ops. A nil hook (net not
// linked) makes the call a no-op — a partition with no network to cut.
var dstNetPartitionHook func(op, a, b uint32)

// dstSetNetPartitionHook registers net's partition handler. Reached via
// //go:linkname from net's init.
//
//go:linkname dstSetNetPartitionHook
func dstSetNetPartitionHook(fn func(op, a, b uint32)) { dstNetPartitionHook = fn }

// dstNetPartitionOp invokes net's partition handler (no-op if net is not linked).
// Reached via //go:linkname from testing/simulation's targeting API.
//
//go:linkname dstNetPartitionOp
func dstNetPartitionOp(op, a, b uint32) {
	if dstNetPartitionHook != nil {
		dstNetPartitionHook(op, a, b)
	}
}

// dstNetHostDeadHook is net's dead-host query: whether host is powered off
// (CrashHost'd, not yet rebooted by a Host re-declaration). Nil when net is
// not linked — then no dial exists to observe the mark, and the query reports
// alive.
var dstNetHostDeadHook func(host uint32) bool

// dstSetNetHostDeadHook registers net's dead-host query. Reached via
// //go:linkname from net's init.
//
//go:linkname dstSetNetHostDeadHook
func dstSetNetHostDeadHook(fn func(host uint32) bool) { dstNetHostDeadHook = fn }

// dstNetHostDead invokes net's dead-host query (false if net is not linked).
// Reached via //go:linkname from testing/simulation's Process guard.
//
//go:linkname dstNetHostDead
func dstNetHostDead(host uint32) bool {
	return dstNetHostDeadHook != nil && dstNetHostDeadHook(host)
}

// dstProcessTeardownHook is os's handler for process-owned resource teardown
// (open simulated Files, virtual fds/flocks, and mmap registrations). Runtime is
// the dependency-free relay so testing/simulation can drive process death without
// importing os.
var dstProcessTeardownHook func(proc uint32)

// dstSetProcessTeardownHook registers os's process resource teardown handler.
// Reached via //go:linkname from os's init.
//
//go:linkname dstSetProcessTeardownHook
func dstSetProcessTeardownHook(fn func(proc uint32)) { dstProcessTeardownHook = fn }

// dstProcessTeardown invokes os's resource teardown handler. Reached via
// //go:linkname from testing/simulation's process-teardown path.
//
//go:linkname dstProcessTeardown
func dstProcessTeardown(proc uint32) {
	if dstProcessTeardownHook != nil {
		dstProcessTeardownHook(proc)
	}
}

// dstDiskFaultHook is os's disk-fault handler, registered from os's init via the
// same passthrough shape as the net hook above (os, unlike net, is always linked,
// but the indirection keeps runtime free of an os dependency and lets the off-build
// leave it nil). The op codes are os's contract (see os/dst_disk_fault.go),
// mirrored by the caller in testing/simulation; host is the victim host id, arg an
// op-specific scalar (a capacity / a latency, for the later disk-fault chunks), and
// path the per-file target (empty for host-level ops). A nil hook makes the call a
// no-op.
var dstDiskFaultHook func(op, host uint32, arg int64, path string)

// dstSetDiskFaultHook registers os's disk-fault handler. Reached via //go:linkname
// from os's init.
//
//go:linkname dstSetDiskFaultHook
func dstSetDiskFaultHook(fn func(op, host uint32, arg int64, path string)) { dstDiskFaultHook = fn }

// dstDiskFaultOp invokes os's disk-fault handler. Reached via //go:linkname from
// testing/simulation's targeting API.
//
//go:linkname dstDiskFaultOp
func dstDiskFaultOp(op, host uint32, arg int64, path string) {
	if dstDiskFaultHook != nil {
		dstDiskFaultHook(op, host, arg, path)
	}
}

// dstDeactivate turns DST off. Used by testing/simulation.Run to restore normal
// behavior after a run.
//
//go:linkname dstDeactivate
func dstDeactivate() {
	dstSeed.Store(0)
	// Clear the activating goroutine's per-g root. It is the one seeded
	// goroutine that survives the run AND can still execute: bubble goroutines
	// exit with the run (gdestroy zeroes dstrand), and the seeded-but-parked
	// goroutines a recovered deadlock abandons are permanently unwakeable
	// (dstDiscardAbandonedDrainChains), so they can never reach a draw site.
	// A stale nonzero root here would pass dstReadRandom's
	// membership gate during a LATER run started by another goroutine, handing
	// this goroutine — and, via newproc1, its new children — deterministic
	// bytes derived from the PREVIOUS run's seed (INV-CRYPTO: the seeded tree
	// is the active run's own). dstDeactivate runs on the activating goroutine
	// (runLocked's defer), so this is a self-write.
	getg().dstrand = 0
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

// dstNextCallbackSeq returns the next per-run registration sequence for a finalizer or
// cleanup being registered by THIS run's bubble — the sibling of dstCallbackEpoch: same
// ownership gate, but a strictly increasing per-registration value the drain sorts by.
// Returns 0 for a foreign/pre-bubble registration (it is deferred, never drained, so
// its seq is unused); the run's own seqs start at 1 (dstActivate resets the counter).
func dstNextCallbackSeq() uintptr {
	if !dstActive() {
		return 0
	}
	if gp := getg().m.curg; gp != nil && gp.bubble != nil && gp.bubble == dstSimBubble {
		return dstCallbackSeq.Add(1)
	}
	return 0
}

// dstFenceActive reports whether the calling goroutine is a bubble goroutine of
// the active simulation — the predicate for the interception-boundary fences
// (raw syscalls, process spawn, signals, cgo; see design.md "The interception
// boundary"). It is true only while a run is active AND the goroutine executing
// right now belongs to the run's bubble. Everything else reads false and keeps
// full host access: the harness goroutines around the run, a foreign bubble, and
// — critically — two contexts that must never be fenced:
//
//   - A post-fork exec child. syscall.forkExec is fenced at entry, so only a
//     non-bubble goroutine ever reaches THAT fork; the child inherits that
//     goroutine's g, whose bubble is nil, so its post-fork RawSyscalls (execve,
//     dup3, close — asm and unfenced-by-construction paths aside) read false.
//     forkExec is not the only fork site — os.checkPidfd's checkClonePidfd
//     forks a CLONE_VFORK|CLONE_VM probe child whose exit_group is a fenced
//     trampoline; os keeps that probe off bubble goroutines entirely (a bubble
//     never runs the process-global pidfd sync.Once — see os/pidfd_linux.go
//     pidfdWorks), so no bubble g ever reaches it.
//   - A signal handler. getg() there is gsignal, whose bubble is nil, so the
//     runtime's own signal-context RawSyscalls are never fenced. This is why the
//     predicate reads getg() directly, not getg().m.curg (which would resolve to
//     the interrupted bubble goroutine and mis-fence the signal machinery).
//
// nosplit + alloc-free + lock-free (reads getg() and two globals only) so the
// nosplit/uintptrkeepalive raw-syscall trampolines may call it without growing
// their stack, and so it is safe from the norace post-fork child. Folds to
// constant false in non-dst builds (dstActive folds via dstBuild).
//
// The push linkname lets the syscall package pull it to fence the trampolines
// and process spawn.
//
//go:nosplit
//go:linkname dstFenceActive
func dstFenceActive() bool {
	if !dstActive() {
		return false
	}
	gp := getg()
	return gp.bubble != nil && gp.bubble == dstSimBubble
}

// dstInSimBubble reports whether the calling goroutine belongs to the ACTIVE
// simulation's bubble — not merely any synctest bubble: a FOREIGN bubble
// running concurrently with a simulation is a distinct scheduling domain, and
// a fault injected from it lands at foreign-bubble timing the seed does not
// control. Reached via //go:linkname from testing/simulation's fault-caller
// guard.
//
//go:linkname dstInSimBubble
func dstInSimBubble() bool {
	gp := getg()
	return gp.bubble != nil && gp.bubble == dstSimBubble
}

// dstRefuseForeignForcedGC panics when a user-forced GC cycle is requested
// during an active simulation run from a goroutine outside the run's bubble.
// A foreign forced cycle would mark the bubble's heap — discovering its
// finalizers and weak pointers — and zero its allocation-trigger counter at a
// wall-clock instant the seed does not control; refuse loudly, like foreign
// fault injection (testing/simulation's caller-position guard). A bubble
// goroutine's runtime.GC() is sanctioned: it runs at that call's
// deterministic point in the schedule.
//
// The guard keys on dstSimEnvSet, which testing/simulation publishes before
// the activation seed store and clears after deactivation. That covers the
// whole run INCLUDING the activation stretch between the seed store and the
// bubble's creation — a foreign cycle landing there would move
// gcController.heapMarked after the dstHeapBase baseline snapshot, silently
// shifting every later trigger crossing — and exempts the white-box
// dstActivate mode (runtime tests; no simulated process env, no bubble),
// which drives its deferral protocol through runtime.GC. Callers: GC and
// goroutineLeakGC (before it arms the process-global leak-detection flag);
// debug.FreeOSMemory funnels through GC, and sysmon's forcegc is neutralized
// separately.
func dstRefuseForeignForcedGC() {
	if dstBuild && dstActive() && dstSimEnvSet && !dstInSimBubble() {
		panic("runtime: GC forced during an active simulation from outside the run's bubble")
	}
}

// dstCgoRefuse panics with the interception-boundary unsupported shape for a
// bubble goroutine calling into cgo (a real C call is wall-clock, host-visible
// work no seed controls; see design.md). It is deliberately NOT nosplit and NOT
// inlined: the cold refusal path, so cgocall's nosplit fast path is unaffected
// and the panic — which grows the stack — runs only after the decision to refuse.
// Reached only from a genuine bubble goroutine (dstFenceActive gated it), which
// always has a healthy stack and a P.
//
// The panic can grow the stack (morestack), which copies cgocall's frame. On
// linux — the only platform DST supports (design.md sources table) — cgocall
// carries only typed cgo pointer args, so the copy is safe. On the platforms
// where cgocall also backs raw syscalls (Windows/solaris/illumos) its frame may
// hold untyped syscall args that must not be adjusted; DST is not active there,
// so this path is unreachable — but if DST ever targets them, this refuse-path
// stack growth must be revisited.
//
//go:noinline
func dstCgoRefuse() {
	panic("runtime: cgo call unsupported under deterministic simulation")
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
