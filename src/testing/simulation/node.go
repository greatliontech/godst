// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"slices"
	"strconv"
	"time"
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

// dstHostIdentMu serializes dstSetHostIdent, which publishes a host's identity into
// a copy-on-write table that concurrent Host/Process declarations would otherwise
// race on (the table copy). Host/Process declarations are rare — not a hot path — so
// the lock is free; the table READS (os.Hostname / runtime.NumCPU, in the runtime)
// stay lock-free atomic loads of the published table.
var dstHostIdentMu kernelMutex

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

	// NumCPU is runtime.NumCPU() on this host. A positive value is reported exactly;
	// a value <= 0 means the run default (Options.NumCPU, default 8). GOMAXPROCS
	// stays 1 for determinism regardless.
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

type procKey struct {
	host uint32
	name string
}

type hostBootKey struct {
	host uint32
	boot uint64
}

type pidnsKey struct {
	host uint32
	boot uint64
	name string
}

type procInstance struct {
	proc uint32
	host uint32
	pid  int32
	slot uint32
}

// nodeReg interns Host/Process names to the integer ids the runtime carries on each
// goroutine. Process-global, reset per run (nodeRegReset, called by the run
// envelope before the bubble starts) so id assignment is a deterministic function
// of call order within a run — and call order is deterministic because the schedule
// is. Guarded by a mutex because Host/Process may be called concurrently by bubble
// goroutines (the same in-bubble-use-of-a-process-global-mutex pattern as net's
// simulated-network registry). Host names are run-global; process names are
// host-scoped ((host, name)): CrashHost("x") targets machine "x" and
// CrashProcess("x", "p") targets process "p" on machine "x" (or "" for the
// unnamed root machine).
var nodeReg struct {
	mu       kernelMutex
	hosts    map[string]uint32
	procs    map[procKey]uint32
	nextHost uint32
	nextProc uint32
	nextSlot uint32
	// down is the machines' powered-off state (CrashHost sets, a Host
	// re-declaration clears). Owned HERE, not by the net layer, so host boot
	// semantics (boot count, boot_id, uptime-clock origin) hold in binaries that
	// never link net; the net partition table keeps its own down state for dial
	// blackholing.
	down map[uint32]bool
	// pidns interns ProcessConfig.PIDNamespace names to namespace identities
	// (the /proc/self/ns/pid inode), scoped per (host, boot): first-use order
	// from 2 on each host boot — identity 1 is each host boot's root namespace,
	// never in the map.
	pidns     map[pidnsKey]uint32
	pidnsNext map[hostBootKey]uint32
}

var activeProcs struct {
	mu     kernelMutex
	byProc map[uint32][]procInstance
	bySlot map[uint32]procInstance
}

// procTeardownMu spans a process's liveness bookkeeping and the resource
// teardown it triggers — exit's pop-and-teardown, crash's clear-all plus its
// teardown, and a starting invocation's registration. Each Process invocation
// owns a fresh monotonic slot (g.dstProcSlot) for its OS/net/env resources
// while sharing the logical proc id (g.dstProc) for cumulative memory
// accounting. At plain P=1 the exit defer runs without park points and the lock
// is uncontended; the lock exists for Level 2 exploration, whose
// access-granularity scheduling can yield inside this package's instrumented
// accesses.
var procTeardownMu kernelMutex
var mappingFaultTeardownRuns uint64

func init() { dstSetMappingFaultTeardownHook(mappingFaultProcessTeardown) }

func mappingFaultProcessTeardown(slot uint32, pid int32) {
	if pid <= 0 || slot == 0 {
		return
	}
	procTeardownMu.Lock()
	defer procTeardownMu.Unlock()
	inst, ok := activeProcPopSlot(slot)
	if !ok {
		return
	}
	mappingFaultTeardownRuns++
	dstCrashProcessPid(inst.host, inst.slot, inst.pid)
	dstProcessTeardown(inst.slot)
	dstNetPartitionOp(partOpCrashProcConns, inst.slot, 0)
	dstNetPartitionOp(partOpCloseProcListeners, inst.slot, 0)
}

