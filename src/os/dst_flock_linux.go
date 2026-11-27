// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import (
	"syscall"
	"unsafe"
)

func dstFDFlock(fd int, how int) (syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return errno, handled
	}
	file, ok := entry.backend.(*dstFile)
	if !ok {
		return syscall.ENOTSUP, true
	}
	op := how &^ syscall.LOCK_NB
	if op != syscall.LOCK_SH && op != syscall.LOCK_EX && op != syscall.LOCK_UN {
		return syscall.EINVAL, true
	}
	owner := dstFlockOwner{host: entry.host, proc: entry.proc, fd: fd}
	nonblock := how&syscall.LOCK_NB != 0
	exclusive := op == syscall.LOCK_EX
	// Coarse DPOR dependency (runtime.dstCoarseDep): flock is THE
	// cross-process mutual-exclusion decision — which contender wins a
	// LOCK_EX is outcome-determining, so both orders must be explorable.
	// Announce both the handle (read, pairing with Close) and the file node
	// (write) in a single scheduling step before enter() pins the node.
	if dstFSActive() {
		file.mu.Lock()
		peekNode := file.node
		peekLive := !file.closed && file.epoch == dstFSEpoch()
		file.mu.Unlock()
		dstCoarseDep(uintptr(unsafe.Pointer(file)), false)
		if peekLive && peekNode != nil {
			dstCoarseRecord(uintptr(unsafe.Pointer(peekNode)), true)
		}
	}
	// Pin the node for the duration of the call: an in-flight flock holds a
	// reference to the open file description on Linux, so a concurrent close
	// of the fd neither aborts the wait nor invalidates the lock state the
	// call operates on (dropClosedNode nils file.node; the pinned pointer is
	// this call's reference).
	if err := file.enter(); err != nil {
		return dstFDErr(err), true
	}
	node := file.node
	file.leave()
	for {
		if err := file.enter(); err != nil {
			if !file.flockClosedInRun() {
				return dstFDErr(err), true // a leaked dead-run handle stays refused
			}
			// The fd was closed elsewhere while we were blocked. Linux keeps
			// the wait alive on the pinned open file description and GRANTS
			// when the lock becomes available. If live MAP_SHARED mappings
			// still hold references to the open file description, the grant
			// persists in holders/flockPending until the last Munmap; if no
			// mappings remain, this in-flight call's reference drops at return
			// and releases the grant immediately.
			dstFS.mu.Lock()
			if op == syscall.LOCK_UN {
				dstMMapRegistry.mu.Lock()
				dstStorePendingLocksLocked(file, node, true, dstFlockOwner{}, false, false, false)
				dstMMapRegistry.mu.Unlock()
				node.flock.unlockNode(node, owner)
				dstFS.mu.Unlock()
				return 0, true
			}
			node.flock.removeForConversionNode(node, owner, exclusive)
			if node.flock.canLock(owner, exclusive) {
				dstMMapRegistry.mu.Lock()
				dstMMapRollLocked()
				if dstMMapLiveThroughLocked(file) > 0 {
					node.flock.lock(owner, exclusive)
					dstStorePendingLocksLocked(file, node, true, owner, true, false, false)
				} else {
					dstStorePendingLocksLocked(file, node, true, dstFlockOwner{}, false, false, false)
				}
				dstMMapRegistry.mu.Unlock()
				dstKernelRaceAcquire(unsafe.Pointer(&node.flock.raceSync))
				dstFS.mu.Unlock()
				return 0, true
			}
			if _, stillHeld := node.flock.holders[owner]; !stillHeld {
				dstMMapRegistry.mu.Lock()
				dstStorePendingLocksLocked(file, node, true, dstFlockOwner{}, false, false, false)
				dstMMapRegistry.mu.Unlock()
			}
			if nonblock {
				dstFS.mu.Unlock()
				return syscall.EWOULDBLOCK, true
			}
			wait := node.flock.waiter()
			dstFS.mu.Unlock()
			<-wait
			if dstFSActive() {
				dstCoarseDep(uintptr(unsafe.Pointer(file)), false)
				dstCoarseRecord(uintptr(unsafe.Pointer(node)), true)
			}
			continue
		}
		if op == syscall.LOCK_UN {
			node.flock.unlockNode(node, owner)
			file.leave()
			return 0, true
		}
		// Linux (fs/locks.c flock_lock_inode) removes the caller's existing
		// holding of the OTHER type before scanning for conflicts, all under one
		// spinlock: a conversion is atomic when it succeeds, and a FAILED
		// nonblocking conversion has already lost the old lock — EWOULDBLOCK
		// leaves the caller holding nothing. Retaining the old lock across a
		// failed conversion would keep executions no real kernel produces.
		node.flock.removeForConversionNode(node, owner, exclusive)
		if node.flock.canLock(owner, exclusive) {
			node.flock.lock(owner, exclusive)
			dstKernelRaceAcquire(unsafe.Pointer(&node.flock.raceSync))
			file.leave()
			return 0, true
		}
		if nonblock {
			file.leave()
			return syscall.EWOULDBLOCK, true
		}
		wait := node.flock.waiter()
		file.leave()
		<-wait
		if dstFSActive() {
			dstCoarseDep(uintptr(unsafe.Pointer(file)), false)
			dstCoarseRecord(uintptr(unsafe.Pointer(node)), true)
		}
	}
}

// flockClosedInRun reports whether the handle died by an in-run close — the
// case Linux's in-flight flock survives on the pinned description — rather
// than by its run ending (a leaked handle stays refused).
func (d *dstFile) flockClosedInRun() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return dstFSActive() && d.closed && d.epoch == dstFSEpoch()
}

