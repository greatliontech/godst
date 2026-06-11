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
// math/rand/v2 top-level functions, crypto/rand, synctest timer ordering, and
// the process identity a program observes (os.Getpid/Getppid/Hostname,
// os.Getuid/Getgid/Geteuid/Getegid, os/user.Current, runtime.NumCPU), and TCP
// net.Dial/net.Listen through the in-memory deterministic network. It does not
// make wall-clock time, real file I/O, unsupported network kinds, or cgo
// deterministic; programs under test model those surfaces in-memory or avoid them.
//
// The determinism boundary is Run itself: these are virtualized only inside a
// Run. Nondeterminism a program captures *before* Run — e.g. reading time.Now or
// a real pid in an init function and stashing it in a package variable — is
// outside the contract; acquire such values inside Run.
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
//     poll order, map iteration order, math/rand[/v2], crypto/rand, synctest timer
//     ordering, the simulated process identity, and the values the program
//     computes are a reproducible function of the seed. This is the contract the
//     simulation exists for, and it holds under -race (it is driven by a
//     per-goroutine deterministic RNG and single-threaded execution, neither of
//     which the race detector perturbs).
//   - GC discovery (unconditional). The GC count, the total set of objects whose
//     finalizers/weak refs are discovered, and which GC cycle discovers them are
//     deterministic, including under -race (the trigger fires from per-object
//     allocated bytes rather than span-granular heap layout).
//
// Not in the contract: byte-level heap MemStats fields
// (HeapAlloc/HeapInuse/HeapReleased/HeapIdle). These remain sub-observable noise
// of allocator span layout and scavenging. A program must not assert on them; size
// memory behavior by NumGC and Options.MemoryLimit.
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
	"sync/atomic"
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

// Level-2 exploration substrate (scheduled strategy + trace recorder; see
// explore.go and runtime/dst_explore.go).
//
//go:linkname dstExploreInit runtime.dstExploreInit
func dstExploreInit(maxDecisions, maxEnabledTotal, maxEdges, maxAccesses int)

//go:linkname dstSetSchedule runtime.dstSetSchedule
func dstSetSchedule(prefix []uint64)

//go:linkname dstSetAccessForce runtime.dstSetAccessForce
func dstSetAccessForce(seq, count []uint64, pc []uintptr)

//go:linkname dstTraceLenFP runtime.dstTraceLenFP
func dstTraceLenFP() int

//go:linkname dstTraceChosenFP runtime.dstTraceChosenFP
func dstTraceChosenFP(i int) uint64

//go:linkname dstTraceAccessFP runtime.dstTraceAccessFP
func dstTraceAccessFP(i int) (addr uintptr, write bool)

//go:linkname dstTraceEnabledFP runtime.dstTraceEnabledFP
func dstTraceEnabledFP(i int) []uint64

//go:linkname dstTraceOverflowFP runtime.dstTraceOverflowFP
func dstTraceOverflowFP() bool

//go:linkname dstExplorePanicFP runtime.dstExplorePanicFP
func dstExplorePanicFP() (any, bool)

//go:linkname dstExploreDeadlockFP runtime.dstExploreDeadlockFP
func dstExploreDeadlockFP() string

//go:linkname dstEdgeLenFP runtime.dstEdgeLenFP
func dstEdgeLenFP() int

//go:linkname dstEdgeAtFP runtime.dstEdgeAtFP
func dstEdgeAtFP(i int) (from, to uint64, step, acc int)

//go:linkname dstEdgeOrderFP runtime.dstEdgeOrderFP
func dstEdgeOrderFP(i int) int

//go:linkname dstEdgeOverflowFP runtime.dstEdgeOverflowFP
func dstEdgeOverflowFP() bool

//go:linkname dstSyncEventLenFP runtime.dstSyncEventLenFP
func dstSyncEventLenFP() int

//go:linkname dstSyncEventAtFP runtime.dstSyncEventAtFP
func dstSyncEventAtFP(i int) (kind uint8, id, aux uintptr, seq uint64, step, acc, order int)

//go:linkname dstAccLogLenFP runtime.dstAccLogLenFP
func dstAccLogLenFP() int

//go:linkname dstAccLogAtFP runtime.dstAccLogAtFP
func dstAccLogAtFP(i int) (seq uint64, addr uintptr, size uintptr, pc uintptr, count uint64, write bool, step int)

//go:linkname dstAccLogOverflowFP runtime.dstAccLogOverflowFP
func dstAccLogOverflowFP() bool

