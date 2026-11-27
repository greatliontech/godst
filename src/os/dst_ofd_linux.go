// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import (
	"cmp"
	"slices"
	"syscall"
	"unsafe"
)

const (
	dstFcntlOFDGetlk  = 36
	dstFcntlOFDSetlk  = 37
	dstFcntlOFDSetlkw = 38

	dstOffsetMax int64 = 1<<63 - 1
)

func dstFDOFDFlock(fd int, cmd int, lk *syscall.Flock_t) (syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return errno, handled
	}
	file, ok := entry.backend.(*dstFile)
	if !ok {
		if lk == nil {
			return syscall.EFAULT, true
		}
		return syscall.ENOTSUP, true
	}
	if lk == nil {
		return syscall.EFAULT, true
	}
	if cmd != dstFcntlOFDGetlk && cmd != dstFcntlOFDSetlk && cmd != dstFcntlOFDSetlkw {
		return syscall.EINVAL, true
	}

	// Coarse DPOR dependency: OFD byte-range locks coordinate cross-process
	// reader/writer ownership. F_OFD_GETLK reads node lock state (commuting with
	// other GETLK calls while racing SETLK/SETLKW); SETLK/SETLKW mutate it.
	if dstFSActive() {
		file.mu.Lock()
		peekNode := file.node
		peekLive := !file.closed && file.epoch == dstFSEpoch()
		file.mu.Unlock()
		dstCoarseDep(uintptr(unsafe.Pointer(file)), false)
		if peekLive && peekNode != nil {
			dstCoarseRecord(uintptr(unsafe.Pointer(peekNode)), cmd != dstFcntlOFDGetlk)
		}
	}

	// Snapshot the caller's Flock_t outside KernelMutex so TSan/DPOR observe
	// caller memory accesses normally, then validate in Linux fs/locks.c order:
	// flock_to_posix_lock (whence, range, type) -> l_pid != 0 -> check_fmode_for_setlk.
	req := *lk
	if err := file.enter(); err != nil {
		return dstFDErr(err), true
	}
	node := file.node
	if node == nil {
		file.leave()
		return syscall.EBADF, true
	}
	var base int64
	switch req.Whence {
	case 0: // SEEK_SET
		base = 0
	case 1: // SEEK_CUR
		if node.isDir {
			base = int64(file.dirpos)
		} else {
			base = file.off
		}
	case 2: // SEEK_END
		base = int64(len(node.data))
	default:
		file.leave()
		return syscall.EINVAL, true
	}
	start, end, rerr := dstResolveOFDRange(base, req.Start, req.Len)
	if rerr != 0 {
		file.leave()
		return rerr, true
	}
	switch req.Type {
	case syscall.F_RDLCK, syscall.F_WRLCK, syscall.F_UNLCK:
	default:
		file.leave()
		return syscall.EINVAL, true
	}
	if req.Pid != 0 {
		file.leave()
		return syscall.EINVAL, true
	}
	if cmd != dstFcntlOFDGetlk {
		if req.Type == syscall.F_RDLCK && !file.rd {
			file.leave()
			return syscall.EBADF, true
		}
		if req.Type == syscall.F_WRLCK && !file.wr {
			file.leave()
			return syscall.EBADF, true
		}
	}

	openSeq := file.openSeq
	exclusive := req.Type == syscall.F_WRLCK

	if cmd == dstFcntlOFDGetlk {
		var c dstOFDLock
		var found bool
		if req.Type == syscall.F_UNLCK {
			// Linux >= 6.6 posix_test_locks_conflict: F_UNLCK in F_OFD_GETLK
			// inspects overlapping locks held by the SAME open file description.
			c, found = node.ofd.firstOwnOverlap(file, start, end)
		} else {
			c, found = node.ofd.firstConflict(file, start, end, exclusive)
		}
		dstKernelRaceAcquire(unsafe.Pointer(&node.ofd.raceSync))
		file.leave()
		if !found {
			lk.Type = syscall.F_UNLCK
			return 0, true
		}
		out := req
		if c.exclusive {
			out.Type = syscall.F_WRLCK
		} else {
			out.Type = syscall.F_RDLCK
		}
		out.Whence = 0
		out.Start = c.start
		if c.end == dstOffsetMax {
			out.Len = 0
		} else {
			out.Len = c.end - c.start + 1
		}
		out.Pid = -1
		*lk = out
		return 0, true
	}

	if req.Type == syscall.F_UNLCK {
		node.ofd.apply(node, file, openSeq, start, end, syscall.F_UNLCK)
		file.leave()
		return 0, true
	}

	// Unlike BSD flock, posix_lock_inode checks for conflicts BEFORE splitting or
	// converting the caller's existing intervals: a failed F_OFD_SETLK (or a
	// blocked F_OFD_SETLKW) retains any existing locks held by this description.
	if _, conflict := node.ofd.firstConflict(file, start, end, exclusive); !conflict {
		node.ofd.apply(node, file, openSeq, start, end, req.Type)
		dstKernelRaceAcquire(unsafe.Pointer(&node.ofd.raceSync))
		file.leave()
		return 0, true
	}
	if cmd == dstFcntlOFDSetlk {
		file.leave()
		return syscall.EAGAIN, true
	}

	wait := node.ofd.waiter()
	file.leave()
	for {
		<-wait
		if dstFSActive() {
			dstCoarseDep(uintptr(unsafe.Pointer(file)), false)
			dstCoarseRecord(uintptr(unsafe.Pointer(node)), true)
		}
		if err := file.enter(); err != nil {
			if !file.flockClosedInRun() {
				return dstFDErr(err), true
			}
			// Closed while blocked: Linux keeps the F_OFD_SETLKW wait alive on
			// the pinned struct file and grants when conflicts clear; the grant
			// persists only if live MAP_SHARED mappings still reference the
			// description, and releases immediately at syscall return otherwise.
			dstFS.mu.Lock()
			if _, conflict := node.ofd.firstConflict(file, start, end, exclusive); !conflict {
				dstMMapRegistry.mu.Lock()
				dstMMapRollLocked()
				if dstMMapLiveThroughLocked(file) > 0 {
					node.ofd.apply(node, file, openSeq, start, end, req.Type)
					dstStorePendingLocksLocked(file, node, false, dstFlockOwner{}, false, true, node.ofd.hasOwner(file))
				} else {
					node.ofd.unlockOwner(node, file)
					dstStorePendingLocksLocked(file, node, false, dstFlockOwner{}, false, true, false)
				}
				dstMMapRegistry.mu.Unlock()
				dstKernelRaceAcquire(unsafe.Pointer(&node.ofd.raceSync))
				dstFS.mu.Unlock()
				return 0, true
			}
			wait = node.ofd.waiter()
			dstFS.mu.Unlock()
			continue
		}
		if _, conflict := node.ofd.firstConflict(file, start, end, exclusive); !conflict {
			node.ofd.apply(node, file, openSeq, start, end, req.Type)
			dstKernelRaceAcquire(unsafe.Pointer(&node.ofd.raceSync))
			file.leave()
			return 0, true
		}
		wait = node.ofd.waiter()
		file.leave()
	}
}

