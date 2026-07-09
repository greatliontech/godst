// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import "syscall"

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
	for {
		if err := file.enter(); err != nil {
			return dstFDErr(err), true
		}
		if op == syscall.LOCK_UN {
			file.node.flock.unlock(owner)
			file.leave()
			return 0, true
		}
		exclusive := op == syscall.LOCK_EX
		// Linux (fs/locks.c flock_lock_inode) removes the caller's existing
		// holding of the OTHER type before scanning for conflicts, all under one
		// spinlock: a conversion is atomic when it succeeds, and a FAILED
		// nonblocking conversion has already lost the old lock — EWOULDBLOCK
		// leaves the caller holding nothing. Retaining the old lock across a
		// failed conversion would keep executions no real kernel produces.
		file.node.flock.removeForConversion(owner, exclusive)
		if file.node.flock.canLock(owner, exclusive) {
			file.node.flock.lock(owner, exclusive)
			file.leave()
			return 0, true
		}
		if nonblock {
			file.leave()
			return syscall.EWOULDBLOCK, true
		}
		wait := file.node.flock.waiter()
		file.leave()
		<-wait
	}
}

func dstFlockReleaseFD(entry dstFDEntry, fd int) {
	file, ok := entry.backend.(*dstFile)
	if !ok {
		return
	}
	file.mu.Lock()
	dstFS.mu.Lock()
	file.node.flock.unlock(dstFlockOwner{host: entry.host, proc: entry.proc, fd: fd})
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
