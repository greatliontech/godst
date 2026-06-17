// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
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

//go:linkname dstSetHostClockOffset runtime.dstSetHostClockOffset
func dstSetHostClockOffset(offset int64) (old int64)

//go:linkname dstHostSeededClockOffset runtime.dstHostSeededClockOffset
func dstHostSeededClockOffset(hostid uint32, bound int64) int64

//go:linkname dstSetHostIdent runtime.dstSetHostIdent
func dstSetHostIdent(host uint32, hostname string, numcpu int)

//go:linkname dstAllocPid runtime.dstAllocPid
func dstAllocPid() int32

//go:linkname dstSetProcessPid runtime.dstSetProcessPid
func dstSetProcessPid(pid int32) (old int32)

//go:linkname dstProcAllocEnsure runtime.dstProcAllocEnsure
func dstProcAllocEnsure(procid uint32)

//go:linkname dstProcAllocBytes runtime.dstProcAllocBytes
func dstProcAllocBytes(procid uint32) int64

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

// ClockConfig describes a host's wall clock as a function of the universe base
// virtual clock (docs/dst/faults.md "Per-host clock"). Today it expresses a static
// wall offset — the clock skew an HLC and other time-sensitive distributed systems
// are built to tolerate. The offset shifts only what time.Now reads on the host,
// never durations or timer deadlines, so relative timers fire at the same base time
// on every host. Build one with Skew (a fixed offset) or BoundedSkew (a per-host
// offset drawn from the seed within a bound). The zero value is no skew. Dynamic
// clock perturbation — drift (rate != 1) and step (an NTP jump) — is a later
// fault-layer axis over this same representation.
type ClockConfig struct {
	seeded bool          // false: fixed offset; true: offset seeded within ±bound
	offset time.Duration // static wall offset (when !seeded)
	bound  time.Duration // seeded offset is drawn from [-bound, +bound] (when seeded)
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

// offsetNanos resolves the configured clock to a concrete wall offset in
// nanoseconds for the given host id. The seeded case asks the runtime, which hashes
// the run seed with the host id (advancing no RNG stream).
func (c ClockConfig) offsetNanos(hostid uint32) int64 {
	if c.seeded {
		return dstHostSeededClockOffset(hostid, int64(c.bound))
	}
	return int64(c.offset)
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

func nodeRegReset() {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
	nodeReg.hosts = make(map[string]uint32)
	nodeReg.procs = make(map[string]uint32)
	nodeReg.nextHost = 0
	nodeReg.nextProc = 0
}

func internHost(name string) uint32 {
	nodeReg.mu.Lock()
	defer nodeReg.mu.Unlock()
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
// host. Host stamps the running goroutine's host identity and clock offset for the
// dynamic extent of f and restores them on return — they inherit at goroutine
// creation, so the stamp labels the whole subtree and the subtree's long-lived
// goroutines outlive the Host call. Hosts and processes may be declared at any time
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
	dstSetHostIdent(hid, hostname, config.NumCPU)
	_, curProc := dstCurrentNode()
	oldH, oldP := dstSetNode(hid, curProc)
	oldOff := dstSetHostClockOffset(config.Clock.offsetNanos(hid))
	defer func() {
		dstSetHostClockOffset(oldOff)
		dstSetNode(oldH, oldP)
	}()
	f()
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
		dstSetHostIdent(host, name, 0) // implicit host: hostname = process name, default NumCPU
	}
	pid := internProc(name)
	dstProcAllocEnsure(pid) // per-process allocation counter exists before the body allocates
	oldH, oldP := dstSetNode(host, pid)
	oldPid := dstSetProcessPid(dstAllocPid())
	defer func() {
		dstSetProcessPid(oldPid)
		dstSetNode(oldH, oldP)
	}()
	f()
}
