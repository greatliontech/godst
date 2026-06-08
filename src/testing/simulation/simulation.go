// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simulation provides deterministic simulation testing (DST): a mode in
// which goroutine scheduling and runtime randomness are a reproducible function
// of a seed, so concurrent code replays identically across runs.
//
// It makes the following deterministic, as a function of the seed and the
// program's logical structure: the order in which runnable goroutines are
// scheduled, select poll order, map iteration order, the math/rand and
// math/rand/v2 top-level functions, and synctest timer ordering. It does not
// make wall-clock time, real network/file I/O, or cgo deterministic; programs
// under test must confine themselves to goroutines, channels, sync primitives,
// and time (real I/O is modeled in-memory).
//
// It finds logical concurrency bugs — ordering, atomicity, deadlock, lost
// wakeup, stale read. It runs single-threaded and does not reproduce data races
// that require two goroutines to execute the same instant; those remain the job
// of the race detector. The two are complementary: the race detector tracks
// happens-before, not physical timing, so running under -race checks the
// reproducible interleaving the simulation is exploring for data races.
//
// # Determinism contract (what is and is not guaranteed)
//
// The guarantee is layered, so nothing is over-promised:
//
//   - Logical determinism (unconditional). Goroutine scheduling order, select
//     poll order, map iteration order, math/rand[/v2], synctest timer ordering,
//     and the values the program computes are a reproducible function of the
//     seed. This is the contract the simulation exists for, and it holds under
//     -race (it is driven by a per-goroutine deterministic RNG and
//     single-threaded execution, neither of which the race detector perturbs).
//   - GC set-level (unconditional). The GC count and the total set of objects
//     whose finalizers/weak refs are discovered are deterministic, including under
//     -race (the trigger fires the right number of times with the right total).
//   - GC per-cycle timing (conditional). *Which* GC cycle discovers a given object
//     is deterministic in normal builds, but relaxed under -race/-msan: those
//     rewrite the heap, and the trigger is checked at allocator span boundaries
//     that they shift, so the per-cycle split moves by ±span. The total set and
//     the GC count are unaffected.
//
// A program whose correctness depends on *when* a finalizer runs is observing the
// conditional layer; one that only relies on scheduling/values/replay (the usual
// case) rests entirely on the unconditional layer.
//
// # Build constraint
//
// Run and RunWith require building with -tags dst, which fixes the process-global
// map hash key (a precondition for deterministic map iteration order that cannot
// be set at runtime). They panic if the binary was not built with that tag.
package simulation

import (
	"internal/synctest"
	"runtime"
	_ "unsafe" // for go:linkname
)

//go:linkname dstActivate runtime.dstActivate
func dstActivate(seed uint64)

//go:linkname dstDeactivate runtime.dstDeactivate
func dstDeactivate()

//go:linkname dstSetAsyncPreemptOff runtime.dstSetAsyncPreemptOff
func dstSetAsyncPreemptOff(off bool) (old bool)

//go:linkname dstBuilt runtime.dstBuilt
func dstBuilt() bool

//go:linkname dstSetSchedStrategy runtime.dstSetSchedStrategy
func dstSetSchedStrategy(kind uint8, depth, steps int32)

//go:linkname dstSetSimEnv runtime.dstSetSimEnv
func dstSetSimEnv(hostname string, pid int)

//go:linkname dstClearSimEnv runtime.dstClearSimEnv
func dstClearSimEnv()

//go:linkname dstSetMemLimit runtime.dstSetMemLimit
func dstSetMemLimit(limit int64)

// Strategy selects how RunWith explores goroutine interleavings. All strategies
// are sound (they only reorder goroutines that are simultaneously runnable, a
// real degree of freedom) and deterministic (a function of the seed); they differ
// in which interleavings they prefer.
type Strategy int

const (
	// Random explores a uniformly-random sound interleaving, different per seed.
	// Sweeping seeds samples the interleaving space; the default and the strategy
	// Run uses.
	Random Strategy = iota
	// PCT (Probabilistic Concurrency Testing) biases the schedule with random
	// goroutine priorities and a bounded number of priority-change points, giving
	// a probabilistic guarantee of exposing a concurrency bug of a given "depth"
	// (the number of ordering constraints it needs) per seed — higher yield than
	// uniform-random for deep bugs.
	PCT
)

// runtime strategy kinds (must match runtime/dst.go: dstSchedRandom, dstSchedPCT).
const (
	kindRandom uint8 = iota
	kindPCT
)

// Options configures RunWith. The zero Options is equivalent to Run (Random).
type Options struct {
	// Strategy is the interleaving-exploration strategy (default Random).
	Strategy Strategy
	// Depth is the PCT target bug depth d: the number of ordering constraints a
	// bug needs, i.e. the number of priority-change points used. Ignored unless
	// Strategy==PCT. Default 3. Higher d targets deeper bugs but lowers the
	// per-seed hit probability.
	Depth int
	// Steps is the PCT estimate of the number of scheduling decisions in a run;
	// the priority-change points are placed uniformly in [1,Steps]. Ignored unless
	// Strategy==PCT. Default 1000; a rough over-estimate is fine.
	Steps int

	// Hostname and PID are the simulated process identity: within Run, os.Hostname
	// and os.Getpid return these instead of the real machine's values (which vary
	// per run and per host, and would leak nondeterminism into any program under
	// test that reads them). Both Run and RunWith fix them — to "sim" and 1 by
	// default — so even plain Run is reproducible for a SUT that reads pid/hostname.
	Hostname string
	PID      int

	// MemoryLimit, if > 0, is a deterministic bubble-local memory budget in bytes:
	// the simulation triggers GC to keep the program's *own* heap growth within it.
	// It is the deterministic substitute for GOMEMLIMIT, whose total-RSS semantics
	// cannot be modeled deterministically under the simulation (the scavenger is
	// parked and total mapped memory carries process history). Unlike GOMEMLIMIT it
	// bounds the program's heap growth, not total process RSS; 0 leaves memory
	// bounded by GOGC (and a floor when GOGC=off).
	MemoryLimit int64
}

