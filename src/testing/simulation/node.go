// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"slices"
	"strconv"
	"sync"
	"time"
	_ "unsafe" // for go:linkname
)

// Host and Process declare the distributed model's two identity layers within a
// simulation (see docs/dst/faults.md "The distributed model"): a Host is a machine
// that owns a filesystem and network identity shared by the processes on it; a
// Process is the unit of crash/restart and memory isolation. They stamp the running
// goroutine's host/process identity (g.dstHost / g.dstProc), which the runtime
// inherits to every goroutine the body starts (newproc1) — the labeled-subtree
// tree. Later layers key per-host filesystem/network/clock and per-process
// pid/memory off these ids; faults target a host, host-pair, or process by name.
// The runtime carries integer ids; this file owns the string↔id interning. The
// default (unstamped) host and process are id 0 — so a program that declares
// neither is one host, one process (the N=1 collapse, identical to a plain Run).

//go:linkname dstSetNode runtime.dstSetNode
func dstSetNode(host, proc uint32) (oldHost, oldProc uint32)

//go:linkname dstCurrentNode runtime.dstCurrentNode
func dstCurrentNode() (host, proc uint32)

//go:linkname dstReestablishHostClock runtime.dstReestablishHostClock
func dstReestablishHostClock(host uint32, offset, ppb int64) bool

//go:linkname dstHostSeededClockOffset runtime.dstHostSeededClockOffset
func dstHostSeededClockOffset(hostid uint32, bound int64) int64

//go:linkname dstHostSeededDriftPPB runtime.dstHostSeededDriftPPB
func dstHostSeededDriftPPB(hostid uint32, maxPPB int64) int64

//go:linkname dstSetHostIdent runtime.dstSetHostIdent
func dstSetHostIdent(host uint32, hostname string, numcpu int)

//go:linkname dstAllocPid runtime.dstAllocPid
func dstAllocPid() int32

//go:linkname dstSetProcessPid runtime.dstSetProcessPid
func dstSetProcessPid(pid int32) (old int32)

//go:linkname dstSetPidLive runtime.dstSetPidLive
func dstSetPidLive(pid int32, live bool)

//go:linkname dstCrashProcessPid runtime.dstCrashProcessPid
func dstCrashProcessPid(pid int32)

//go:linkname dstProcessTeardown runtime.dstProcessTeardown
func dstProcessTeardown(proc uint32)

//go:linkname dstSelfCrashed runtime.dstSelfCrashed
func dstSelfCrashed() bool

//go:linkname dstParkCrashedSelf runtime.dstParkCrashedSelf
func dstParkCrashedSelf()

//go:linkname dstPidAliveSim runtime.dstPidAlive
func dstPidAliveSim(pid int32) bool

//go:linkname dstPidOwnsBubbleMain runtime.dstPidOwnsBubbleMain
func dstPidOwnsBubbleMain(pid int32) bool

//go:linkname dstHostOwnsBubbleMain runtime.dstHostOwnsBubbleMain
func dstHostOwnsBubbleMain(host uint32) bool

//go:linkname dstMarkHostGoroutinesCrashed runtime.dstMarkHostGoroutinesCrashed
func dstMarkHostGoroutinesCrashed(host uint32)

//go:linkname dstProcAllocEnsure runtime.dstProcAllocEnsure
func dstProcAllocEnsure(procid uint32)

//go:linkname dstProcAllocBytes runtime.dstProcAllocBytes
func dstProcAllocBytes(procid uint32) int64

// dstHostIdentMu serializes dstSetHostIdent, which publishes a host's identity into
// a copy-on-write table that concurrent Host/Process declarations would otherwise
// race on (the table copy). Host/Process declarations are rare — not a hot path — so
// the lock is free; the table READS (os.Hostname / runtime.NumCPU, in the runtime)
// stay lock-free atomic loads of the published table.
var dstHostIdentMu sync.Mutex

func setHostIdent(host uint32, hostname string, numcpu int) {
	dstHostIdentMu.Lock()
	dstSetHostIdent(host, hostname, numcpu)
	dstHostIdentMu.Unlock()
}