//go:linkname dstRaceEnabledFP runtime.dstRaceEnabledFP
func dstRaceEnabledFP() bool

//go:linkname dstScheduleAbortedFP runtime.dstScheduleAbortedFP
func dstScheduleAbortedFP() bool

//go:linkname dstSetSimEnv runtime.dstSetSimEnv
func dstSetSimEnv(hostname string, pid, numcpu int)

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

const maxStrategyParam = 1<<31 - 1

// runtime strategy kinds (must match runtime/dst.go: dstSchedRandom, dstSchedPCT,
// dstSchedScheduled).
const (
	kindRandom uint8 = iota
	kindPCT
	kindScheduled // follow an explicit schedule prefix; used by Explore (see explore.go)
)

// Options configures RunWith. The zero Options is equivalent to Run (Random).
type Options struct {
	// Strategy is the interleaving-exploration strategy (default Random).
	Strategy Strategy
	// Depth is the PCT target bug depth d: the number of ordering constraints a
	// bug needs, i.e. the number of priority-change points used. Ignored unless
	// Strategy==PCT. Default 3. Higher d targets deeper bugs but lowers the
	// per-seed hit probability. A value <= 0 selects the default; positive values
	// must fit in the runtime scheduler's int32 strategy field.
	Depth int
	// Steps is the PCT estimate of the number of scheduling decisions in a run;
	// the priority-change points are placed uniformly in [1,Steps]. Ignored unless
	// Strategy==PCT. Default 1000; a rough over-estimate is fine. A value <= 0
	// selects the default; positive values must fit in the runtime scheduler's
	// int32 strategy field.
	Steps int

	// Hostname and PID are the simulated process identity: within Run, os.Hostname
	// and os.Getpid return these instead of the real machine's values (which vary
	// per run and per host, and would leak nondeterminism into any program under
	// test that reads them). Both Run and RunWith fix them — to "sim" and 1 by
	// default — so even plain Run is reproducible for a SUT that reads pid/hostname.
	//
	// The rest of the process-identity surface is fixed to deterministic constants
	// during a run (not configurable): os.Getppid is 1; os.Getuid/Geteuid are 7777
	// and os.Getgid/Getegid are 7777; os/user.Current reports user "sim" (uid/gid
	// 7777, home "/home/sim"). crypto/rand is seeded from the run's deterministic
	// RNG, so UUIDs/nonces/tokens/keys replay too — outside a run crypto/rand is
	// unaffected and remains the real OS source. (crypto/rand is deterministic in
	// the standard configuration only; FIPS mode keeps a process-global SP 800-90A
	// DRBG and BoringCrypto its own generator, neither of which is a simulation
	// configuration.)
	Hostname string
	PID      int

	// NumCPU is the simulated runtime.NumCPU() within Run (default 8; any value
	// <= 0 selects the default). GOMAXPROCS is independently forced to 1 for
	// determinism, but NumCPU is reported separately, so a SUT that sizes worker
	// pools or shards by NumCPU still creates real concurrency for the simulation
	// to explore — fixed here so it is reproducible across hosts rather than the
	// real machine's core count. A value of 1 makes the simulated machine
	// single-CPU.
	NumCPU int

	// MemoryLimit, if > 0, is a deterministic bubble-local memory budget in bytes:
	// the simulation triggers GC to keep the program's *own* heap growth within it.
	// It is the deterministic substitute for GOMEMLIMIT, whose total-RSS semantics
	// cannot be modeled deterministically under the simulation (the scavenger is
	// parked and total mapped memory carries process history). Unlike GOMEMLIMIT it
	// bounds the program's heap growth, not total process RSS; 0 leaves memory
	// bounded by GOGC (and a floor when GOGC=off).
	MemoryLimit int64
}

// default simulated process identity (see Options.Hostname/PID/NumCPU).
const (
	defaultHostname = "sim"
	defaultPID      = 1
	defaultNumCPU   = 8
)

// runActive protects the process-global DST knobs that Run mutates. DST state is
// not nestable: seed, scheduler strategy, simulated identity, memory limit,
// async-preempt, and GOMAXPROCS are all process-wide for the duration of a run.
var runActive atomic.Bool

func enterSimulation(api, buildPanic string) {
	if !dstBuilt() {
		panic(buildPanic)
	}
	if synctest.IsInBubble() {
		panic("testing/simulation: " + api + " called from within a synctest bubble")
	}
	if !runActive.CompareAndSwap(false, true) {
		panic("testing/simulation: " + api + " called while another simulation operation is active")
	}
}

