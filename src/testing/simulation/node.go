// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
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
}

func nodeRegReset() {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	nodeReg.hosts = make(map[string]uint32)
	nodeReg.procs = make(map[string]uint32)
	nodeReg.nextHost = 0
	nodeReg.nextProc = 0
	activeProcs.mu.Lock()
	activeProcs.pids = make(map[uint32][]int32)
	activeProcs.mu.Unlock()
}

func activeProcSet(proc uint32, pid int32) {
	activeProcs.mu.Lock()
	if activeProcs.pids == nil {
		activeProcs.pids = make(map[uint32][]int32)
	}
	activeProcs.pids[proc] = append(activeProcs.pids[proc], pid)
	activeProcs.mu.Unlock()
}

func activeProcClear(proc uint32, pid int32) {
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
}

func activeProcPIDs(proc uint32) []int32 {
	activeProcs.mu.Lock()
	defer activeProcs.mu.Unlock()
	return append([]int32(nil), activeProcs.pids[proc]...)
}

func activeProcClearAll(proc uint32) {
	activeProcs.mu.Lock()
	delete(activeProcs.pids, proc)
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
	pids := activeProcPIDs(proc)
	if len(pids) == 0 {
		if runActive.Load() {
			panic("testing/simulation: process " + strconv.Quote(name) + " is not live")
		}
		return
	}
	activeProcClearAll(proc)
	dstProcessTeardown(proc)
	dstNetPartitionOp(partOpResetProc, proc, 0)
	dstNetPartitionOp(partOpCloseProcListeners, proc, 0)
	for _, pid := range pids {
		dstCrashProcessPid(pid)
	}
}

// Process runs f as the named process — the unit of crash/restart and memory
// isolation. A Process declared inside a Host body runs on that host; a Process
// outside any Host gets an implicit dedicated host named after the process (the
// common one-process-per-machine topology, so CrashHost(name) and Crash(name) both
// address it; its os.Hostname is the process name). Process stamps the running
// goroutine's process identity (and host, if it allocated an implicit one) and a
// fresh per-process pid (os.Getpid) for the dynamic extent of f and restores them on
// return, labeling the whole subtree. A process is restarted by calling Process
// again with the same name — it keeps the logical name but gets a new pid, as a real
// restart does.
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
	dstProcAllocEnsure(proc) // per-process allocation counter exists before the body allocates
	oldH, oldP := dstSetNode(host, proc)
	simPid := dstAllocPid()
	oldPid := dstSetProcessPid(simPid)
	activeProcSet(proc, simPid)
	live := false
	defer func() {
		if live {
			dstSetPidLive(simPid, false)
		}
		activeProcClear(proc, simPid)
		dstSetProcessPid(oldPid)
		dstSetNode(oldH, oldP)
	}()
	dstSetPidLive(simPid, true)
	live = true
	f()
}