// HostConfig configures a host declared with Host. It is the declarative place to
// set a host's simulated properties; today it carries the host's clock, hostname,
// and NumCPU, and grows as later layers add more per-host identity (e.g. IP). The
// zero HostConfig is the unconfigured host — clock in sync with the universe base
// clock, hostname defaulting to the host name, NumCPU to the run default — so
// Host(name, HostConfig{}, f) is the plain host (the N=1 default, identical to a
// host that declares nothing).
type HostConfig struct {
	// Clock sets the host's wall clock relative to the universe base virtual clock.
	// The zero value is in sync (no skew). Build one with Skew or BoundedSkew.
	Clock ClockConfig

	// Hostname is os.Hostname() on this host. Empty means the host's declared name
	// (Host("node1", ...) reports "node1"), so distinct nodes get distinct hostnames
	// with no extra config; set it to override.
	Hostname string

	// NumCPU is runtime.NumCPU() on this host. A value <= 0 means the run default
	// (Options.NumCPU, default 8). GOMAXPROCS stays 1 for determinism regardless.
	NumCPU int
}

// driftPPBBase is the parts-per-billion scale of a clock rate: rate = 1 + ppb/1e9.
// A host's drift must keep the rate positive (ppb > -driftPPBBase) — a stopped or
// reversed clock is a step, not drift — and is bounded to rate ≤ 2 (ppb ≤
// maxDriftPPB) so the runtime's integer rate arithmetic cannot overflow. Realistic
// crystal drift is parts-per-million (ppb in the thousands), far inside this range.
const (
	driftPPBBase = 1_000_000_000 // 1e9
	maxDriftPPB  = driftPPBBase  // rate in (0, 2]
)

// ClockConfig describes a host's wall clock as a function of the universe base
// virtual clock (docs/dst/faults.md "Per-host clock"): a static wall offset (skew)
// and/or a rate departure (drift). The offset shifts only what time.Now reads on the
// host. Drift (rate ≠ 1) additionally makes the host's wall advance faster/slower than
// base and its relative timers fire after the rate-converted interval — a crystal that
// runs fast or slow, which time-sensitive distributed systems must tolerate. Build one
// with Skew (a fixed offset), BoundedSkew (a per-host seeded offset), Drift (a fixed
// rate), or BoundedDrift (a per-host seeded rate); compose skew and drift with
// Skew(d).WithDrift(ppb) or Skew(d).WithBoundedDrift(maxPPB). The zero value is an
// in-sync clock. A step (an NTP jump) is injected mid-run by StepClock, and a mid-run
// rate change by DriftClock.
type ClockConfig struct {
	seeded      bool          // false: fixed offset; true: offset seeded within ±bound
	offset      time.Duration // static wall offset (when !seeded)
	bound       time.Duration // seeded offset is drawn from [-bound, +bound] (when seeded)
	driftPPB    int64         // clock-rate departure in parts-per-billion (0 = rate 1; used when !driftSeeded)
	driftSeeded bool          // true: rate departure seeded within ±driftBound
	driftBound  int64         // seeded rate is drawn from [-driftBound, +driftBound] ppb (when driftSeeded)
}

// Skew returns a ClockConfig that puts the host's wall clock a fixed offset ahead
// of (offset > 0) or behind (offset < 0) the universe base clock. The offset is
// constant for the run and shifts only time.Now's reading on the host, not
// durations or timer deadlines.
func Skew(offset time.Duration) ClockConfig {
	return ClockConfig{offset: offset}
}

// BoundedSkew returns a ClockConfig whose offset is drawn deterministically from
// the run seed within [-bound, +bound], independently per host. It is stable within
// a run (and across a host restart) and varies with the seed, so sweeping the seed
// (Test/Explore) sweeps the bounded skew-assignment space — the way to explore "all
// the ways N clocks can disagree by up to bound" rather than pinning one offset by
// hand. A non-positive bound is no skew.
func BoundedSkew(bound time.Duration) ClockConfig {
	return ClockConfig{seeded: true, bound: bound}
}

// Drift returns a ClockConfig whose wall clock runs at rate 1 + ppb/1e9 — a departure
// of ppb parts-per-billion from base time. ppb > 0 runs fast (a 2× clock is ppb 1e9),
// ppb < 0 runs slow; the rate is constant for the host's life. A drifting host's
// time.Now and time.Since advance at the rate, and its relative timers (Sleep, After,
// NewTimer, NewTicker, context deadlines) fire after the rate-converted base interval —
// a rate-r host's d-timer fires after d/r of base time. ppb must be in (-1e9, 1e9]
// (rate in (0, 2]); Drift panics otherwise (a non-positive or reversed rate is a step,
// not drift). Realistic crystal drift is a few parts-per-million.
func Drift(ppb int64) ClockConfig {
	return ClockConfig{}.WithDrift(ppb)
}