func nodeRegReset() {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	nodeReg.hosts = make(map[string]uint32)
	nodeReg.procs = make(map[procKey]uint32)
	nodeReg.nextHost = 0
	nodeReg.nextProc = 0
	nodeReg.nextSlot = 0
	nodeReg.down = make(map[uint32]bool)
	nodeReg.pidns = make(map[pidnsKey]uint32)
	nodeReg.pidnsNext = make(map[hostBootKey]uint32)
	activeProcs.mu.Lock()
	activeProcs.byProc = make(map[uint32][]procInstance)
	activeProcs.bySlot = make(map[uint32]procInstance)
	activeProcs.mu.Unlock()
}

// markHostDown records the simulation-owned powered-off state (see
// nodeReg.down). It tolerates a nil map (a crash fault driven before the first
// run's nodeRegReset is a documented no-op path).
func markHostDown(host uint32) {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	if nodeReg.down == nil {
		nodeReg.down = make(map[uint32]bool)
	}
	nodeReg.down[host] = true
}

// nodeNames resolves numeric runtime host and process IDs back to their
// declared names for post-run failure diagnostics. ID 0 is the unnamed root
// scope ("", "").
func nodeNames(host, proc uint32) (hostName, procName string) {
	if host == 0 && proc == 0 {
		return "", ""
	}
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	if host != 0 {
		for name, id := range nodeReg.hosts {
			if id == host {
				hostName = name
				break
			}
		}
	}
	if proc != 0 {
		for k, id := range nodeReg.procs {
			if id == proc {
				procName = k.name
				break
			}
		}
	}
	return hostName, procName
}

func activeProcAdd(inst procInstance) {
	activeProcs.mu.Lock()
	if activeProcs.byProc == nil {
		activeProcs.byProc = make(map[uint32][]procInstance)
	}
	if activeProcs.bySlot == nil {
		activeProcs.bySlot = make(map[uint32]procInstance)
	}
	activeProcs.byProc[inst.proc] = append(activeProcs.byProc[inst.proc], inst)
	if inst.slot > 0 {
		activeProcs.bySlot[inst.slot] = inst
	}
	activeProcs.mu.Unlock()
}

func activeProcPopSlot(slot uint32) (procInstance, bool) {
	activeProcs.mu.Lock()
	defer activeProcs.mu.Unlock()
	inst, ok := activeProcs.bySlot[slot]
	if !ok {
		return procInstance{}, false
	}
	delete(activeProcs.bySlot, slot)
	insts := activeProcs.byProc[inst.proc]
	for i, cur := range insts {
		if cur.slot == slot {
			insts = append(insts[:i], insts[i+1:]...)
			break
		}
	}
	if len(insts) == 0 {
		delete(activeProcs.byProc, inst.proc)
	} else {
		activeProcs.byProc[inst.proc] = insts
	}
	return inst, true
}

func activeProcInstances(proc uint32) []procInstance {
	activeProcs.mu.Lock()
	defer activeProcs.mu.Unlock()
	return append([]procInstance(nil), activeProcs.byProc[proc]...)
}

func activeProcSlots(proc uint32) []uint32 {
	activeProcs.mu.Lock()
	insts := activeProcs.byProc[proc]
	slots := make([]uint32, len(insts))
	for i, inst := range insts {
		slots[i] = inst.slot
	}
	activeProcs.mu.Unlock()
	return slots
}

func activeProcPopAll(proc uint32) []procInstance {
	activeProcs.mu.Lock()
	defer activeProcs.mu.Unlock()
	insts := activeProcs.byProc[proc]
	if len(insts) == 0 {
		return nil
	}
	delete(activeProcs.byProc, proc)
	for _, inst := range insts {
		delete(activeProcs.bySlot, inst.slot)
	}
	return insts
}

// activeProcInstancesOnHost returns the live process invocations running on
// host, ordered deterministically by (proc, pid) so a host crash validates and
// tears its victims down reproducibly (DST-FAULT-REPLAY).
func activeProcInstancesOnHost(host uint32) []procInstance {
	activeProcs.mu.Lock()
	var victims []procInstance
	for _, insts := range activeProcs.byProc {
		if len(insts) > 0 && insts[0].host == host {
			victims = append(victims, insts...)
		}
	}
	activeProcs.mu.Unlock()
	slices.SortFunc(victims, func(a, b procInstance) int {
		if a.proc != b.proc {
			if a.proc < b.proc {
				return -1
			}
			return 1
		}
		if a.pid < b.pid {
			return -1
		}
		if a.pid > b.pid {
			return 1
		}
		return 0
	})
	return victims
}