// dstFlockPending records advisory locks (BSD flock and/or OFD byte-range
// locks) whose descriptor closed while shared mappings taken through the
// description were still live, released at the last unmap.
type dstFlockPending struct {
	node     *dstFSNode
	file     *dstFile
	owner    dstFlockOwner
	hasFlock bool
	hasOFD   bool
}

// dstStorePendingLocksLocked updates file.flockPending while preserving the
// sibling lock family's pending state. Caller holds dstFS.mu and
// dstMMapRegistry.mu.
func dstStorePendingLocksLocked(file *dstFile, node *dstFSNode, setFlock bool, owner dstFlockOwner, hasFlock bool, setOFD bool, hasOFD bool) {
	var next dstFlockPending
	if prev := file.flockPending.Load(); prev != nil {
		next = *prev
	}
	next.node = node
	next.file = file
	if setFlock {
		next.hasFlock = hasFlock
		if hasFlock {
			next.owner = owner
		} else {
			next.owner = dstFlockOwner{}
		}
	}
	if setOFD {
		next.hasOFD = hasOFD
	}
	if !next.hasFlock && !next.hasOFD {
		file.flockPending.Store(nil)
		return
	}
	file.flockPending.Store(&next)
}

func dstFlockReleaseFD(entry dstFDEntry, fd int) {
	file, ok := entry.backend.(*dstFile)
	if !ok {
		return
	}
	owner := dstFlockOwner{host: entry.host, proc: entry.proc, fd: fd}
	// Linux releases both BSD flocks and OFD byte-range locks with the open
	// file description's LAST reference, and every shared mapping taken through
	// the description holds one: a close under live mappings keeps the locks
	// until the last of them is unmapped (an exiting process drops both).
	// Acquire dstFS.mu before dstMMapRegistry.mu (the canonical lock order
	// shared with dstFDMmap and dstMunmapLocked) so an unmap racing this close
	// cannot deadlock or slip between the live-mapping count and flockPending.
	file.mu.Lock()
	node := file.node
	epochOk := dstFSActive() && file.epoch == dstFSEpoch()
	file.mu.Unlock()
	if node == nil || !epochOk {
		return
	}
	dstFS.mu.Lock()
	_, heldFlock := node.flock.holders[owner]
	if heldFlock {
		dstMMapRegistry.mu.Lock()
		dstMMapRollLocked()
		if dstMMapLiveThroughLocked(file) > 0 {
			dstStorePendingLocksLocked(file, node, true, owner, true, false, false)
			dstMMapRegistry.mu.Unlock()
			dstFS.mu.Unlock()
			return
		}
		dstMMapRegistry.mu.Unlock()
	}
	dstFS.mu.Unlock()
	if !heldFlock {
		return
	}
	dstCoarseDep(uintptr(unsafe.Pointer(node)), true)
	dstFS.mu.Lock()
	node.flock.unlockNode(node, owner)
	dstFS.mu.Unlock()
}

// dstFlockReleasePending releases the advisory locks a closed descriptor left
// on its description once the description's last mapping is gone.
func dstFlockReleasePending(p *dstFlockPending) {
	if p == nil || p.node == nil || (!p.hasFlock && !p.hasOFD) {
		return
	}
	dstCoarseDep(uintptr(unsafe.Pointer(p.node)), true)
	dstFS.mu.Lock()
	if p.hasFlock {
		p.node.flock.unlockNode(p.node, p.owner)
	}
	if p.hasOFD {
		p.node.ofd.unlockOwner(p.node, p.file)
	}
	dstFS.mu.Unlock()
}

func (node *dstFSNode) flockReleaseHBLocked() {
	dstKernelRaceReleaseMerge(unsafe.Pointer(&node.flock.raceSync))
}

func (s *dstFlockState) canLock(owner dstFlockOwner, exclusive bool) bool {
	for holder, holderExclusive := range s.holders {
		if holder == owner {
			continue
		}
		if exclusive || holderExclusive {
			return false
		}
	}
	return true
}

func (s *dstFlockState) lock(owner dstFlockOwner, exclusive bool) {
	if s.holders == nil {
		s.holders = make(map[dstFlockOwner]bool)
	}
	// A conversion's old holding was already dropped by removeForConversionNode (as
	// the kernel deletes before its conflict scan), so this only grants or
	// re-grants the same type — no waiter can be unblocked by it.
	s.holders[owner] = exclusive
}

// removeForConversionNode drops owner's existing holding when it is of the OTHER
// type than the requested one — the kernel deletes the old flock before its
// conflict scan, so both the successful atomic conversion and the failed
// nonblocking conversion (which loses the lock) fall out of this one step. A
// same-type request is a re-grant and removes nothing. Dropping an exclusive
// (or shared) holding can make a blocked waiter compatible, hence the broadcast.
func (s *dstFlockState) removeForConversionNode(node *dstFSNode, owner dstFlockOwner, wantExclusive bool) {
	held, ok := s.holders[owner]
	if !ok || held == wantExclusive {
		return
	}
	node.flockReleaseHBLocked()
	delete(s.holders, owner)
	s.broadcast()
}

func (s *dstFlockState) unlockNode(node *dstFSNode, owner dstFlockOwner) {
	if _, ok := s.holders[owner]; !ok {
		return
	}
	node.flockReleaseHBLocked()
	delete(s.holders, owner)
	s.broadcast()
}

func (s *dstFlockState) waiter() chan struct{} {
	if s.wait == nil {
		s.wait = make(chan struct{})
	}
	return s.wait
}

func (s *dstFlockState) broadcast() {
	if s.wait != nil {
		close(s.wait)
		s.wait = nil
	}
}