// WithDrift returns a copy of c with the clock rate set to a departure of ppb
// parts-per-billion (see Drift), so a skew and a drift compose: Skew(d).WithDrift(ppb)
// is a host that is both offset by d and running at rate 1 + ppb/1e9. It panics if ppb
// is out of (-1e9, 1e9].
func (c ClockConfig) WithDrift(ppb int64) ClockConfig {
	if ppb <= -driftPPBBase || ppb > maxDriftPPB {
		panic("testing/simulation: Drift ppb out of range (-1e9, 1e9]; rate must be in (0, 2]")
	}
	c.driftPPB = ppb
	c.driftSeeded = false
	return c
}

// BoundedDrift returns a ClockConfig whose clock RATE departure is drawn
// deterministically from the run seed within [-maxPPB, +maxPPB] parts-per-billion,
// independently per host — the drift analogue of BoundedSkew. It is stable within a run
// (and across a host restart) and varies with the seed, so sweeping the seed
// (Test/Explore) sweeps the bounded rate-assignment space rather than pinning one rate
// by hand. maxPPB must be in [0, 1e9) — every drawn rate 1 + ppb/1e9 then stays in
// (0, 2) (a stopped or reversed clock is a step, not drift); BoundedDrift panics
// otherwise. A zero maxPPB is no drift. Compose with a skew via Skew(d).WithBoundedDrift(maxPPB).
func BoundedDrift(maxPPB int64) ClockConfig {
	return ClockConfig{}.WithBoundedDrift(maxPPB)
}

// WithBoundedDrift returns a copy of c with a seeded rate departure bounded by ±maxPPB
// (see BoundedDrift), so a skew and a seeded drift compose. It panics if maxPPB is out
// of [0, 1e9).
func (c ClockConfig) WithBoundedDrift(maxPPB int64) ClockConfig {
	if maxPPB < 0 || maxPPB >= driftPPBBase {
		panic("testing/simulation: BoundedDrift maxPPB out of range [0, 1e9); every drawn rate must stay in (0, 2)")
	}
	c.driftBound = maxPPB
	c.driftSeeded = true
	return c
}

// offsetNanos resolves the configured clock to a concrete wall offset in
// nanoseconds for the given host id. The seeded case asks the runtime, which hashes
// the run seed with the host id (advancing no RNG stream).
func (c ClockConfig) offsetNanos(hostid uint32) int64 {
	if c.seeded {
		return dstHostSeededClockOffset(hostid, int64(c.bound))
	}
	return int64(c.offset)
}

// driftPPBForHost resolves the configured clock rate to a concrete ppb for the given
// host id. The seeded case asks the runtime, which hashes the run seed with the host id
// (advancing no RNG stream), so a bounded drift replays and is stable across a restart.
func (c ClockConfig) driftPPBForHost(hostid uint32) int64 {
	if c.driftSeeded {
		return dstHostSeededDriftPPB(hostid, c.driftBound)
	}
	return c.driftPPB
}

// nodeReg interns Host/Process names to the integer ids the runtime carries on each
// goroutine. Process-global, reset per run (nodeRegReset, called by the run
// envelope before the bubble starts) so id assignment is a deterministic function
// of call order within a run — and call order is deterministic because the schedule
// is. Guarded by a mutex because Host/Process may be called concurrently by bubble
// goroutines (the same in-bubble-use-of-a-process-global-mutex pattern as net's
// simulated-network registry). Host and process names are independent namespaces:
// CrashHost("x") targets a host and Crash("x") a process.
var nodeReg struct {
	mu       sync.Mutex
	hosts    map[string]uint32
	procs    map[string]uint32
	nextHost uint32
	nextProc uint32
}

var activeProcs struct {
	mu   sync.Mutex
	pids map[uint32][]int32
	// host records which machine each live process runs on, so a host crash
	// can enumerate its victims. Process names are a global namespace (one
	// name interns to one process id), so a process lives on exactly one host
	// at a time; a restart on another host overwrites the entry.
	host map[uint32]uint32
}