func activeProcPopHost(host uint32) []procInstance {
	victims := activeProcInstancesOnHost(host)
	if len(victims) == 0 {
		return nil
	}
	activeProcs.mu.Lock()
	for _, inst := range victims {
		delete(activeProcs.bySlot, inst.slot)
		delete(activeProcs.byProc, inst.proc)
	}
	activeProcs.mu.Unlock()
	return victims
}

func init() { dstSetHostDeclaredHook(hostDeclared) }

// hostDeclared reports whether id names a declared host of the current run:
// host 0 (the root) always exists, and declared ids are allocated
// contiguously from 1, so the declared set is exactly [0, nextHost]. Queried by
// net through the runtime relay for the unowned-address dial blackhole.
func hostDeclared(id uint32) bool {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	return id <= nodeReg.nextHost
}

// admitProcAndSlot atomically ensures the logical (host, name) process counter
// and allocates a fresh per-invocation resource slot. Every refusal check runs
// before nodeReg is mutated so a rejected Process admission stays state-neutral.
func admitProcAndSlot(host uint32, name string) (proc, slot uint32) {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	if nodeReg.procs == nil {
		nodeReg.procs = make(map[procKey]uint32)
	}
	key := procKey{host: host, name: name}
	proc, exists := nodeReg.procs[key]
	if !exists {
		proc = nodeReg.nextProc + 1
	}
	if nodeReg.nextSlot == ^uint32(0) {
		panic("testing/simulation: process slot table exhausted")
	}
	dstProcAllocEnsure(proc)
	if !exists {
		nodeReg.nextProc = proc
		nodeReg.procs[key] = proc
	}
	nodeReg.nextSlot++
	slot = nodeReg.nextSlot
	return proc, slot
}

// Host runs f as the named host with the default (zero) HostConfig: clock in
// sync with the universe base clock, os.Hostname equal to name, and
// runtime.NumCPU equal to the run default. It is equivalent to
// HostWith(name, HostConfig{}, f).
func Host(name string, f func()) {
	hostWith("Host", name, HostConfig{}, f)
}

// HostWith runs f as the named host with the given configuration. Goroutines f starts
// (and their descendants) belong to host name, sharing its filesystem and network
// identity and its clock (config.Clock); a Process started within f runs on this
// host. HostWith stamps the running goroutine's host identity for the dynamic extent of f
// and restores it on return — it inherits at goroutine creation, so the stamp labels
// the whole subtree and the subtree's long-lived goroutines outlive the HostWith call. The
// host's clock offset is recorded in a per-host table keyed by that identity, so it
// likewise applies to the whole subtree and can be moved mid-run by StepClock. Hosts
// and processes may be declared at any time
// during a run, including mid-run to model a node joining; re-declaring a host with
// the same name and a seeded clock (BoundedSkew) yields the same offset, so a
// restart keeps the host's clock. The host's hostname (os.Hostname, default the host
// name) and NumCPU (runtime.NumCPU) come from config and are recorded for the host
// for the rest of the run. The zero HostConfig is the plain, in-sync host whose
// os.Hostname is its name. Calling Host or HostWith outside a simulation has no effect beyond
// running f (the recorded identity is read only inside an active run), except that an
// empty host name always panics. During an active run Host and HostWith must be called
// from a goroutine the simulation schedules (the run body or a goroutine started inside
// it); a foreign caller panics, like the fault APIs — a mid-run declaration is a reboot,
// and a reboot at a wall-clock instant the seed does not control would silently diverge replay.
func HostWith(name string, config HostConfig, f func()) {
	hostWith("HostWith", name, config, f)
}

