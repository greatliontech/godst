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
	// Coarse DPOR dependency (runtime.dstCoarseDep): flock is THE
	// cross-process mutual-exclusion decision — which contender wins a
	// LOCK_EX is outcome-determining, so both orders must be explorable.
	// Announced pre-decision as a write-conflict on the file node; an
	// unlock also RELEASES its happens-before history (the next grantee
	// acquires it below), mirroring the runtime mutex hooks' shape.
	flockID := uintptr(unsafe.Pointer(node))
	if op == syscall.LOCK_UN {
		dstCoarseDep(flockID, true, 2)
	} else {
		dstCoarseDep(flockID, true, 0)
	}
	for {
		if err := file.enter(); err != nil {
			if !file.flockClosedInRun() {
				return dstFDErr(err), true // a leaked dead-run handle stays refused
			}
			// The fd was closed elsewhere while we were blocked. Linux keeps
			// the wait alive on the pinned description and GRANTS when the
			// lock becomes available; the description's last reference — this
			// in-flight call — drops at return, releasing the grant at once.
			// So: grant without recording (the owner's prior locks were
			// already released by the close), and LOCK_UN is a no-op success.
			dstFS.mu.Lock()
			if op == syscall.LOCK_UN || node.flock.canLock(owner, exclusive) {
				dstFS.mu.Unlock()
				if op != syscall.LOCK_UN {
					dstCoarseHB(flockID, 1) // granted: acquire the released history
				}
				return 0, true
			}
			if nonblock {
				dstFS.mu.Unlock()
				return syscall.EWOULDBLOCK, true
			}
			wait := node.flock.waiter()
			dstFS.mu.Unlock()
			<-wait
			continue
		}
		if op == syscall.LOCK_UN {
			node.flock.unlock(owner)
			file.leave()
			return 0, true
		}
		// Linux (fs/locks.c flock_lock_inode) removes the caller's existing
		// holding of the OTHER type before scanning for conflicts, all under one
		// spinlock: a conversion is atomic when it succeeds, and a FAILED
		// nonblocking conversion has already lost the old lock — EWOULDBLOCK
		// leaves the caller holding nothing. Retaining the old lock across a
		// failed conversion would keep executions no real kernel produces.
		node.flock.removeForConversion(owner, exclusive)
		if node.flock.canLock(owner, exclusive) {
			node.flock.lock(owner, exclusive)
			file.leave()
			dstCoarseHB(flockID, 1) // granted: acquire the released history
			return 0, true
		}
		if nonblock {
			file.leave()
			return syscall.EWOULDBLOCK, true
		}
		wait := node.flock.waiter()
		file.leave()
		<-wait
	}
}

// flockClosedInRun reports whether the handle died by an in-run close — the
// case Linux's in-flight flock survives on the pinned description — rather
// than by its run ending (a leaked handle stays refused).
func (d *dstFile) flockClosedInRun() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed && d.epoch == dstFSEpoch()
}

func dstFlockReleaseFD(entry dstFDEntry, fd int) {
	file, ok := entry.backend.(*dstFile)
	if !ok {
		return
	}
	// Coarse DPOR dependency: a close (or exit) RELEASING a held flock is
	// as outcome-determining as an explicit LOCK_UN — a contender's
	// nonblocking attempt lands on either side of it. Announced with the
	// same write-conflict + release edge, before this function's own
	// mutation (the announce yields, so it precedes the locks it is
	// re-taken under). NARROWED to closes whose owner actually holds a
	// lock: announcing every close would put a write-conflict on every
	// file teardown and combinatorially inflate exhaustive explorations
	// that never flock at all (measured: an unbudgeted explore test went
	// from seconds to minutes).
	file.mu.Lock()
	node := file.node
	file.mu.Unlock()
	if node != nil {
		dstFS.mu.Lock()
		_, held := node.flock.holders[dstFlockOwner{host: entry.host, proc: entry.proc, fd: fd}]
		dstFS.mu.Unlock()
		if held {
			dstCoarseDep(uintptr(unsafe.Pointer(node)), true, 2)
		}
	}
	if node == nil {
		// The node was already dropped (a close raced the registry sweep,
		// or nilled during the announce's yield): the earlier drop's own
		// release path covered the flock state — nothing left to unlock.
		return
	}
	file.mu.Lock()
	dstFS.mu.Lock()
	node.flock.unlock(dstFlockOwner{host: entry.host, proc: entry.proc, fd: fd})
	dstFS.mu.Unlock()
	file.mu.Unlock()
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
	// A conversion's old holding was already dropped by removeForConversion (as
	// the kernel deletes before its conflict scan), so this only grants or
	// re-grants the same type — no waiter can be unblocked by it.
	s.holders[owner] = exclusive
}

// removeForConversion drops owner's existing holding when it is of the OTHER
// type than the requested one — the kernel deletes the old flock before its
// conflict scan, so both the successful atomic conversion and the failed
// nonblocking conversion (which loses the lock) fall out of this one step. A
// same-type request is a re-grant and removes nothing. Dropping an exclusive
// (or shared) holding can make a blocked waiter compatible, hence the broadcast.
func (s *dstFlockState) removeForConversion(owner dstFlockOwner, wantExclusive bool) {
	held, ok := s.holders[owner]
	if !ok || held == wantExclusive {
		return
	}
	delete(s.holders, owner)
	s.broadcast()
}

func (s *dstFlockState) unlock(owner dstFlockOwner) {
	if _, ok := s.holders[owner]; !ok {
		return
	}
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