// procTeardownMu spans a process's liveness bookkeeping and the resource
// teardown it triggers — exit's last-invocation decision plus the teardown
// itself, crash's clear-all plus its teardown, and a starting invocation's
// registration. Without it, a same-name restart interleaved between the
// "last invocation died" decision and the teardown could register and open
// resources that the predecessor's teardown then closes — a state a real
// kernel (per-process fd tables) cannot show. At plain P=1 the exit defer
// runs without park points and the lock is uncontended; the lock exists for
// Level 2 exploration, whose access-granularity scheduling can yield inside
// this package's instrumented accesses.
var procTeardownMu sync.Mutex

func nodeRegReset() {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	nodeReg.hosts = make(map[string]uint32)
	nodeReg.procs = make(map[string]uint32)
	nodeReg.nextHost = 0
	nodeReg.nextProc = 0
	activeProcs.mu.Lock()
	activeProcs.pids = make(map[uint32][]int32)
	activeProcs.host = make(map[uint32]uint32)
	activeProcs.mu.Unlock()
}

func activeProcSet(proc, host uint32, pid int32) {
	activeProcs.mu.Lock()
	if activeProcs.pids == nil {
		activeProcs.pids = make(map[uint32][]int32)
	}
	if activeProcs.host == nil {
		activeProcs.host = make(map[uint32]uint32)
	}
	activeProcs.pids[proc] = append(activeProcs.pids[proc], pid)
	activeProcs.host[proc] = host
	activeProcs.mu.Unlock()
}

// activeProcLivesElsewhere reports whether the logical process already has a
// live invocation on a DIFFERENT machine. One logical process lives on exactly
// one machine at a time: two homes would give a host crash two candidate victim
// sets, and it would scope by whichever was recorded last — silently sparing a
// pid on the machine that lost power. Checked before a Process stamps anything,
// so the refusal mutates no state.
func activeProcLivesElsewhere(proc, host uint32) bool {
	activeProcs.mu.Lock()
	defer activeProcs.mu.Unlock()
	old, ok := activeProcs.host[proc]
	return ok && old != host && len(activeProcs.pids[proc]) > 0
}

// activeProcsOnHost returns the live processes running on host, in process-id
// order — a deterministic function of declaration order, never the map's
// iteration order, so a host crash tears its victims down reproducibly
// (DST-FAULT-REPLAY).
func activeProcsOnHost(host uint32) []uint32 {
	activeProcs.mu.Lock()
	var procs []uint32
	for proc, h := range activeProcs.host {
		if h == host && len(activeProcs.pids[proc]) > 0 {
			procs = append(procs, proc)
		}
	}
	activeProcs.mu.Unlock()
	slices.Sort(procs)
	return procs
}

// activeProcClear removes one invocation's pid and reports whether it was the
// process's LAST live invocation — the point at which proc-keyed resources can be
// torn down (concurrent same-name invocations share the logical proc id, so
// resource teardown must wait for the last; the goroutine half is pid-keyed and
// per-invocation regardless).
func activeProcClear(proc uint32, pid int32) (last bool) {
	activeProcs.mu.Lock()
	pids := activeProcs.pids[proc]
	for i, p := range pids {
		if p == pid {
			pids = append(pids[:i], pids[i+1:]...)
			break
		}
	}
	if len(pids) == 0 {
		delete(activeProcs.pids, proc)
	} else {
		activeProcs.pids[proc] = pids
	}
	activeProcs.mu.Unlock()
	return len(pids) == 0
}

func activeProcPIDs(proc uint32) []int32 {
	activeProcs.mu.Lock()
	defer activeProcs.mu.Unlock()
	return append([]int32(nil), activeProcs.pids[proc]...)
}

func activeProcClearAll(proc uint32) {
	activeProcs.mu.Lock()
	delete(activeProcs.pids, proc)
	delete(activeProcs.host, proc)
	activeProcs.mu.Unlock()
}

