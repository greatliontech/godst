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
// The determinism boundary is Run/Test itself: these are virtualized only inside
// a simulation. Nondeterminism a program captures *before* simulation — e.g.
// reading time.Now or a real pid in an init function and stashing it in a package
// variable — is outside the contract; acquire such values inside the simulation.
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
//     allocated bytes rather than span-granular heap layout). The contract
//     covers objects the simulation itself allocates. Finalizers and cleanups
//     are ownership-scoped: only callbacks registered by the simulation's own
//     goroutines during the run execute inside it; a callback registered
//     outside the simulation (before the run, or mid-run by an outside
//     goroutine) never runs during the run even if a simulation GC discovers
//     its object — it is deferred and runs on the ordinary runtime workers
//     after Run returns.
//
// Not in the contract: byte-level heap MemStats fields
// (HeapAlloc/HeapInuse/HeapReleased/HeapIdle, Sys and the per-subsystem *Sys
// fields). These remain sub-observable noise of allocator span layout and
// scavenging, and NumGC driven by an environment GOMEMLIMIT is likewise out of
// contract. A program must not assert on them; size memory behavior by NumGC
// and Options.MemoryLimit.
//
// # Build constraint
//
// Run, RunWith, Test, and TestWith require building with -tags dst, which fixes
// the process-global map hash key (a precondition for deterministic map iteration
// order that cannot be set at runtime). They panic if the binary was not built
// with that tag.
package simulation