func hostWith(api, name string, config HostConfig, f func()) {
	var oldH, oldP uint32
	stamped := false
	defer func() {
		if stamped {
			dstPopHostScope(oldH, oldP)
		}
	}()
	declare := func() {
		if name == "" {
			panic("testing/simulation: " + api + " name must be non-empty")
		}
		if !runActive.Load() {
			return
		}
		var hid uint32
		var booted bool
		func() {
			nodeReg.mu.Lock()
			defer nodeReg.mu.Unlock()
			var exists bool
			hid, exists = nodeReg.hosts[name]
			if !exists {
				hid = nodeReg.nextHost + 1
			}
			offset := config.Clock.offsetNanos(hid)
			ppb := config.Clock.driftPPBForHost(hid)
			hostname := config.Hostname
			if hostname == "" {
				hostname = name
			}
			if !dstReestablishHostClock(hid, offset, ppb) {
				panic("testing/simulation: " + api + " clock skew takes the wall clock before the epoch (no real kernel accepts a pre-epoch wall clock)")
			}
			if !exists {
				if nodeReg.hosts == nil {
					nodeReg.hosts = make(map[string]uint32)
				}
				nodeReg.nextHost = hid
				nodeReg.hosts[name] = hid
			}
			// The machine BOOTS on the powered-off→up edge only: its first
			// declaration, or its first re-declaration after CrashHost. A
			// re-declaration of a machine that is UP re-establishes the clock but
			// does not boot — its processes survive, and boot identity (boot_id,
			// uptime origin) changing under a live process is a state no kernel
			// shows. After the clock commit, so a rejected declaration stays
			// state-neutral.
			if !exists || nodeReg.down[hid] {
				delete(nodeReg.down, hid)
				dstBootHost(hid)
				booted = true
			}
			setHostIdent(hid, hostname, config.NumCPU)
			_, curProc := dstCurrentNode()
			oldH, oldP = dstPushHostScope(hid, curProc)
			stamped = true
		}()
		// Clock commit precedes identity and caller stamping and rejects without
		// applying any clock or timer change, so every remaining step publishes
		// an already-valid declaration.
		// The declaration boots (or reboots) the machine: if it was crashed, it is
		// reachable again — dials find its kernel (ECONNREFUSED until a listener is
		// up, then connect), and any dial blocked blackholing on the dead machine
		// wakes to retry. In-run only: outside a run there is no network state and
		// Host has no effect beyond running f.
		if booted && runActive.Load() {
			bootHostDisk(hid)
		}
		if runActive.Load() {
			dstNetPartitionOp(partOpHostUp, hid, 0)
		}
	}
	withBubbleDeclCaller(api, declare)
	f()
}

// withBubbleFaultCaller is the fault APIs' caller-position guard, run as
// each one's first act (before victim lookup). A fault invoked during an
// active run from a goroutine outside the RUN'S bubble — no bubble at all, or
// a foreign synctest bubble, itself a distinct scheduling domain — would
// execute at a wall-clock instant the seed does not control (crash,
// partition, disk) or silently no-op (clock faults: the caller has no run
// clock to step) — both the "fault that silently tests nothing" class the
// victim-naming rule already panics on. Outside a run the APIs stay
// documented no-ops.
//
// op is the complete state transition: its first validation or lookup through
// its final mutation. An inactive caller retains callerGate throughout op. An
// admitted active bubble caller validates without acquiring the gate and relies
// on bubble liveness. Moving validation or mutation outside op reintroduces the
// activation-edge tear. User code and self-crash parking stay outside op.
func withBubbleCaller(api, guidance string, yield bool, op func()) {
	panicOutOfBubble := func() {
		panic("testing/simulation: " + api + " called during an active run from outside the run's bubble; " + guidance + " from a goroutine the simulation schedules (the run body or a goroutine started inside it)")
	}
	runInBubble := func() {
		if yield {
			dstYieldPoint()
		}
		op()
	}
	if runActive.Load() {
		if !dstInSimBubble() {
			panicOutOfBubble()
		}
		runInBubble()
		return
	}
	callerGate.RLock()
	if runActive.Load() {
		callerGate.RUnlock()
		if dstInSimBubble() {
			runInBubble()
			return
		}
		panicOutOfBubble()
	}
	defer callerGate.RUnlock()
	op()
}

func withBubbleFaultCaller(api string, op func()) {
	withBubbleCaller(api, "inject faults", true, op)
}

// withBubbleDeclCaller is the declaration APIs' (Host, Process) twin of
// withBubbleFaultCaller, run as each one's first act — before the intern
// tables are touched. A mid-run Host re-declaration relays the host-up op (a
// reboot: partOpHostUp plus clock re-establishment) and Process starts SUT
// goroutines; from a goroutine outside the run's bubble both would mutate run
// state at a wall-clock instant the seed does not control — the same
// silent-nondeterminism class the fault guard kills, through a declaration
// API. Outside a run the APIs keep their documented no-run behavior.
//
// op contains the complete Host declaration or Process admission transition;
// moving its first validation or final publication outside op reintroduces the
// activation-edge tear. User f and Process exit teardown stay outside.
func withBubbleDeclCaller(api string, op func()) {
	withBubbleCaller(api, "declare topology", false, op)
}

