// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import (
	"path"
	"unsafe" // go:linkname + coarse fault identities
)

// Disk faults over the per-host filesystem seam (docs/dst/faults.md "Disk faults").
// A fault is policy on the host's disk (dstFSDisk), consulted at the dstFile I/O
// choke points — never new representation, exactly the fault model the durability
// split was built for. testing/simulation drives a fault by naming a host (and, for
// the per-file form, a path) and calling the runtime relay, which lands here; os
// registers this handler from init so runtime carries no os dependency. The op codes
// are this package's contract, mirrored by the caller in testing/simulation.
const (
	diskOpFailDisk      uint32 = iota + 1 // host-disk EIO on
	diskOpHealDisk                        // host-disk EIO off
	diskOpFailFile                        // per-file EIO on (name is the host-absolute path)
	diskOpHealFile                        // per-file EIO off
	diskOpLimit                           // ENOSPC: cap the disk at arg total bytes
	diskOpUnlimit                         // remove the capacity
	diskOpSlow                            // latency: arg nanoseconds per disk-touching op (0 = none)
	diskOpCorruptFile                     // bit rot: flip one seeded bit of the file's durable image
	diskOpFailWriteback                   // writeback EIO on: syncs fail and drop dirty pages; cache-served reads/writes succeed
	diskOpHealWriteback                   // writeback EIO off
	diskOpUnslow                          // clear slow-disk latency
)

//go:linkname dstSetDiskFaultHook runtime.dstSetDiskFaultHook
func dstSetDiskFaultHook(fn func(op, host uint32, arg int64, name string))

func init() { dstSetDiskFaultHook(dstApplyDiskFaultOp) }

// dstApplyDiskFaultOp applies one disk-fault op to the named host's disk. Reached
// from testing/simulation through the runtime relay. arg is op-specific (unused by
// the EIO ops; reserved for capacity and latency faults); name is the per-file
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
		dstCoarseRecord(uintptr(unsafe.Pointer(d)), true)
		d.eio = true
	case diskOpHealDisk:
		dstCoarseRecord(uintptr(unsafe.Pointer(d)), true)
		d.eio = false
	case diskOpFailWriteback:
		dstCoarseRecord(uintptr(unsafe.Pointer(d)), true)
		d.wbFail = true
	case diskOpHealWriteback:
		dstCoarseRecord(uintptr(unsafe.Pointer(d)), true)
		d.wbFail = false
	case diskOpFailFile:
		dstCoarseRecord(dstCoarseNamespaceForHost(host), false)
		node := dstFSNodeAt(d.root, name)
		if node == nil || node.isDir || node.isDevice() {
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
		dstCoarseNodeRecord(node, true)
		if d.eioFiles == nil {
			d.eioFiles = make(map[*dstFSNode]bool)
		}
		d.eioFiles[node] = true
	case diskOpHealFile:
		dstCoarseRecord(dstCoarseNamespaceForHost(host), false)
		if node := dstFSNodeAt(d.root, name); node != nil && d.eioFiles != nil {
			dstCoarseNodeRecord(node, true)
			delete(d.eioFiles, node)
		}
	case diskOpLimit:
		dstCoarseRecord(uintptr(unsafe.Pointer(d)), true)
		dstCoarseRecord(dstCoarseNamespaceForHost(host), true)
		d.capped.Store(true)
		d.capacity = arg
	case diskOpUnlimit:
		dstCoarseRecord(uintptr(unsafe.Pointer(d)), true)
		dstCoarseRecord(dstCoarseNamespaceForHost(host), true)
		d.capped.Store(false)
	case diskOpSlow:
		dstCoarseRecord(uintptr(unsafe.Pointer(d)), true)
		dstCoarseRecord(dstCoarseNamespaceForHost(host), true)
		d.latency.Store(arg)
		if arg > 0 {
			dstDiskSlow.Store(true) // arm the global gate; reset on the next run's roll
		}
	case diskOpUnslow:
		dstCoarseRecord(uintptr(unsafe.Pointer(d)), true)
		dstCoarseRecord(dstCoarseNamespaceForHost(host), true)
		d.latency.Store(0)
	case diskOpCorruptFile:
		dstCoarseRecord(dstCoarseNamespaceForHost(host), false)
		node := dstFSNodeAt(d.root, name)
		if node == nil || node.isDir || node.isDevice() || node.synced.size == 0 {
			// Bit rot corrupts durable media. A missing target or a directory is
			// a no-op as for FailFile; a file whose durable image has no
			// allocated platter blocks (empty or all-hole sparse) has nothing
			// to rot. No fault-RNG draw happens on the no-op paths, so a
			// skipped target never shifts a later fault's stream position.
			return
		}
		dstCoarseNodeRecord(node, true)
		// One seeded byte across the file's durably allocated platter blocks,
		// one seeded bit — the canonical silent-corruption quantum. On a live
		// machine the offset addresses the durable platter (node.rot) and live
		// reads keep serving cached bytes; on a powered-off machine (after
		// CrashHost, before the next boot) no page cache exists, so the flip
		// lands directly on the platter image that recovery or offline
		// inspection will read.
		off, ok := dstBlocksSampleByteOff(node.synced.blocks, node.synced.size, dstFaultRandN)
		if !ok {
			return
		}
		shift := dstFaultRandN(8)
		bit := byte(1) << shift
		if d.down {
			dstRecordCausalEvent(dstCausalCorruptFile, 1, host, 0, 0, 0, off, int64(shift), 0, name)
			node.data[off] ^= bit
			pg := off / dstPageSize
			var buf [dstPageSize]byte
			if old := node.synced.pages[pg]; old != nil {
				buf = *old
			}
			buf[off%dstPageSize] ^= bit
			if dstBytesAllZero(buf[:]) {
				delete(node.synced.pages, pg)
			} else {
				if node.synced.pages == nil {
					node.synced.pages = make(map[int64]dstDurablePage)
				}
				node.synced.pages[pg] = &buf
			}
			return
		}
		dstRecordCausalEvent(dstCausalCorruptFile, 0, host, 0, 0, 0, off, int64(shift), 0, name)
		if node.rot == nil {
			node.rot = make(map[int64]byte)
		}
		node.rot[off] ^= bit
		if node.rot[off] == 0 {
			delete(node.rot, off) // a re-flip un-rots: the platter holds the original byte again
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
