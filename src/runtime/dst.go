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
//go:nosplit
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
	// Root the caller, then turn routing on. Correctness does not rely on the
	// atomic ordering the store provides: every goroutine that can draw under DST
	// is either created after activation (newproc1 seeds it from its parent, with
	// goroutine creation establishing happens-before) or is the caller itself,
	// rooted here; a synctest bubble re-roots its main independently. The store
	// order just avoids the caller observing dstActive with an unrooted dstrand.
	getg().dstrand = dstBubbleRoot(seed)
	dstSchedRand = dstSchedRoot(seed)
	dstSeed.Store(seed)
	// Establish the per-bubble heap baseline: a full GC here (STW now that DST is
	// active) collects pre-bubble garbage so gcController.heapMarked is the
	// process *live* set, and we snapshot it as the baseline the relative heap
	// trigger subtracts out. Without the GC the baseline would include pre-bubble
	// garbage that the first in-bubble GC then frees, driving heapMarked below the
	// baseline and breaking the relative computation. See docs/dst/design.md
	// (Tier 2, per-bubble relative trigger).
	GC()
	// The entry GC queues finalizers for objects that died *before* this bubble
	// (process-level garbage, e.g. transient stdlib objects). Run them now, on this
	// still-bubble-less goroutine (getg().bubble == nil), so the bubble's finalizer
	// drain does not run them in-bubble at the first quiescence: they are not part
	// of any bubble's dead set, and their run-to-run-varying count (process heap
	// history) would otherwise add nondeterministic entries to the first
	// quiescence's finalizer run set. They cannot touch this bubble's channels
	// (the bubble does not exist yet), so running them here is safe. After this,
	// finq is empty and the bubble's finalizer state starts from a clean baseline.
	//
	// Cleanups (runtime.AddCleanup) get the same treatment: the entry GC's sweep
	// flushed them, and draining them here (bubble-less) keeps pre-bubble and
	// prior-Run cleanups out of this bubble's first-quiescence drain. The async
	// cleanup pool is gated off under DST (proc.go), so this is the only thing that
	// runs them before the bubble.
	dstDrainFinq()
	dstDrainCleanups()
	dstHeapBase.Store(gcController.heapMarked)
	dstFinqBase.Store(finqueued)
}

// dstHeapBase is the process-live heap snapshot taken at bubble entry (after a
// forced GC). The DST heap trigger (gcTrigger.test) fires on the bubble's growth
// relative to this baseline, so the process's pre-bubble heap history — which
// varies run to run — does not perturb the GC *set level* inside the bubble: the
// GC count and the total set of finalizers/weak refs discovered are a
// deterministic function of the bubble's own allocation (the contract; DST-GC-1).
//
// The trigger fires on heap *bytes*, so *which cycle* discovers a given object is
// byte-exact only in a fixed normal build and is sub-observable noise otherwise
// (perturbed by -race/-msan redzones or a change in binary composition). That
// per-cycle byte-exactness is NOT part of the contract and is not tested — a SUT
// relies on the set level and on scheduling/values/replay, which are unaffected.
// If a SUT drops references to pre-bubble objects so process-live falls below this
// baseline, the trigger degrades soundly to the heapMinimum floor (gcTrigger.test
// guards heapMarked > dstHeapBase).
var dstHeapBase atomic.Uint64

// dstFinqBase is the finqueued snapshot at bubble entry. finqueued is a
// process-cumulative counter, and the entry GC queues a run-to-run-varying number
// of pre-bubble finalizers, so only the delta from this baseline is
// bubble-deterministic.
var dstFinqBase atomic.Uint64

// dstBubbleFinqFP returns the bubble-local count of finalizers queued so far
// (finqueued minus the bubble-entry baseline) — the *set-level* finalizer-
// discovery observable. This total is the contract (DST-GC-1): the GC count and
// the total set of discovered finalizers are deterministic, including under
// -race. (Which GC *cycle* discovers a given object is not part of the contract —
// it is sub-observable byte-trigger noise; the simulation does not claim or test
// it. See design.md D1.)
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

// dstSchedRandUint64 advances and returns the next scheduling-RNG value.
//
//go:nosplit
func dstSchedRandUint64() uint64 {
	dstSchedRand += 0x9e3779b97f4a7c15
	z := dstSchedRand
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

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
// Set by dstSetSimEnv *before* dstActivate, so the activation's atomic store
// publishes them to the bubble's goroutines; read by os.Getpid/os.Hostname etc.
// and runtime.NumCPU via the linkname'd accessors below. dstSimEnvSet is false on
// the white-box dstActivate path (no public Run), so the real identity is
// returned there. Hostname, PID, and NumCPU are configurable (testing/simulation
// Options); the remaining identity (ppid, uid/gid, the current user) is fixed to
// the deterministic constants below, which testing/simulation documents.
var (
	dstSimPID      int
	dstSimHostname string
	dstSimNumCPU   int // simulated runtime.NumCPU(); 0 leaves NumCPU real
	dstSimEnvSet   bool
)

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
}

// dstClearSimEnv stops simulating process identity (run end).
//
//go:linkname dstClearSimEnv
func dstClearSimEnv() {
	dstSimEnvSet = false
	dstSimHostname = ""
	dstSimPID = 0
	dstSimNumCPU = 0
}

// The accessors below are read by os.Getpid/Getppid/Getuid/.../os.Hostname and
// os/user.Current (via linkname) to return the simulated identity during a run;
// the bool reports whether process identity is being simulated. runtime.NumCPU
// reads dstSimNumCPU/dstSimEnvSet directly (same package).
//
//go:linkname dstSimGetpid
func dstSimGetpid() (int, bool) { return dstSimPID, dstSimEnvSet }

//go:linkname dstSimGethostname
func dstSimGethostname() (string, bool) { return dstSimHostname, dstSimEnvSet }

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

// dstDeactivate turns DST off. Used by testing/simulation.Run to restore normal
// behavior after a run.
//
//go:linkname dstDeactivate
func dstDeactivate() {
	dstSeed.Store(0)
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