// withBubbleInspectCaller guards inspection APIs (HostFS) with the same
// caller-position invariant as withBubbleDeclCaller: during an active run the
// caller must be a goroutine the simulation schedules (never a foreign host
// goroutine racing the run at wall-clock instants), while outside a run the
// call holds callerGate across lookup and handle construction.
func withBubbleInspectCaller(api string, op func()) {
	withBubbleCaller(api, "inspect hosts", false, op)
}

// lookupHost resolves an already-declared host name for a fault or inspection API,
// panicking during a run on a name no Host/HostWith declaration has established — a typo'd victim must fail loud, never intern a fresh host id whose
// state no goroutine observes (a fault that silently tests nothing; docs/dst/faults.md
// "Targeting", victim names fail loud). Outside a run it returns 0 for an unknown name
// — the mutating fault ops all discard host 0 / no-bubble calls, preserving their
// documented outside-a-run no-op — while a name declared by a PREVIOUS run still
// resolves to that run's id for pure-identity inspectors (HostIP), whereas HostFS
// handles are run-epoch-fenced because page-cache memfds are torn down when the run exits.
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

// internPIDNS interns a ProcessConfig.PIDNamespace name to its namespace
// identity on the target host's current boot: "" is the host boot's root
// namespace (identity 1, every process's default); named namespaces get
// deterministic identities in first-use order (2, 3, …), scoped by (host, boot)
// so distinct machines and post-CrashHost reboots never share container
// namespace inodes, while same-boot process restarts naming the same namespace
// retain it.
func internPIDNS(host uint32, name string) uint32 {
	if name == "" {
		return 1
	}
	key := pidnsKey{
		host: host,
		boot: dstCurrentHostBootCount(host),
		name: name,
	}
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	if nodeReg.pidns == nil {
		nodeReg.pidns = make(map[pidnsKey]uint32)
	}
	if nodeReg.pidnsNext == nil {
		nodeReg.pidnsNext = make(map[hostBootKey]uint32)
	}
	if id, ok := nodeReg.pidns[key]; ok {
		return id
	}
	hb := hostBootKey{host: key.host, boot: key.boot}
	nodeReg.pidnsNext[hb]++
	id := 1 + nodeReg.pidnsNext[hb]
	nodeReg.pidns[key] = id
	return id
}

// lookupProc resolves an already-declared process name on host (0 for the
// unnamed root host) for process-targeting fault APIs (CrashProcess,
// ResetProcess).
func lookupProc(host uint32, name string) uint32 {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	if id, ok := nodeReg.procs[procKey{host: host, name: name}]; ok {
		return id
	}
	if runActive.Load() {
		panic("testing/simulation: unknown process " + strconv.Quote(name) + " (no Process declaration; fault victims must name a declared process)")
	}
	return 0
}

func crashProcess(hostName, name string) {
	var hid uint32
	if hostName != "" {
		hid = lookupHost(hostName)
		if hid == 0 {
			return
		}
	}
	proc := lookupProc(hid, name)
	if proc == 0 {
		return
	}
	procTeardownMu.Lock()
	defer procTeardownMu.Unlock()
	insts := activeProcInstances(proc)
	if len(insts) == 0 {
		if runActive.Load() {
			panic("testing/simulation: process " + strconv.Quote(name) + " is not live")
		}
		return
	}
	if !runActive.Load() {
		return
	}
	for _, inst := range insts {
		if dstPidOwnsBubbleMain(inst.slot, inst.pid) {
			panic("testing/simulation: CrashProcess would kill the run's main goroutine — declare a crashable process on its own goroutine (go Process(name, f))")
		}
	}
	insts = activeProcPopAll(proc)
	// Kernel teardown order: the threads die first, then exit_files closes fds
	// (releasing flocks, unmapping (the bytes are the page cache's; nothing to write back)) and the sockets
	// reset. Killing the goroutines first also closes the window in which a
	// victim could observe its own resources half-torn-down.
	for _, inst := range insts {
		dstCrashProcessPid(inst.host, inst.slot, inst.pid)
	}
	for _, inst := range insts {
		dstProcessTeardown(inst.slot)
		dstNetPartitionOp(partOpCrashProcConns, inst.slot, 0)
		dstNetPartitionOp(partOpCloseProcListeners, inst.slot, 0)
	}
}

