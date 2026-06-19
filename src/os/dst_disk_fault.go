// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import (
	"path"
	_ "unsafe" // for go:linkname
)

// Disk faults over the per-host filesystem seam (docs/dst/faults.md "Disk faults").
// A fault is policy on the host's disk (dstFSDisk), consulted at the dstFile I/O
// choke points — never new representation, exactly the fault model the durability
// split was built for. testing/simulation drives a fault by naming a host (and, for
// the per-file form, a path) and calling the runtime relay, which lands here; os
// registers this handler from init so runtime carries no os dependency. The op codes
// are this package's contract, mirrored by the caller in testing/simulation.
const (
	diskOpFailDisk uint32 = iota + 1 // host-disk EIO on
	diskOpHealDisk                   // host-disk EIO off
	diskOpFailFile                   // per-file EIO on (name is the host-absolute path)
	diskOpHealFile                   // per-file EIO off
	diskOpLimit                      // ENOSPC: cap the disk at arg total bytes
	diskOpUnlimit                    // remove the capacity
	diskOpSlow                       // latency: arg nanoseconds per disk-touching op (0 = none)
)

//go:linkname dstSetDiskFaultHook runtime.dstSetDiskFaultHook
func dstSetDiskFaultHook(fn func(op, host uint32, arg int64, name string))

func init() { dstSetDiskFaultHook(dstApplyDiskFaultOp) }

// dstApplyDiskFaultOp applies one disk-fault op to the named host's disk. Reached
// from testing/simulation through the runtime relay. arg is op-specific (unused by
// the EIO ops; the capacity / latency the later chunks add); name is the per-file
// target. A call outside a run is a no-op (no disk to fault). Per-file faults key on
// the resolved node, not the path, so the fault follows the file across a rename and
// a target that does not (yet) exist is a no-op.
func dstApplyDiskFaultOp(op, host uint32, arg int64, name string) {
	if !dstFSActive() {
		return
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	d := dstFSDiskForHost(host)
	switch op {
	case diskOpFailDisk:
		d.eio = true
	case diskOpHealDisk:
		d.eio = false
	case diskOpFailFile:
		node := dstFSNodeAt(d.root, name)
		if node == nil || node.isDir {
			// Per-file EIO is scoped to a regular file's blocks; a directory (and
			// the root, which an empty path resolves to) is a no-op. This is a
			// real scoping choice, not a vacuous one: a directory handle's Sync
			// does reach diskEIO (sync has no isDir short-circuit, unlike read /
			// write, which return EISDIR first), so without this guard FailFile on
			// a directory would EIO its fsync. A whole-disk failure (FailDisk)
			// still fails a directory's fsync — a dead disk cannot persist
			// anything — but a single targeted file does not.
			return
		}
		if d.eioFiles == nil {
			d.eioFiles = make(map[*dstFSNode]bool)
		}
		d.eioFiles[node] = true
	case diskOpHealFile:
		if node := dstFSNodeAt(d.root, name); node != nil && d.eioFiles != nil {
			delete(d.eioFiles, node)
		}
	case diskOpLimit:
		d.capped = true
		d.capacity = arg
	case diskOpUnlimit:
		d.capped = false
	case diskOpSlow:
		d.latency.Store(arg)
		if arg > 0 {
			dstDiskSlow.Store(true) // arm the global gate; reset on the next run's roll
		}
	}
}

// dstFSDiskForHost returns the named host's disk, creating it (with the pre-seeded
// /tmp) on first touch — the explicit-host counterpart of dstFSDiskHere, so a fault
// can target a host that has not yet done any I/O. Caller holds dstFS.mu.
func dstFSDiskForHost(host uint32) *dstFSDisk {
	d := dstFS.disks[host]
	if d == nil {
		d = newDstFSDisk()
		dstFS.disks[host] = d
	}
	return d
}

// dstFSNodeAt resolves a host-absolute path against root and returns the target
// node, or nil if it does not exist. Unlike dstFSResolve it takes an explicit root
// (a fault targets a host that may not be the caller's) and no process cwd (the
// harness names absolute paths). Caller holds dstFS.mu.
func dstFSNodeAt(root *dstFSNode, name string) *dstFSNode {
	clean := path.Clean("/" + name)
	if clean == "/" {
		return root
	}
	dir := root
	rest := clean[1:]
	for {
		i := 0
		for i < len(rest) && rest[i] != '/' {
			i++
		}
		next := dir.entries[rest[:i]]
		if next == nil {
			return nil
		}
		if i == len(rest) {
			return next
		}
		if !next.isDir {
			return nil
		}
		dir = next
		rest = rest[i+1:]
	}
}