// dstOFDReleaseClosedFile releases OFD byte-range locks held by a closing open
// file description, or defers release via file.flockPending when live
// MAP_SHARED mappings still reference the description. Invoked from
// dstDropClosedNode (every *dstFile close funnel, independent of whether any
// virtual fd remains registered in dstFDRegistry).
func dstOFDReleaseClosedFile(file *dstFile) {
	if file == nil {
		return
	}
	file.mu.Lock()
	node := file.node
	epochOk := file.closed && dstFSActive() && file.epoch == dstFSEpoch()
	file.mu.Unlock()
	if node == nil || !epochOk {
		return
	}
	dstFS.mu.Lock()
	heldOFD := node.ofd.hasOwner(file)
	if heldOFD {
		dstMMapRegistry.mu.Lock()
		dstMMapRollLocked()
		if dstMMapLiveThroughLocked(file) > 0 {
			dstStorePendingLocksLocked(file, node, false, dstFlockOwner{}, false, true, true)
			dstMMapRegistry.mu.Unlock()
			dstFS.mu.Unlock()
			return
		}
		dstMMapRegistry.mu.Unlock()
	}
	dstFS.mu.Unlock()
	if !heldOFD {
		return
	}
	dstCoarseDep(uintptr(unsafe.Pointer(node)), true)
	dstFS.mu.Lock()
	node.ofd.unlockOwner(node, file)
	dstFS.mu.Unlock()
}

// dstResolveOFDRange converts (base, l_start, l_len) into an inclusive byte
// interval [start, end] matching Linux's flock_to_posix_lock (fs/locks.c).
// base is always >= 0 (0, file.off/dirpos, or file size).
func dstResolveOFDRange(base, lstart, llen int64) (start, end int64, errno syscall.Errno) {
	if lstart > 0 && base > dstOffsetMax-lstart {
		return 0, 0, syscall.EOVERFLOW
	}
	start = base + lstart
	if start < 0 {
		return 0, 0, syscall.EINVAL
	}
	end = dstOffsetMax
	if llen > 0 {
		if llen-1 > dstOffsetMax-start {
			return 0, 0, syscall.EOVERFLOW
		}
		end = start + llen - 1
	} else if llen < 0 {
		if start+llen < 0 {
			return 0, 0, syscall.EINVAL
		}
		end = start - 1
		start += llen
	}
	return start, end, 0
}