func internHost(name string) uint32 {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	if nodeReg.hosts == nil {
		// Host/Process before the first Run ever: the registry is inert but must not
		// nil-map-panic (the run envelope's nodeRegReset has never run).
		nodeReg.hosts = make(map[string]uint32)
	}
	if id, ok := nodeReg.hosts[name]; ok {
		return id
	}
	nodeReg.nextHost++
	nodeReg.hosts[name] = nodeReg.nextHost
	return nodeReg.nextHost
}

func internProc(name string) uint32 {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	if nodeReg.procs == nil {
		nodeReg.procs = make(map[string]uint32) // see internHost
	}
	if id, ok := nodeReg.procs[name]; ok {
		return id
	}
	nodeReg.nextProc++
	nodeReg.procs[name] = nodeReg.nextProc
	return nodeReg.nextProc
}

// Host runs f as the named host with the given configuration. Goroutines f starts
// (and their descendants) belong to host name, sharing its filesystem and network
// identity and its clock (config.Clock); a Process started within f runs on this
// host. Host stamps the running goroutine's host identity for the dynamic extent of f
// and restores it on return — it inherits at goroutine creation, so the stamp labels
// the whole subtree and the subtree's long-lived goroutines outlive the Host call. The
// host's clock offset is recorded in a per-host table keyed by that identity, so it
// likewise applies to the whole subtree and can be moved mid-run by StepClock. Hosts
// and processes may be declared at any time
// during a run, including mid-run to model a node joining; re-declaring a host with
// the same name and a seeded clock (BoundedSkew) yields the same offset, so a
// restart keeps the host's clock. The host's hostname (os.Hostname, default the host
// name) and NumCPU (runtime.NumCPU) come from config and are recorded for the host
// for the rest of the run. The zero HostConfig is the plain, in-sync host whose
// os.Hostname is its name. Calling Host outside a simulation has no effect beyond
// running f (the recorded identity is read only inside an active run).
func Host(name string, config HostConfig, f func()) {
	hid := internHost(name)
	hostname := config.Hostname
	if hostname == "" {
		hostname = name
	}
	setHostIdent(hid, hostname, config.NumCPU)
	_, curProc := dstCurrentNode()
	oldH, oldP := dstSetNode(hid, curProc)
	// Establish the host's configured clock (offset, and rate) in the per-host table
	// (keyed by host id). No save/restore: the clock is read via g.dstHost, which
	// dstSetNode already saves and restores, so after f returns the caller reads its
	// own host's clock again; the table entry persists for hid's long-lived goroutines
	// that outlive this call. A re-declaration (restart) re-establishes the clock
	// COMPLETELY (docs/dst/faults.md "Clock faults", Host re-declaration): the rate is
	// applied through the DriftClock path unconditionally — including rate 1 for a
	// zero config — so a surviving stale rate/anchor is folded and cleared and the
	// host's armed timers are re-mapped to the declared rate; then the offset is
	// overwritten to the declared value, discarding prior steps and folded drift.
	// Setting only the offset (the earlier shape) left a "restarted, in-sync" host
	// reading ahead of base and sleeping at the old rate — self-consistent to its own
	// probes, wrong against the base clock.
	if !dstReestablishHostClock(hid, config.Clock.offsetNanos(hid), config.Clock.driftPPBForHost(hid)) {
		dstSetNode(oldH, oldP)
		panic("testing/simulation: Host clock skew takes the wall clock before the epoch (no real kernel accepts a pre-epoch wall clock)")
	}
	defer dstSetNode(oldH, oldP)
	f()
}

// lookupHost resolves an already-declared host name for a fault or inspection API,
// panicking during a run on a name no Host (or implicit-host Process) declaration has
// established — a typo'd victim must fail loud, never intern a fresh host id whose
// state no goroutine observes (a fault that silently tests nothing; docs/dst/faults.md
// "Targeting", victim names fail loud). Outside a run it returns 0 for an unknown name
// — the mutating fault ops all discard host 0 / no-bubble calls, preserving their
// documented outside-a-run no-op — while a name declared by a PREVIOUS run still
// resolves to that run's id: the value-returning inspectors (HostFS, HostIP) then
// serve the finished run's view, which is the post-run-inspection reading of the
// leaked-handle stance (deterministic, host-isolated), not a live contract.
func lookupHost(name string) uint32 {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	if id, ok := nodeReg.hosts[name]; ok {
		return id
	}
	if runActive.Load() {
		panic("testing/simulation: unknown host " + strconv.Quote(name) + " (no Host declaration; fault victims must name a declared host)")
	}
	return 0
}