func leaveSimulation() {
	runActive.Store(false)
}

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
// Run drains two quiet sync.Pool generations before it returns. This does not
// affect the determinism of the run itself; it evicts Pool entries that outlive
// the bubble. A channel placed in a sync.Pool inside f is stamped with f's bubble,
// and reusing it in a later Run would fatal ("channel from outside bubble"); the
// two generations clear the Pool victim cache so each Run starts from a clean
// pool. The drain happens before the bubble is torn down, so finalizers or
// cleanups made reachable only by Pool eviction still run with the bubble active.
// This is a cross-Run pool-lifetime concern, distinct from the in-run memory
// bounding that the deterministic in-run GC provides.
//
// Run, RunWith, Explore, ExploreWith, and Replay are process-global simulation
// operations: they must not overlap in one process, and must not be called from
// within a synctest bubble. Attempts to change GOMAXPROCS with
// runtime.GOMAXPROCS or runtime.SetDefaultGOMAXPROCS inside the simulation are
// ignored; the run stays pinned to GOMAXPROCS=1 until it returns.
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
// guarantees as Run. Unknown Strategy values, and PCT Depth/Steps values that do
// not fit the runtime scheduler field, panic before the run starts. The zero
// Options is exactly Run. It has the same process-global non-overlap restriction
// as Run.
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
	switch opts.Strategy {
	case Random:
	case PCT:
		kind = kindPCT
		depth, steps = 3, 1000 // defaults
		if opts.Depth > 0 {
			if opts.Depth > maxStrategyParam {
				panic("testing/simulation: RunWith PCT Depth overflows runtime strategy field")
			}
			depth = int32(opts.Depth)
		}
		if opts.Steps > 0 {
			if opts.Steps > maxStrategyParam {
				panic("testing/simulation: RunWith PCT Steps overflows runtime strategy field")
			}
			steps = int32(opts.Steps)
		}
	default:
		panic("testing/simulation: RunWith unknown Strategy")
	}
	hostname := opts.Hostname
	if hostname == "" {
		hostname = defaultHostname
	}
	pid := opts.PID
	if pid == 0 {
		pid = defaultPID
	}
	numcpu := opts.NumCPU
	if numcpu <= 0 {
		// <= 0, not == 0: a negative NumCPU must not fall through to the real host
		// count (the runtime gate is dstSimNumCPU > 0), which would silently leak a
		// per-host value into the run. Both 0 and negative mean "use the default".
		numcpu = defaultNumCPU
	}
	run(seed, kind, depth, steps, hostname, pid, numcpu, opts.MemoryLimit, nil, f)
}

// run sets the determinism preconditions, activates DST, and runs f in a synctest
// bubble, restoring everything on return (including on panic). When kind is
// kindScheduled, prefix is the explicit decision sequence the scheduled strategy
// follows (see explore.go); for the other strategies prefix is nil.
func run(seed uint64, kind uint8, depth, steps int32, hostname string, pid, numcpu int, memLimit int64, prefix []uint64, f func()) {
	enterSimulation("Run", "testing/simulation: Run requires building with -tags dst (for a reproducible map hash key)")
	defer leaveSimulation()
	runLocked(seed, kind, depth, steps, hostname, pid, numcpu, memLimit, prefix, f)
}

// runLocked runs one simulation after enterSimulation has reserved the
// process-global DST state.
func runLocked(seed uint64, kind uint8, depth, steps int32, hostname string, pid, numcpu int, memLimit int64, prefix []uint64, f func()) {
	oldProcs := runtime.GOMAXPROCS(1)
	oldPreempt := dstSetAsyncPreemptOff(true)
	dstSetSchedStrategy(kind, depth, steps)
	if kind == kindScheduled {
		dstSetSchedule(prefix)
	}
	dstSetSimEnv(hostname, pid, numcpu) // before dstActivate: published to the bubble by the activation store
	dstSetMemLimit(memLimit)
	dstActivate(seed)
	defer func() {
		dstDeactivate()
		dstSetSchedStrategy(kindRandom, 0, 0) // reset for the next run
		dstClearSimEnv()
		dstSetMemLimit(0)
		dstSetAsyncPreemptOff(oldPreempt)
		runtime.GOMAXPROCS(oldProcs)
	}()
	synctest.Run(f)
}