func (node *dstFSNode) ofdReleaseHBLocked() {
	dstKernelRaceReleaseMerge(unsafe.Pointer(&node.ofd.raceSync))
}

func (s *dstOFDState) hasOwner(owner *dstFile) bool {
	if owner == nil {
		return false
	}
	for i := range s.locks {
		if s.locks[i].owner == owner {
			return true
		}
	}
	return false
}

func (s *dstOFDState) firstOwnOverlap(owner *dstFile, start, end int64) (dstOFDLock, bool) {
	for i := range s.locks {
		l := s.locks[i]
		if l.owner != owner {
			continue
		}
		if l.end < start || end < l.start {
			continue
		}
		return l, true
	}
	return dstOFDLock{}, false
}

func (s *dstOFDState) firstConflict(owner *dstFile, start, end int64, exclusive bool) (dstOFDLock, bool) {
	for i := range s.locks {
		l := s.locks[i]
		if l.owner == owner {
			continue
		}
		if l.end < start || end < l.start {
			continue
		}
		if exclusive || l.exclusive {
			return l, true
		}
	}
	return dstOFDLock{}, false
}

func (s *dstOFDState) apply(node *dstFSNode, owner *dstFile, ownerSeq uint64, start, end int64, typ int16) {
	var others []dstOFDLock
	var mine []dstOFDLock
	var released bool

	for _, l := range s.locks {
		if l.owner != owner {
			others = append(others, l)
			continue
		}
		if l.end < start || end < l.start {
			mine = append(mine, l)
			continue
		}
		if typ == syscall.F_UNLCK || (l.exclusive && typ == syscall.F_RDLCK) {
			released = true
		}
		if l.start < start {
			mine = append(mine, dstOFDLock{
				owner:     owner,
				ownerSeq:  ownerSeq,
				start:     l.start,
				end:       start - 1,
				exclusive: l.exclusive,
			})
		}
		if l.end > end {
			mine = append(mine, dstOFDLock{
				owner:     owner,
				ownerSeq:  ownerSeq,
				start:     end + 1,
				end:       l.end,
				exclusive: l.exclusive,
			})
		}
	}

	if typ != syscall.F_UNLCK {
		mine = append(mine, dstOFDLock{
			owner:     owner,
			ownerSeq:  ownerSeq,
			start:     start,
			end:       end,
			exclusive: typ == syscall.F_WRLCK,
		})
	}
	if len(mine) > 1 {
		slices.SortFunc(mine, func(a, b dstOFDLock) int {
			return cmp.Compare(a.start, b.start)
		})
		merged := mine[:1]
		for _, cur := range mine[1:] {
			prev := &merged[len(merged)-1]
			if prev.exclusive == cur.exclusive && prev.end < dstOffsetMax && prev.end+1 == cur.start {
				prev.end = cur.end
			} else {
				merged = append(merged, cur)
			}
		}
		mine = merged
	}

	out := make([]dstOFDLock, 0, len(others)+len(mine))
	out = append(out, others...)
	out = append(out, mine...)
	slices.SortFunc(out, func(a, b dstOFDLock) int {
		if c := cmp.Compare(a.ownerSeq, b.ownerSeq); c != 0 {
			return c
		}
		return cmp.Compare(a.start, b.start)
	})
	if len(out) == 0 {
		s.locks = nil
	} else {
		s.locks = out
	}

	if released {
		node.ofdReleaseHBLocked()
		s.broadcast()
	}
}

func (s *dstOFDState) unlockOwner(node *dstFSNode, owner *dstFile) {
	if owner == nil || len(s.locks) == 0 {
		return
	}
	out := s.locks[:0]
	var removed bool
	for _, l := range s.locks {
		if l.owner == owner {
			removed = true
			continue
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		s.locks = nil
	} else {
		s.locks = out
	}
	if removed {
		node.ofdReleaseHBLocked()
		s.broadcast()
	}
}

func (s *dstOFDState) waiter() chan struct{} {
	if s.wait == nil {
		s.wait = make(chan struct{})
	}
	return s.wait
}

func (s *dstOFDState) broadcast() {
	if s.wait != nil {
		close(s.wait)
		s.wait = nil
	}
}