import (
	"internal/asan"
	"internal/godebug"
	"internal/goexperiment"
	"internal/msan"
	"internal/race"
	"internal/synctest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

//go:linkname dstSchedForeignSeenFP runtime.dstSchedForeignSeenFP
func dstSchedForeignSeenFP() bool

//go:linkname dstTraceChosenFP runtime.dstTraceChosenFP
func dstTraceChosenFP(i int) uint64

//go:linkname dstTraceAccessFP runtime.dstTraceAccessFP
func dstTraceAccessFP(i int) (addr uintptr, write bool)

//go:linkname dstTraceEnabledFP runtime.dstTraceEnabledFP
func dstTraceEnabledFP(i int) []uint64

//go:linkname dstTraceOverflowFP runtime.dstTraceOverflowFP
func dstTraceOverflowFP() bool

//go:linkname dstTraceEnabOverflowFP runtime.dstTraceEnabOverflowFP
func dstTraceEnabOverflowFP() bool

//go:linkname dstExplorePanicFP runtime.dstExplorePanicFP
func dstExplorePanicFP() (any, bool)

//go:linkname dstExploreDeadlockFP runtime.dstExploreDeadlockFP
func dstExploreDeadlockFP() string

//go:linkname dstEdgeLenFP runtime.dstEdgeLenFP
func dstEdgeLenFP() int

//go:linkname dstEnsureSeqSelfFP runtime.dstEnsureSeqSelfFP
func dstEnsureSeqSelfFP() uint64

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

//go:linkname dstScheduleAbortStepFP runtime.dstScheduleAbortStepFP
func dstScheduleAbortStepFP() int32

//go:linkname dstExplorePanicStepFP runtime.dstExplorePanicStepFP
func dstExplorePanicStepFP() int32

//go:linkname dstSyncEventOverflowFP runtime.dstSyncEventOverflowFP
func dstSyncEventOverflowFP() bool

//go:linkname dstSetSimEnv runtime.dstSetSimEnv
func dstSetSimEnv(hostname string, pid, numcpu int)

//go:linkname dstClearSimEnv runtime.dstClearSimEnv
func dstClearSimEnv()

//go:linkname dstSetMemLimit runtime.dstSetMemLimit
func dstSetMemLimit(limit int64)

//go:linkname dstSetNetCrossHostLatency runtime.dstSetNetCrossHostLatency
func dstSetNetCrossHostLatency(ns int64)

//go:linkname dstSetNetCrossHostJitter runtime.dstSetNetCrossHostJitter
func dstSetNetCrossHostJitter(ns int64)

//go:linkname dstSetNetCrossHostBandwidth runtime.dstSetNetCrossHostBandwidth
func dstSetNetCrossHostBandwidth(bytesPerSec int64)

//go:linkname dstSetNetSendBuffer runtime.dstSetNetSendBuffer
func dstSetNetSendBuffer(bytes int64)

//go:linkname dstSetNetRetransmitTimeout runtime.dstSetNetRetransmitTimeout
func dstSetNetRetransmitTimeout(ns int64)

//go:linkname testingSimulationTest testing/simulation.testingSimulationTest
func testingSimulationTest(t *testing.T, f func(*testing.T)) bool

//go:linkname testingSimulationCleanupStarted testing/simulation.testingSimulationCleanupStarted
func testingSimulationCleanupStarted(t *testing.T) bool

//go:linkname dstGOMAXPROCSAuto runtime.dstGOMAXPROCSAutoFP
func dstGOMAXPROCSAuto() bool

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

// Options configures RunWith and TestWith. The zero Options is equivalent to
// Run or Test (Random).
type Options struct {
	// Strategy is the interleaving-exploration strategy (default Random).
	Strategy Strategy
	// Depth is the PCT target bug depth d: the number of ordering constraints a
	// bug needs, i.e. the number of priority-change points used. Ignored unless
	// Strategy==PCT. Default 3. Higher d targets deeper bugs but lowers the
	// per-seed hit probability. A value <= 0 selects the default; positive values
	// must fit in the runtime scheduler's int32 strategy field.
	Depth int
	// Steps is the PCT estimate of the number of scheduling decisions in a simulation;
	// the priority-change points are placed uniformly in [1,Steps]. Ignored unless
	// Strategy==PCT. Default 1000; a rough over-estimate is fine. A value <= 0
	// selects the default; positive values must fit in the runtime scheduler's
	// int32 strategy field.
	Steps int

	// Hostname and PID are the simulated process identity defaults: within a
	// simulation, os.Hostname and os.Getpid return deterministic values instead of
	// the real machine's (which vary per run and per host, and would leak
	// nondeterminism into any program under test that reads them). Run, RunWith,
	// Test, and TestWith fix them — to "sim" and 1 by default — so even plain Run or
	// Test is reproducible for a SUT that reads pid/hostname.
	//
	// These are the host-0 / driver defaults; the distributed model refines them.
	// Hostname is os.Hostname() for the root (no Host declared); a Host reports its
	// declared name by default, or HostConfig.Hostname. PID is the root pid; each
	// Process gets a fresh, deterministic pid (a restart gets a new one), so distinct
	// processes have distinct pids. A custom PID must fit in the OS pid_t-sized
	// runtime identity field; values <= 0 select the default, and oversized values
	// panic instead of wrapping. A run that exhausts that finite pid field while
	// allocating Process pids also panics instead of reusing or wrapping pids.
	//
	// The rest of the process-identity surface is fixed to deterministic
	// constants during a simulation (not configurable): os.Getppid is 1;
	// os.Getuid/Geteuid are 7777 and os.Getgid/Getegid are 7777; os.Getgroups
	// is [7777]; os/user.Current reports user "sim" (uid/gid 7777, home
	// "/home/sim"), and the os/user lookup functions resolve against a minimal
	// database containing exactly that user and its group — anything else is
	// the deterministic unknown-user/group error, never a host-database read. crypto/rand is seeded
	// from the simulation's deterministic RNG, so UUIDs/nonces/tokens/keys replay
	// too — outside a simulation crypto/rand is unaffected and remains the real OS
	// source. (crypto/rand is deterministic in the standard configuration only;
	// FIPS mode keeps a process-global SP 800-90A
	// DRBG and BoringCrypto its own generator, neither of which is a simulation
	// configuration.)
	Hostname string
	PID      int

	// NumCPU is the default simulated runtime.NumCPU() within a simulation (default
	// 8; any value <= 0 selects the default), used by every host that does not set
	// HostConfig.NumCPU. GOMAXPROCS is independently forced to 1 for determinism, but
	// NumCPU is reported separately, so a SUT that sizes worker pools or shards by
	// NumCPU still creates real concurrency for the simulation to explore — fixed
	// here so it is reproducible across hosts rather than the real machine's core
	// count. A value of 1 makes the simulated machine single-CPU.
	NumCPU int

	// MemoryLimit, if > 0, is a deterministic bubble-local memory budget in bytes:
	// the simulation triggers GC to keep the program's *own* heap growth within it.
	// It is the deterministic substitute for GOMEMLIMIT, whose total-RSS semantics
	// cannot be modeled deterministically under the simulation (the scavenger is
	// parked and total mapped memory carries process history). Unlike GOMEMLIMIT it
	// bounds the program's heap growth, not total process RSS; 0 leaves memory
	// bounded by GOGC (and a floor when GOGC=off).
	MemoryLimit int64

	// Network configures the simulated in-memory network (the testing/simulation
	// deterministic net). Today it carries the base cross-host link latency; it
	// grows as later network-fault axes (partition, reset, throttle) add policy.
	// The zero value is the plain instant network — byte-identical to no Network
	// config — so even plain Run keeps today's behavior.
	Network NetworkConfig

	// CrashTear makes CrashHost model power loss at page-cache granularity
	// rather than losing every unsynced byte. With it off (the default) a host
	// crash restores exactly the durable image — one legal outcome of the
	// durability contract, and the simplest to reason about. With it on, each
	// dirty PAGE of a file independently either reached the platter, did not, or
	// was caught in flight and tore at a byte boundary; each unsynced
	// directory-entry change independently landed or did not; and a file's
	// unsynced size change lands or does not. Every choice is drawn from the
	// run's fault RNG, so a torn crash is a deterministic function of the seed
	// and replays exactly (DST-FAULT-REPLAY): sweeping seeds sweeps the crash
	// outcomes a crash-consistent database must survive.
	//
	// Writeback flushes pages, and a dirty page carries the CURRENT bytes of
	// every byte in it — so a torn crash never persists an older write's bytes
	// for a byte a newer write covered, a state no page cache produces (see
	// docs/dst/faults.md, the disk Crash axis).
	CrashTear bool
}

// NetworkConfig configures the simulated network for a run.
type NetworkConfig struct {
	// CrossHostLatency is the base one-way delivery latency applied to every
	// simulated TCP connection between two DISTINCT hosts; same-host and loopback
	// connections are always instant. The zero value is instant cross-host
	// delivery — byte-identical to a connection with no latency machinery, so it
	// does not perturb the N=1 collapse or any test that does not set it.
	//
	// It is measured in universe BASE (virtual) time: a per-host clock skew
	// (HostConfig.Clock) shifts what time.Now reads on a host but never the wire
	// delay, so two hosts that disagree on wall time still observe the same
	// latency — the property a clock-skew-tolerant system (e.g. an HLC) is tested
	// against. Delivery stays in-order (FIFO, as TCP). Per-pair asymmetric
	// latencies arrive with the targeting API; this is the single base default.
	CrossHostLatency time.Duration

	// CrossHostJitter, if > 0, is the maximum extra per-segment delivery jitter on
	// every cross-host connection: each segment is delivered after CrossHostLatency
	// plus a value drawn from [0, CrossHostJitter) by the dedicated seeded fault
	// RNG. It is the network-jitter fault — variable link latency — and like the
	// base latency it is base-time and FIFO-preserving (a later segment never
	// overtakes an earlier one, so a reliable in-order stream is never reordered).
	// Deterministic per seed and stream-isolated: enabling it never shifts the
	// goroutine interleaving. The zero value is no jitter (same-host is always
	// jitter-free). Per-link jitter arrives with the L4 targeting API.
	CrossHostJitter time.Duration

	// CrossHostBandwidth, if > 0, is the bandwidth limit in BYTES PER SECOND for
	// each cross-host connection direction (full-duplex, per flow): the link
	// transmits segments serially at this rate, so a segment of S bytes is held
	// S/CrossHostBandwidth before it propagates (then CrossHostLatency + jitter
	// apply). A receiver therefore gets bytes no faster than this rate — the
	// throttle fault, modeling a finite-capacity link. It is deterministic (no
	// random draw) and FIFO-preserving. The zero value is unlimited (same-host is
	// always unlimited). Shared-link contention (one budget across a host-pair's
	// flows) is the L4 per-link refinement; this is per-flow.
	CrossHostBandwidth int64

	// SendBuffer is the per-direction send-buffer capacity in bytes on every
	// connection (a TCP socket's send buffer). A Write blocks once this many bytes
	// are written but not yet delivered to and consumed by the peer, resuming as the
	// link drains — so a program that outruns a slow or partitioned peer feels
	// backpressure instead of buffering unbounded data the peer will never receive.
	// The zero value uses a 1 MiB default; a negative value means unbounded (writes
	// never block, the pre-backpressure behavior).
	SendBuffer int

	// RetransmitTimeout is the virtual-time horizon after which a connection whose
	// data cannot be delivered — any bytes held at a partition (written, in
	// flight, or blocking a full send buffer), a deadline-less Dial across a
	// cut or to a crashed host, or a Dial whose SYN a full accept backlog
	// drops — fails with ETIMEDOUT, modeling a real kernel's ~15
	// retransmissions. The zero value uses a 2-minute default; a negative
	// value disables the horizon (blocked writes and dials wait forever,
	// subject only to their own deadlines).
	RetransmitTimeout time.Duration
}

// default simulated process identity (see Options.Hostname/PID/NumCPU).
const (
	defaultHostname = "sim"
	defaultPID      = 1
	defaultNumCPU   = 8
	maxPID          = 1<<31 - 1
)

// runActive protects the process-global DST knobs that Run mutates. DST state is
// not nestable: seed, scheduler strategy, simulated identity, memory limit,
// async-preempt, and GOMAXPROCS are all process-wide for the duration of a run.
var runActive atomic.Bool

// callerGate orders the caller-position guards (requireBubbleFaultCaller,
// requireBubbleDeclCaller) against the runActive flips. Every guarded API
// holds the read side from its guard check through its state mutation;
// enterSimulation and leaveSimulation hold the write side across the flip.
// Mutual exclusion puts a guarded op's whole extent on one side of any flip:
// before activation, the guard saw false and the op completed against
// pre-run state (the documented no-op) before the run could observe it;
// after, a foreign caller panics at the guard. The activation-edge TOCTOU —
// guard loads false, the CAS lands, the op executes torn into the
// newly-activated run — is unrepresentable. During a settled run the write
// side is never taken (enterSimulation fast-paths the doomed overlap before
// touching the gate), so bubble callers' RLock never contends and never
// parks: no schedule perturbation. The one transient exception is a doomed
// activation-tie loser — two activations that both loaded false — taking the
// write side for the microseconds of its failing CAS at the flip's edge; it
// holds no park and panics immediately. Ops that park forever (a self-crash,
// a dead
// enclosing invocation) release BEFORE parking, and the declaration APIs
// release before running f, so no reader outlives its op
// (TestDSTRunActivationExcludesInFlightGuardedOps).
var callerGate sync.RWMutex

// fips140Mode is latched at startup, mirroring crypto/internal/fips140's own
// init-time read of the GODEBUG, so a mid-process Setenv cannot desynchronize
// the two.
var fips140Mode = func() bool {
	switch godebug.New("#fips140").Value() {
	case "on", "only", "debug":
		return true
	}
	return false
}()

func enterSimulation(api, buildPanic string) {
	if !dstBuilt() {
		panic(buildPanic)
	}
	if fips140Mode {
		// FIPS mode keeps a process-global SP 800-90A DRBG that consumes the
		// simulation's deterministic bytes only as additional input, so
		// crypto/rand would be silently nondeterministic inside the run. Fail
		// loud instead — one GODEBUG is all it takes to be in this mode.
		panic("testing/simulation: " + api + " is unsupported in FIPS 140 mode (GODEBUG fips140=on)")
	}
	if goexperiment.SizeSpecializedMalloc && !race.Enabled && !msan.Enabled && !asan.Enabled {
		// The experiment makes the compiler emit direct size-specialized
		// malloc calls in USER packages, bypassing the mallocgc dispatcher
		// that is the DST heap trigger's single evaluation point: SUT
		// allocations would neither count toward the per-bubble counter nor
		// gate the trigger, silently breaking GC determinism. Fail loud, as
		// with FIPS mode. Instrumented builds are exempt: the compiler
		// suppresses specialized emission whenever it instruments a package
		// (ssagen's sizeSpecializedMallocEnabled; runtime-group packages
		// never get it at all, and the NoInstrument non-runtime packages —
		// runtime/race, runtime/msan, runtime/asan — do get emission but
		// contain no allocating Go code), so under -race/-msan/-asan every
		// heap allocation still funnels through the dispatcher and there is
		// no bypass to refuse. This exemption assumes build-uniform
		// instrumentation; configurations it cannot see (a per-package
		// -gcflags instrumentation opt-out) are caught by the runtime's
		// generated-site backstop, which throws on the first specialized
		// allocation during an active run (see runtime's mallocStub).
		panic("testing/simulation: " + api + " is unsupported with GOEXPERIMENT=sizespecializedmalloc (allocations would bypass the deterministic GC trigger)")
	}
	if synctest.IsInBubble() {
		panic("testing/simulation: " + api + " called from within a synctest bubble")
	}
	// Fast-path the doomed mid-run overlap BEFORE touching callerGate: a
	// pending writer would park mid-run bubble readers (writer preference) on
	// a non-simulated sema — a schedule perturbation the overlap panic alone
	// must not cause. Only when activation is plausible does the write side
	// exclude in-flight guarded ops across the flip (see callerGate); the CAS
	// under the Lock still arbitrates concurrent activation ties.
	if runActive.Load() {
		panic("testing/simulation: " + api + " called while another simulation operation is active")
	}
	callerGate.Lock()
	ok := runActive.CompareAndSwap(false, true)
	callerGate.Unlock()
	if !ok {
		panic("testing/simulation: " + api + " called while another simulation operation is active")
	}
}

func leaveSimulation() {
	callerGate.Lock()
	runActive.Store(false)
	callerGate.Unlock()
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
// count). The GC trigger fires from per-object allocated bytes, so the GC count
// and which cycle discovers a finalizer or weak reference are part of the
// determinism contract (see the package overview); only byte-level heap MemStats
// remain sub-observable allocator noise. Within Run, time
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
// Run, RunWith, Test, TestWith, Explore, ExploreWith, and Replay are
// process-global simulation operations: they must not overlap in one process,
// and must not be called from within a synctest bubble. A rejected attempt
// (nested, concurrent, missing build tag) panics with NO side effect — every
// process-global policy, the active run's crash-tear setting included, is
// untouched; options publish only after the run is admitted. Attempts to change
// GOMAXPROCS with runtime.GOMAXPROCS or runtime.SetDefaultGOMAXPROCS inside the
// simulation are ignored; the run stays pinned to GOMAXPROCS=1 until it returns.
// A process whose GOMAXPROCS was in container-aware auto mode before the run
// returns to auto mode afterward.
//
// If f exits by calling runtime.Goexit, as when it calls t.Fatal on an enclosing
// *testing.T, Run restores the simulation state and then exits its caller with
// runtime.Goexit.
//
// A finalizer or cleanup that panics crashes the process under Run (as in
// production) and is recorded as a Failure by Explore. A finalizer or cleanup
// that calls runtime.Goexit kills the simulation's drain goroutine: from that
// point the run's queued and later-discovered finalizers and cleanups are
// deterministically discarded rather than run.
//
// A goroutine in f that never blocks and never makes a function call (e.g. a
// bare for{}) will not be preempted and will stall the simulation; real code
// rarely does this. A goroutine OUTSIDE the simulation that never blocks (a
// Gosched loop started before Run) does not stall it: outside goroutines are
// scheduled around the simulation and cannot starve it.
func Run(seed uint64, f func()) {
	RunWith(seed, Options{}, f)
}

// Test runs f inside a deterministic simulation seeded by seed, passing f a
// bubble-scoped *testing.T like [testing/synctest.Test]. It is equivalent to
// TestWith(t, seed, Options{}, f).
func Test(t *testing.T, seed uint64, f func(*testing.T)) {
	TestWith(t, seed, Options{}, f)
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
	kind, depth, steps, hostname, pid, numcpu := runOptions("RunWith", opts)
	sendBuf, retransNs := resolveNetConfig(opts.Network)
	run(seed, kind, depth, steps, hostname, pid, numcpu, opts.MemoryLimit, opts.Network.CrossHostLatency.Nanoseconds(), opts.Network.CrossHostJitter.Nanoseconds(), opts.Network.CrossHostBandwidth, sendBuf, retransNs, opts.CrashTear, nil, f)
}

// TestWith is Test with explicit RunWith-style options. The *testing.T passed to
// f has the same synctest properties as the one passed by [testing/synctest.Test]:
// cleanup functions and t.Context run inside the bubble, and T.Run, T.Parallel,
// and T.Deadline must not be called.
func TestWith(t *testing.T, seed uint64, opts Options, f func(*testing.T)) {
	kind, depth, steps, hostname, pid, numcpu := runOptions("TestWith", opts)
	if testingSimulationCleanupStarted(t) {
		// Reject on the caller goroutine, before the bubble exists: the
		// equivalent check inside the bubble would panic on the bubble main
		// goroutine, where it cannot be recovered by the caller.
		panic("testing/simulation: TestWith called during t.Cleanup")
	}
	enterSimulation("TestWith", "testing/simulation: TestWith requires building with -tags dst (for a reproducible map hash key)")
	defer leaveSimulation()
	setCrashTear(opts.CrashTear) // admitted: publish the run's crash policy (see run)
	var ok bool
	sendBuf, retransNs := resolveNetConfig(opts.Network)
	runLocked(seed, kind, depth, steps, hostname, pid, numcpu, opts.MemoryLimit, opts.Network.CrossHostLatency.Nanoseconds(), opts.Network.CrossHostJitter.Nanoseconds(), opts.Network.CrossHostBandwidth, sendBuf, retransNs, nil, true, func() {
		ok = testingSimulationTest(t, f)
	})
	if !ok {
		t.FailNow()
	}
}

// runOptions validates and resolves opts into the run parameters. It is PURE —
// no process-global state moves here: a rejected entry (nested/concurrent run,
// missing build tag) must leave every global policy untouched, so publication
// happens only after enterSimulation admits the run (run/TestWith for the
// crash-tear policy, runLocked for the runtime knobs); Explore and Replay
// publish after their own guards the same way. Panics (invalid options) are
// fine here — they mutate nothing.
func runOptions(api string, opts Options) (kind uint8, depth, steps int32, hostname string, pid, numcpu int) {
	kind = kindRandom
	switch opts.Strategy {
	case Random:
	case PCT:
		kind = kindPCT
		depth, steps = 3, 1000 // defaults
		if opts.Depth > 0 {
			if opts.Depth > maxStrategyParam {
				panic("testing/simulation: " + api + " PCT Depth overflows runtime strategy field")
			}
			depth = int32(opts.Depth)
		}
		if opts.Steps > 0 {
			if opts.Steps > maxStrategyParam {
				panic("testing/simulation: " + api + " PCT Steps overflows runtime strategy field")
			}
			steps = int32(opts.Steps)
		}
	default:
		panic("testing/simulation: " + api + " unknown Strategy")
	}
	hostname = opts.Hostname
	if hostname == "" {
		hostname = defaultHostname
	}
	pid = opts.PID
	if pid <= 0 {
		// <= 0, not == 0, mirroring NumCPU below: a negative PID must not surface as
		// os.Getpid() < 0, an identity no real process can observe. Both 0 and
		// negative mean "use the default".
		pid = defaultPID
	} else if pid > maxPID {
		panic("testing/simulation: " + api + " Options.PID overflows OS pid field")
	}
	numcpu = opts.NumCPU
	if numcpu <= 0 {
		// <= 0, not == 0: a negative NumCPU must not fall through to the real host
		// count (the runtime gate is dstSimNumCPU > 0), which would silently leak a
		// per-host value into the run. Both 0 and negative mean "use the default".
		numcpu = defaultNumCPU
	}
	return kind, depth, steps, hostname, pid, numcpu
}

// resolveNetConfig applies the SendBuffer/RetransmitTimeout defaults, returning the
// values the runtime globals want: sendBuf 0 = unbounded (default 1 MiB when the field
// is 0, unbounded when it is negative); retransNs 0 = no horizon (default 2 minutes
// when the field is 0, disabled when it is negative).
func resolveNetConfig(n NetworkConfig) (sendBuf, retransNs int64) {
	switch {
	case n.SendBuffer == 0:
		sendBuf = 1 << 20 // default 1 MiB
	case n.SendBuffer < 0:
		sendBuf = 0 // unbounded
	default:
		sendBuf = int64(n.SendBuffer)
	}
	switch {
	case n.RetransmitTimeout == 0:
		retransNs = (2 * time.Minute).Nanoseconds() // default 2 minutes
	case n.RetransmitTimeout < 0:
		retransNs = 0 // disabled
	default:
		retransNs = n.RetransmitTimeout.Nanoseconds()
	}
	return
}

// run sets the determinism preconditions, activates DST, and runs f in a synctest
// bubble, restoring everything on return (including on panic). When kind is
// kindScheduled, prefix is the explicit decision sequence the scheduled strategy
// follows (see explore.go); for the other strategies prefix is nil.
func run(seed uint64, kind uint8, depth, steps int32, hostname string, pid, numcpu int, memLimit, netLatencyNs, netJitterNs, netBandwidthBps, netSendBuf, netRetransNs int64, crashTear bool, prefix []uint64, f func()) {
	enterSimulation("Run", "testing/simulation: Run requires building with -tags dst (for a reproducible map hash key)")
	defer leaveSimulation()
	// Admitted: publish the run's crash-tear policy. Every admitted entry point
	// sets it explicitly — here, TestWith after its own enterSimulation,
	// ExploreWith from its options, Replay from the failure it reproduces — so
	// no run inherits the previous run's policy and none clears it on the way
	// out. A REJECTED entry never reaches this line and leaves the active
	// run's policy untouched.
	setCrashTear(crashTear)
	runLocked(seed, kind, depth, steps, hostname, pid, numcpu, memLimit, netLatencyNs, netJitterNs, netBandwidthBps, netSendBuf, netRetransNs, prefix, true, f)
}

// runLocked runs one simulation after enterSimulation has reserved the
// process-global DST state.
func runLocked(seed uint64, kind uint8, depth, steps int32, hostname string, pid, numcpu int, memLimit, netLatencyNs, netJitterNs, netBandwidthBps, netSendBuf, netRetransNs int64, prefix []uint64, propagateGoexit bool, f func()) {
	// The pin below sets the runtime's custom-GOMAXPROCS flag (that is what
	// keeps the sysmon container-aware auto-updater from resizing P count
	// mid-run); remember whether the process was in auto mode so the restore
	// can return it there instead of leaving it pinned to a stale snapshot for
	// the rest of the process.
	autoProcs := dstGOMAXPROCSAuto()
	oldProcs := runtime.GOMAXPROCS(1)
	oldPreempt := dstSetAsyncPreemptOff(true)
	dstSetSchedStrategy(kind, depth, steps)
	if kind == kindScheduled {
		dstSetSchedule(prefix)
	}
	dstSetSimEnv(hostname, pid, numcpu) // before dstActivate: published to the bubble by the activation store
	dstSetMemLimit(memLimit)
	dstSetNetCrossHostLatency(netLatencyNs)
	dstSetNetCrossHostJitter(netJitterNs)
	dstSetNetCrossHostBandwidth(netBandwidthBps)
	dstSetNetSendBuffer(netSendBuf)
	dstSetNetRetransmitTimeout(netRetransNs)
	dstActivate(seed)
	defer func() {
		dstDeactivate()
		dstSetSchedStrategy(kindRandom, 0, 0) // reset for the next run
		dstClearSimEnv()
		dstSetMemLimit(0)
		dstSetNetCrossHostLatency(0)
		dstSetNetCrossHostJitter(0)
		dstSetNetCrossHostBandwidth(0)
		dstSetNetSendBuffer(0)
		dstSetNetRetransmitTimeout(0)
		dstSetAsyncPreemptOff(oldPreempt)
		if autoProcs {
			runtime.SetDefaultGOMAXPROCS()
		} else {
			runtime.GOMAXPROCS(oldProcs)
		}
	}()
	// The pin (GOMAXPROCS(1) above) and activation are not one atomic step: a
	// foreign GOMAXPROCS call that passed its not-yet-active gate can land its
	// stop-the-world inside the window (which spans the activation's
	// preparation GCs). Past activation the runtime re-checks dstActive under
	// every setter's STW and drops such updates, so verifying the pin held
	// here closes the race: fail loud rather than run a silently
	// nondeterministic simulation. (GOMAXPROCS(0) is a pure read.)
	if runtime.GOMAXPROCS(0) != 1 {
		panic("testing/simulation: GOMAXPROCS changed during simulation entry")
	}
	// Reset the Host/Process name→id interning so id assignment is a deterministic
	// function of call order within this run (the schedule, hence call order, is
	// deterministic). Before the bubble starts; the bubble's Host/Process calls
	// then populate it.
	nodeRegReset()
	if propagateGoexit {
		returned := false
		synctest.Run(func() {
			f()
			returned = true
		})
		if !returned {
			runtime.Goexit()
		}
	} else {
		synctest.Run(f)
	}
}
