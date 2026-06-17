// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"sync"
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

// Host runs f as the named host. Goroutines f starts (and their descendants) belong
// to host name, sharing its filesystem and network identity; a Process started
// within f runs on this host. Host stamps the running goroutine's host identity for
// the dynamic extent of f and restores it on return — the ids inherit at goroutine
// creation, so the stamp labels the whole subtree and the subtree's long-lived
// goroutines outlive the Host call. Hosts and processes may be declared at any time
// during a run, including mid-run to model a node joining. Calling Host outside a
// simulation has no effect beyond running f.
func Host(name string, f func()) {
	hid := internHost(name)
	_, curProc := dstCurrentNode()
	oldH, oldP := dstSetNode(hid, curProc)
	defer dstSetNode(oldH, oldP)
	f()
}

// Process runs f as the named process — the unit of crash/restart and memory
// isolation. A Process declared inside a Host body runs on that host; a Process
// outside any Host gets an implicit dedicated host named after the process (the
// common one-process-per-machine topology, so CrashHost(name) and Crash(name) both
// address it). Process stamps the running goroutine's process identity (and host,
// if it allocated an implicit one) for the dynamic extent of f and restores it on
// return, labeling the whole subtree. A process is restarted by calling Process
// again with the same name.
func Process(name string, f func()) {
	host, _ := dstCurrentNode()
	if host == 0 {
		host = internHost(name)
	}
	pid := internProc(name)
	oldH, oldP := dstSetNode(host, pid)
	defer dstSetNode(oldH, oldP)
	f()
}