// CrashProcess kills the named process on host (pass host == "" for a process
// declared on the unnamed root host) — the process-crash fault
// (docs/dst/faults.md "Crash / restart faults"). Every goroutine of the
// process's live invocations on that host is descheduled permanently (no defers
// run — a killed process does not unwind), its pids read dead (Kill(pid, 0)
// answers ESRCH and the /proc entries disappear), its open simulated files and
// virtual fds close, fd-owned flocks release, shared mappings unregister (their
// bytes already are the kernel page-cache pages), its connections RESET — the
// kernel survives a process crash and answers for the dead sockets: the
// surviving peer drains the bytes already delivered to it, then observes
// ECONNRESET (in-flight bytes die; a conn whose victim end was already
// app-closed is spared, its peer drains to io.EOF; a blackhole cut of the
// victim→peer direction swallows the RST — the peer sees silence until a heal
// lets its probes meet the CLOSED socket's RST) — and its listeners close. The
// host filesystem survives untouched, unsynced writes included: a process crash
// does not tear the disk; only the host-crash fault restores the durable image.
// If the calling goroutine itself belongs to the victim (a self-crash — the OOM
// shape), CrashProcess does not return. A subsequent same-name Process call on
// that host is the restart: fresh pid and resource slot, inheriting nothing but
// host state. CrashProcess panics during a run on an undeclared host, an
// undeclared or not-live process name on that host (a typo'd victim must fail
// loud, never silently fault nothing), and on a process whose goroutine set
// includes the run's own main goroutine — a Process declared inline in the run
// body rather than on a goroutine of its own (`go Process(name, f)`): killing
// it would leave the simulation with no driver, so the crash is refused before
// anything is torn down. CrashProcess is a no-op outside a run.
func CrashProcess(host, proc string) {
	withBubbleFaultCaller("CrashProcess", func() { crashProcess(host, proc) })
	if dstSelfCrashed() {
		dstParkCrashedSelf()
	}
}