// default simulated process identity (see Options.Hostname/PID).
const (
	defaultHostname = "sim"
	defaultPID      = 1
)

// Run runs f inside a deterministic simulation seeded by seed: with the same
// seed, the scheduling of goroutines started within f and the runtime
// randomness they observe replay identically.
//
// Run enforces the preconditions for determinism for the duration of the call,
// restoring them afterward: it pins the program to a single thread
// (GOMAXPROCS=1) and disables asynchronous and time-based preemption (so the
// only preemption points are deterministic cooperative ones). Garbage collection
// is left enabled but is constrained for the duration of the call: every GC runs
// stop-the-world with synchronous sweep, and the scavenger is parked. This makes
// GC's effect on the program observably deterministic — allocation results, the
// goroutine schedule, and the GC count are a function of the seed (the world is
// stopped during mark, so no concurrent mark worker interleaves with the
// simulated goroutines and no wall-clock-timed floating garbage perturbs the GC
// count). The exact heap byte at which a GC triggers carries accounting noise
// (from process heap layout) that is not part of the observable determinism
// guarantee; see the package design notes on finalizer discovery. Within Run, time
// is virtual (testing/synctest semantics): it advances only when every goroutine
// started within f is durably blocked.
//
// Run runs two GC cycles when it returns. These do not affect the determinism of
// the run itself; they evict sync.Pool entries that outlive the bubble. A channel
// placed in a sync.Pool inside f is stamped with f's bubble, and reusing it in a
// later Run would fatal ("channel from outside bubble"); the two cycles clear the
// Pool victim cache so each Run starts from a clean pool. This is a cross-Run
// pool-lifetime concern, distinct from the in-run memory bounding that the
// deterministic in-run GC provides.
//
// Run must not be called from within another Run or a synctest bubble.
//
// A goroutine in f that never blocks and never makes a function call (e.g. a
// bare for{}) will not be preempted and will stall the simulation; real code
// rarely does this.
func Run(seed uint64, f func()) {
	RunWith(seed, Options{}, f)
}

// RunWith is Run with an explicit exploration Strategy (Random or PCT). Use it to
// direct interleaving exploration — e.g. Strategy PCT to bias toward exposing
// deep concurrency bugs — while keeping the same per-seed determinism and replay
// guarantees as Run. The zero Options is exactly Run.
//
// Example: explore depth-3 bugs over a seed sweep.
//
//	for seed := uint64(1); seed <= 100000; seed++ {
//		simulation.RunWith(seed, simulation.Options{Strategy: simulation.PCT, Depth: 3}, func() {
//			// start cluster, run workload, assert invariants
//		})
//	}
func RunWith(seed uint64, opts Options, f func()) {
	kind := kindRandom
	var depth, steps int32
	if opts.Strategy == PCT {
		kind = kindPCT
		depth, steps = 3, 1000 // defaults
		if opts.Depth > 0 {
			depth = int32(opts.Depth)
		}
		if opts.Steps > 0 {
			steps = int32(opts.Steps)
		}
	}
	hostname := opts.Hostname
	if hostname == "" {
		hostname = defaultHostname
	}
	pid := opts.PID
	if pid == 0 {
		pid = defaultPID
	}
	run(seed, kind, depth, steps, hostname, pid, opts.MemoryLimit, f)
}

func run(seed uint64, kind uint8, depth, steps int32, hostname string, pid int, memLimit int64, f func()) {
	if !dstBuilt() {
		panic("testing/simulation: Run requires building with -tags dst (for a reproducible map hash key)")
	}
	oldProcs := runtime.GOMAXPROCS(1)
	oldPreempt := dstSetAsyncPreemptOff(true)
	dstSetSchedStrategy(kind, depth, steps)
	dstSetSimEnv(hostname, pid) // before dstActivate: published to the bubble by the activation store
	dstSetMemLimit(memLimit)
	dstActivate(seed)
	defer func() {
		dstDeactivate()
		dstSetSchedStrategy(kindRandom, 0, 0) // reset for the next run
		dstClearSimEnv()
		dstSetMemLimit(0)
		// Evict sync.Pool entries that outlive the bubble (a pooled channel is
		// stamped with this bubble; reusing it in the next Run fatals). Two cycles
		// clear the Pool victim cache. This is a cross-Run pool-lifetime fix,
		// distinct from in-run memory bounding (which the deterministic in-run GC
		// handles). dstDeactivate ran first, so these are ordinary GC cycles.
		runtime.GC()
		runtime.GC()
		dstSetAsyncPreemptOff(oldPreempt)
		runtime.GOMAXPROCS(oldProcs)
	}()
	synctest.Run(f)
}