// lookupProc is lookupHost's process leg (ResetProcess and later process faults).
func lookupProc(name string) uint32 {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	if id, ok := nodeReg.procs[name]; ok {
		return id
	}
	if runActive.Load() {
		panic("testing/simulation: unknown process " + strconv.Quote(name) + " (no Process declaration; fault victims must name a declared process)")
	}
	return 0
}

func crashProcess(name string) {
	proc := lookupProc(name)
	if proc == 0 {
		return
	}
	procTeardownMu.Lock()
	defer procTeardownMu.Unlock()
	pids := activeProcPIDs(proc)
	if len(pids) == 0 {
		if runActive.Load() {
			panic("testing/simulation: process " + strconv.Quote(name) + " is not live")
		}
		return
	}
	activeProcClearAll(proc)
	// Kernel teardown order: the threads die first, then exit_files closes fds
	// (releasing flocks, unmapping (the bytes are the page cache's; nothing to write back)) and the sockets
	// reset. Killing the goroutines first also closes the window in which a
	// victim could observe its own resources half-torn-down.
	for _, pid := range pids {
		dstCrashProcessPid(pid)
	}
	dstProcessTeardown(proc)
	dstNetPartitionOp(partOpResetProc, proc, 0)
	dstNetPartitionOp(partOpCloseProcListeners, proc, 0)
}

// Crash kills the named process — the process-crash fault (docs/dst/faults.md
// "Crash / restart faults"). Every goroutine of the process's live invocations
// is descheduled permanently (no defers run — a killed process does not
// unwind), its pids read dead (Kill(pid, 0) answers ESRCH and the /proc
// entries disappear), its open simulated files and virtual fds close, fd-owned
// flocks release, writable shared mappings copy back to file state (page cache
// belongs to the kernel) and unregister, its connections RESET — the peer
// observes ECONNRESET — and its listeners close. The host filesystem survives
// untouched, unsynced writes included: a process crash does not tear the disk;
// only the host-crash fault restores the durable image. If the calling
// goroutine itself belongs to the victim (a self-crash — the OOM shape), Crash
// does not return. A subsequent same-name Process call is the restart: fresh
// pid, inheriting nothing but host state. Crash panics during a run on an
// undeclared or not-live process name (a typo'd victim must fail loud, never
// silently fault nothing), and on a process whose goroutine set includes the
// run's own main goroutine — a Process declared inline in the run body rather
// than on a goroutine of its own (`go Process(name, f)`): killing it would
// leave the simulation with no driver, so the crash is refused before anything
// is torn down. Crash is a no-op outside a run.
func Crash(name string) {
	crashProcess(name)
	if dstSelfCrashed() {
		dstParkCrashedSelf()
	}
}