// crashHost is CrashHost's body, without the self-crash park (so the park
// happens after procTeardownMu is released — parking while holding it would
// strand every later teardown).
func crashHost(name string) {
	host := lookupHost(name)
	if host == 0 || !runActive.Load() {
		return
	}
	// Refuse before anything is torn down (a multi-victim fault must not tear
	// half the universe down and only then panic): the driver's machine cannot
	// lose power while the driver runs. This also catches a host whose only
	// activity is the root process (no Process declared), whose goroutines the
	// pid-keyed kill below would never reach.
	if dstHostOwnsBubbleMain(host) {
		panic("testing/simulation: CrashHost would destroy the machine the run's main goroutine runs on — declare crashable hosts and processes on their own goroutines (go Process(name, f) inside Host)")
	}
	procTeardownMu.Lock()
	// defer, not a trailing Unlock: a panic between here and the end — the
	// pid pre-scan's refusal, or the mark path's backstop — must not strand the
	// mutex and deadlock every later teardown.
	defer procTeardownMu.Unlock()
	victims := activeProcInstancesOnHost(host)
	for _, inst := range victims {
		if dstPidOwnsBubbleMain(inst.slot, inst.pid) {
			panic("testing/simulation: CrashHost would kill the run's main goroutine — declare a crashable process on its own goroutine (go Process(name, f))")
		}
	}
	// The machine's threads stop. That set is the UNION of two things, because
	// neither alone is the machine:
	//
	//   - every goroutine of a process declared on the host (slot-keyed), which
	//     also marks those pids dead for Kill and procfs. This leg matters for a
	//     goroutine of the host's process that is momentarily stamped with
	//     ANOTHER host (it entered a nested Host body): it is still a thread of
	//     a process on the dying machine, and must die with it.
	//   - every goroutine whose current or inherited entered-Host membership
	//     contains this host, which catches the ROOT process's goroutines running
	//     or descended from the machine's Host body even while nested in another
	//     Host. The root process's own pid stays live — it is the driver, with
	//     goroutines on other hosts — so the process leg cannot reach them. A
	//     host is not a process.
	//
	victims = activeProcPopHost(host)
	for _, inst := range victims {
		dstCrashProcessPid(inst.host, inst.slot, inst.pid)
		dstProcessStateTeardown(inst.slot)
	}
	dstMarkHostGoroutinesCrashed(host)
	// Kernel order, as in a process crash: the threads stop, then the kernel's
	// own structures go. Here the kernel itself is gone, so what dies is
	// host-scoped: every open file description and descriptor table on the
	// machine, every advisory lock, every mapping (WITHOUT write-back — dirty
	// pages were never on the disk), every socket (RST) and listener.
	closeHostFiles(host)
	// The machine is off: dials to it blackhole (a dead kernel answers no SYN —
	// connect times out at the deadline or retransmit horizon, never
	// ECONNREFUSED) until a Host re-declaration reboots it. Marked down BEFORE
	// the listeners close so no teardown ordering can expose a
	// listener-closed-but-still-alive refusal window. The simulation-owned
	// powered-off mark (boot-edge detection on the next Host/HostWith
	// declaration) is set alongside net's — one transition drives both.
	markHostDown(host)
	dstNetPartitionOp(partOpHostDown, host, 0)
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
// and every connection an end of which lives on the host goes SILENT at its
// surviving peer — a powered-off machine emits no packet. The peer drains the
// bytes already delivered to its receive queue (nothing can destroy what its
// kernel holds; bytes still in flight died with the machine), then the modeled
// laws surface the death: retransmission exhaustion for outstanding bytes
// (host death arms the horizon as a cut does, ETIMEDOUT), keepalive exhaustion
// for idle connections (Go's default keepalive detects the dead machine at
// ~150s), and — once a Host re-declaration reboots the machine — the RST its
// fresh kernel answers the survivor's next traffic with (ECONNRESET). A conn
// the victim's application had already closed is untouched: its peer drains
// and reads io.EOF (bytes on the wire survive). Every listener closes. Until
// the machine reboots, dialing it blackholes: the connect times out (deadline
// or retransmit horizon, ETIMEDOUT), never
// ECONNREFUSED — refusal needs a live kernel to answer RST. Then the machine's kernel state is gone: its
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
// (see CrashProcess); it is a no-op outside a run.
func CrashHost(name string) {
	withBubbleFaultCaller("CrashHost", func() { crashHost(name) })
	if dstSelfCrashed() {
		dstParkCrashedSelf()
	}
}

// Process runs f as the named process — the unit of crash/restart and OS
// resource isolation. A Process declared inside a Host/HostWith body runs on
// that host; a Process outside any Host runs on the unnamed root host (host 0,
// addressed as host == "" in CrashProcess and ResetProcess) without creating an
// implicit machine. Process names are scoped per host, so multiple hosts may
// concurrently run processes with the same name (e.g. Process("server", ...)).
// Each invocation receives a fresh per-process pid (os.Getpid) and a fresh
// per-invocation OS resource slot (file descriptors, flocks, mappings, working
// directory, environment variables, listeners, and sockets), while sharing the
// logical (host, name) process counter for cumulative memory accounting across
// restarts. The body's return (or panic) is the process's EXIT: goroutines it
// started that are still running are killed, its open simulated files and
// virtual fds close (releasing flocks; shared mappings unregister while their
// page-cache contents survive the exit), its listeners close, and its
// connections close with the kernel's conditional — an end holding unread
// received data answers the peer with RST (ECONNRESET; a blackhole cut of the
// RST's direction swallows it, and the peer's probes meet the CLOSED socket's
// RST after heal instead), otherwise the peer drains buffered bytes then reads
// io.EOF — so a process that needs its work observed synchronizes before
// returning, exactly as a real main must. A process is restarted by calling
// Process again with the same name on that host — it keeps the logical name but
// gets a new pid and resource slot, inheriting nothing from the exited
// invocation except the shared host state (filesystem, page cache). An empty
// process name always panics. During an active run Process must be called from
// a goroutine the simulation schedules; a foreign caller panics, like the fault
// APIs. Process runs in the host's root pid namespace; ProcessWith places an
// invocation in a named one.
func Process(name string, f func()) {
	processWith("Process", name, ProcessConfig{}, f)
}

// ProcessConfig configures one Process invocation. The zero ProcessConfig is
// Process's own behavior.
type ProcessConfig struct {
	// PIDNamespace places the invocation in the named pid namespace on the
	// current host boot. Names intern per (host, boot) to deterministic
	// namespace identities (the same name on the same machine boot is the same
	// namespace — containers sharing a pod's pid namespace; a restart naming
	// the same namespace keeps it, while distinct hosts and reboots get fresh
	// namespaces); the empty name is the host boot's root namespace every
	// process is in by default. Namespaces are SIBLINGS partitioning
	// kill/procfs visibility over the global pid space: cross-namespace (or
	// cross-host), kill(pid, 0) answers ESRCH and /proc/<pid>/stat does not
	// exist — what peer containers and distinct machines see of each other.
	PIDNamespace string
}

// ProcessWith is Process with explicit per-invocation configuration.
func ProcessWith(name string, config ProcessConfig, f func()) {
	processWith("ProcessWith", name, config, f)
}

func processWith(api, name string, config ProcessConfig, f func()) {
	var declaredActive bool
	var declaredEpoch uint64
	var host, proc, slot uint32
	var oldH, oldP, oldSlot uint32
	var simPid, oldPid int32
	withBubbleDeclCaller(api, func() {
		if name == "" {
			panic("testing/simulation: " + api + " name must be non-empty")
		}
		declaredActive = runActive.Load()
		if !declaredActive {
			return
		}
		declaredEpoch = dstNetEpoch()
		// Admission is one transaction with teardown and competing starts.
		procTeardownMu.Lock()
		defer procTeardownMu.Unlock()
		host, _ = dstCurrentNode()
		dstCheckPidAvailable()
		proc, slot = admitProcAndSlot(host, name)
		ns := internPIDNS(host, config.PIDNamespace)
		oldH, oldP = dstSetNode(host, proc)
		oldSlot = dstSetProcSlot(slot)
		simPid = dstAllocPid()
		oldPid = dstSetProcessPid(simPid)
		activeProcAdd(procInstance{proc: proc, host: host, pid: simPid, slot: slot})
		// After dstSetNode: the registration stamps the incarnation's
		// starttime from the TARGET host's uptime clock, read through the
		// admitting goroutine's (just-set) host identity.
		dstRegisterPid(simPid, ns)
	})
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
		if runActive.Load() && oldSlot > 0 && !dstSlotAliveSim(oldSlot) {
			dstParkCrashedSelf()
		}
	}()
	defer func() {
		// Teardown belongs to the run admitted under callerGate. A pre-run
		// Process body may span activation, but its stale proc/PID identity must
		// never apply to that new run.
		valid := declaredActive && runActive.Load() && dstNetEpoch() == declaredEpoch
		if valid && dstProcessHasResources(slot) {
			// Yield at user depth while still carrying simPid when this exiting
			// invocation actually owns open kernel resources so DPOR has a
			// scheduling boundary before kernel-owned exit teardown without
			// injecting redundant steps into resource-clean process exits.
			dstYieldPoint()
		}
		// Restore identity BEFORE parking on the teardown mutex: a goroutine
		// waiting here carries its caller's pid, not the dying invocation's,
		// so a concurrent crash of this logical process cannot mark the parked
		// waiter (the sema dequeue additionally skips crashed waiters).
		dstSetProcessPid(oldPid)
		dstSetProcSlot(oldSlot)
		dstSetNode(oldH, oldP)
		if !valid {
			return
		}
		// Atomically pop this invocation's instance under procTeardownMu: if a
		// concurrent CrashProcess, CrashHost, or mapping fault already claimed
		// and tore down slot while this goroutine waited for procTeardownMu,
		// activeProcPopSlot returns false and exit does nothing.
		procTeardownMu.Lock()
		defer procTeardownMu.Unlock()
		inst, ok := activeProcPopSlot(slot)
		if !ok {
			return
		}
		dstStopProcessThreads(inst.host, inst.slot, inst.pid)
		dstProcessTeardown(inst.slot)
		dstNetPartitionOp(partOpCloseProcConns, inst.slot, 0)
		dstNetPartitionOp(partOpCloseProcListeners, inst.slot, 0)
		dstUnregisterPid(inst.host, inst.pid)
	}()
	f()
}