// crashHost is CrashHost's body, without the self-crash park (so the park
// happens after procTeardownMu is released — parking while holding it would
// strand every later teardown).
func crashHost(name string) {
	host := lookupHost(name)
	if host == 0 {
		return
	}
	// Refuse before anything is torn down (a multi-victim fault must not tear
	// half the universe down and only then panic): the driver's machine cannot
	// lose power while the driver runs. This also catches a host whose only
	// activity is the root process (no Process declared), whose goroutines the
	// pid-keyed kill below would never reach.
	if runActive.Load() && dstHostOwnsBubbleMain(host) {
		panic("testing/simulation: CrashHost would destroy the machine the run's main goroutine runs on — declare crashable hosts and processes on their own goroutines (go Process(name, f) inside Host)")
	}
	procTeardownMu.Lock()
	// defer, not a trailing Unlock: a panic between here and the end — the
	// pid pre-scan's refusal, or the mark path's backstop — must not strand the
	// mutex and deadlock every later teardown.
	defer procTeardownMu.Unlock()
	victims := activeProcsOnHost(host)
	for _, proc := range victims {
		for _, pid := range activeProcPIDs(proc) {
			if dstPidOwnsBubbleMain(pid) {
				panic("testing/simulation: CrashHost would kill the run's main goroutine — declare a crashable process on its own goroutine (go Process(name, f))")
			}
		}
	}
	// The machine's threads stop. That set is the UNION of two things, because
	// neither alone is the machine:
	//
	//   - every goroutine of a process declared on the host (pid-keyed), which
	//     also marks those pids dead for Kill and procfs. This leg matters for a
	//     goroutine of the host's process that is momentarily stamped with
	//     ANOTHER host (it entered a nested Host body): it is still a thread of
	//     a process on the dying machine, and must die with it.
	//   - every goroutine stamped with this host (host-keyed), which catches the
	//     ROOT process's goroutines running the machine's Host body. The root
	//     process's own pid stays live — it is the driver, with goroutines on
	//     other hosts — so the pid-keyed leg cannot reach them. A host is not a
	//     process.
	//
	// One residual, recorded: a ROOT-process goroutine that is inside a nested
	// Host body of ANOTHER machine at the instant this one dies is stamped with
	// that other host and has no pid to key on, so it survives. It is a thread
	// of the driver, not of any declared process, and reaching it would require
	// nesting Host declarations on the driver's own goroutine.
	for _, proc := range victims {
		for _, pid := range activeProcPIDs(proc) {
			dstCrashProcessPid(pid)
		}
		activeProcClearAll(proc)
	}
	dstMarkHostGoroutinesCrashed(host)
	// Kernel order, as in a process crash: the threads stop, then the kernel's
	// own structures go. Here the kernel itself is gone, so what dies is
	// host-scoped: every open file description and descriptor table on the
	// machine, every advisory lock, every mapping (WITHOUT write-back — dirty
	// pages were never on the disk), every socket (RST) and listener.
	closeHostFiles(host)
	dstNetPartitionOp(partOpResetHost, host, 0)
	dstNetPartitionOp(partOpCloseHostListeners, host, 0)
	// Finally the disk: what survives is exactly what was committed to it. The
	// host's disk FAULTS (a bad sector, a full disk, a slow device) are physical
	// properties of the hardware, not kernel state: they survive the reboot.
	restoreHostDisk(host)
}

// CrashHost kills the named host — the power-loss / kernel-panic fault
// (docs/dst/faults.md "Crash / restart faults"). Every process on the host dies
// as under Crash (goroutines descheduled permanently, no defers, pids dead),
// every connection an end of which lives on the host is RESET at its peer —
// the peer's next read fails ECONNRESET without draining, except a conn the
// victim's application had already closed, whose peer still drains and reads
// io.EOF (power loss emits no packet; bytes on the wire survive) — and
// every listener closes. Then the machine's kernel state is gone: its
// filesystem TEARS BACK TO ITS DURABLE IMAGE — data a file's Fsync committed
// survives byte-exactly, a name its parent directory's Fsync committed survives,
// and everything else (unsynced writes, unsynced creates and removes, dirty
// shared mappings) is lost, exactly as power loss loses the page cache. A
// process crash, whose kernel survives, tears nothing.
//
// Restart is a fresh Host declaration (which reboots the machine's clock) with
// its processes started inside it; they reopen the recovered on-disk image with
// clean process-owned resources. Other hosts are untouched — their disks,
// locks, and connections among themselves survive. CrashHost panics during a
// run on an undeclared host name, and on a host owning the run's main goroutine
// (see Crash); it is a no-op outside a run.
func CrashHost(name string) {
	crashHost(name)
	if dstSelfCrashed() {
		dstParkCrashedSelf()
	}
}

// Process runs f as the named process — the unit of crash/restart and memory
// isolation. A Process declared inside a Host body runs on that host; a Process
// outside any Host gets an implicit dedicated host named after the process (the
// common one-process-per-machine topology, so CrashHost(name) and Crash(name) both
// address it; its os.Hostname is the process name). Process stamps the running
// goroutine's process identity (and host, if it allocated an implicit one) and a
// fresh per-process pid (os.Getpid) for the dynamic extent of f and restores them on
// return, labeling the whole subtree. The body's return (or panic) is the process's
// EXIT: goroutines it started that are still running are killed, its open simulated
// files and virtual fds close (releasing flocks; writable shared mappings write back
// and unregister — page-cache contents survive the exit), its listeners close, and
// its connections close with the kernel's conditional — an end holding unread
// received data answers the peer with RST (ECONNRESET), otherwise the peer drains
// buffered bytes then reads io.EOF — so a
// process that needs its work observed synchronizes before returning, exactly as a
// real main must. A process is restarted by calling Process again with the same
// name — it keeps the logical name but gets a new pid, as a real restart does, and
// the restart inherits nothing from the exited invocation but the shared host state
// (filesystem, page cache).
func Process(name string, f func()) {
	host, _ := dstCurrentNode()
	if host == 0 {
		host = internHost(name)
		setHostIdent(host, name, 0) // implicit host: hostname = process name, default NumCPU
		// Deliberately NO clock re-establishment here: a process restart does not
		// reboot its host — the host's clock (rate, offset, applied StepClock
		// faults) survives Process re-invocation; only a Host re-declaration models
		// the reboot. A first-ever implicit host reads the per-run table's zero
		// entry (in-sync, rate 1) with no call needed.
	}
	proc := internProc(name)
	if runActive.Load() && activeProcLivesElsewhere(proc, host) {
		panic("testing/simulation: process " + strconv.Quote(name) + " is already live on another host; a logical process lives on one machine at a time (let it exit before restarting it elsewhere)")
	}
	dstProcAllocEnsure(proc) // per-process allocation counter exists before the body allocates
	oldH, oldP := dstSetNode(host, proc)
	simPid := dstAllocPid()
	oldPid := dstSetProcessPid(simPid)
	// Registration serializes with any in-flight teardown of this logical
	// process (procTeardownMu): a restart must not become live — nor open
	// resources — while its predecessor's exit/crash teardown is mid-flight.
	procTeardownMu.Lock()
	activeProcSet(proc, host, simPid)
	procTeardownMu.Unlock()
	// Registered FIRST so it runs LAST — after the exit-teardown defer below
	// has completed and released procTeardownMu (parking while holding it
	// would strand every later teardown). If the ENCLOSING invocation died
	// while this body ran — crash and exit both mark goroutines by pid, which
	// this goroutine did not carry inside the nested body — the pid restore
	// below would hand a dead invocation a running goroutine: a thread
	// outliving its process, which no kernel shows. Park it forever instead
	// (during a panic unwind too: the enclosing process is dead, so the
	// unwind is forfeit exactly like a crash victim's defers).
	defer func() {
		if runActive.Load() && oldPid > 0 && !dstPidAliveSim(oldPid) {
			dstParkCrashedSelf()
		}
	}()
	live := false
	defer func() {
		if live {
			dstSetPidLive(simPid, false)
		}
		// Restore identity BEFORE parking on the teardown mutex: a goroutine
		// waiting here carries its caller's pid, not the dying invocation's,
		// so a concurrent crash of this logical process cannot mark the parked
		// waiter (the sema dequeue additionally skips crashed waiters).
		dstSetProcessPid(oldPid)
		dstSetNode(oldH, oldP)
		// The last-invocation decision and the teardown it triggers are one
		// critical section (procTeardownMu): a same-name restart cannot
		// register between the two and have its fresh resources closed by the
		// predecessor's teardown.
		procTeardownMu.Lock()
		defer procTeardownMu.Unlock()
		last := activeProcClear(proc, simPid)
		// A returning (or panicking) body models process EXIT: the kernel kills
		// the invocation's remaining threads, then closes its fds — releasing
		// flocks, unmapping (writable MAP_SHARED bytes persist: page cache
		// belongs to the kernel) — and closes its sockets gracefully (the peer
		// drains then reads io.EOF; RST is the crash fault's shape, not exit's).
		// Without this the pid reads dead (Kill → ESRCH, procfs gone) while the
		// invocation's goroutines, fds, and locks live on — a half-dead state no
		// real kernel exhibits. The caller's own pid/node were restored above, so
		// the pid-keyed goroutine kill never marks the returning goroutine; the
		// proc-keyed resource half waits for the process's LAST live invocation.
		// Outside an active run there is nothing to exit — the registries hold a
		// finished run's leaked state (deterministic, host-isolated, meaningless),
		// which teardown must not disturb.
		if runActive.Load() {
			dstCrashProcessPid(simPid)
			if last {
				dstProcessTeardown(proc)
				dstNetPartitionOp(partOpCloseProcConns, proc, 0)
				dstNetPartitionOp(partOpCloseProcListeners, proc, 0)
			}
		}
	}()
	dstSetPidLive(simPid, true)
	live = true
	f()
}
