// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import (
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Deterministic futex model: the shared (non-PRIVATE) FUTEX_WAIT-with-timeout
// / FUTEX_WAKE pair on MAP_SHARED file pages — the cross-process wait
// primitive a database parks notification waiters on. The futex word itself
// is ordinary shared page-cache memory (every mapping of the file sees the
// same bytes, stores are lockless peer atomics, exactly like hardware); the
// model supplies only what the kernel supplies: a wait-queue keyed by the
// word's identity, with the value check and the enqueue atomic against a
// wake's dequeue (the kernel's hash-bucket spinlock), so a
// store-then-FUTEX_WAKE between a waiter's load and its park is never lost.
// Wake order is FIFO in park order — deterministic under a deterministic
// schedule. Timeouts run on the bubble's virtual clock. A crashed process's
// parked waiters leave the queue at teardown (kernel exit semantics), so a
// later FUTEX_WAKE(1) can never spend its wake on a dead waiter and starve a
// live one.
//
// The word's identity is (file node, byte offset) — the model's FUTEX_SHARED
// (inode, page, offset) key — so co-located processes waiting through
// DIFFERENT mappings of the same file share one queue. An address outside
// the calling process's simulated mappings meets the fence (the raw
// boundary's convention for non-mapping addresses; the kernel would say
// EFAULT). Unaligned addresses answer EINVAL, as futex(2) does.

type dstFutexKey struct {
	node *dstFSNode
	off  int64 // file offset of the 4-byte futex word
}

type dstFutexWaiter struct {
	ch   chan struct{} // closed by wake or teardown-free (never sent on)
	host uint32        // owning host, for host-crash teardown
	proc uint32        // owning process, for crash/exit teardown
}

var dstFutex struct {
	mu    sync.Mutex
	epoch uint64
	q     map[dstFutexKey][]*dstFutexWaiter
}

func dstFutexRollLocked() {
	if e := dstFSEpoch(); e != dstFutex.epoch || dstFutex.q == nil {
		dstFutex.epoch = e
		dstFutex.q = make(map[dstFutexKey][]*dstFutexWaiter)
	}
}

// dstFutexResolve maps a futex word address to its cross-mapping identity.
// handled=false when the address is not the calling process's simulated
// mapping — the fence decides, like every non-mapping address at the raw
// boundary.
func dstFutexResolve(addr *uint32) (dstFutexKey, syscall.Errno, bool) {
	if uintptr(unsafe.Pointer(addr))%4 != 0 {
		return dstFutexKey{}, syscall.EINVAL, true
	}
	data := unsafe.Slice((*byte)(unsafe.Pointer(addr)), 4)
	entry, errno, handled := dstMMapLookupRange(data)
	if !handled || errno != 0 {
		return dstFutexKey{}, errno, handled
	}
	mapStart, _, errno := dstMMapRange(entry.data)
	if errno != 0 {
		return dstFutexKey{}, errno, true
	}
	off := entry.off + int64(uintptr(unsafe.Pointer(addr))-mapStart)
	// The kernel answers EFAULT for a futex word on an unbacked page (a
	// reservation window past EOF, a hole a truncate cut) — host-probed,
	// both WAIT and WAKE. Backing is PAGE-granular, as the kernel's is: a
	// word inside the file's last partially-used page is reachable (reads
	// zeros past i_size), one past the page is not.
	dstFS.mu.Lock()
	backed := dstFutexBackedLocked(entry.node, off)
	dstFS.mu.Unlock()
	if !backed {
		return dstFutexKey{}, syscall.EFAULT, true
	}
	return dstFutexKey{node: entry.node, off: off}, 0, true
}

// dstFutexBackedLocked reports whether the word at off sits on a backed
// page of node. Caller holds dstFS.mu.
func dstFutexBackedLocked(node *dstFSNode, off int64) bool {
	backedEnd := (int64(len(node.data)) + dstMMapPageSize - 1) &^ (dstMMapPageSize - 1)
	return off+4 <= backedEnd
}

// dstFutex backs the raw SYS_FUTEX dispatch. op is FUTEX_WAIT (0) or
// FUTEX_WAKE (1), shared form — the dispatch arm admits nothing else. The
// returned int is the syscall's r1: 0 for a completed WAIT, the woken count
// for WAKE.
func dstFutexOp(addr *uint32, op int, val uint32, timeoutNs int64, hasTimeout bool) (int, syscall.Errno, bool) {
	switch op {
	case 0:
		errno, handled := dstFutexWait(addr, val, timeoutNs, hasTimeout)
		return 0, errno, handled
	case 1:
		return dstFutexWake(addr, val)
	}
	return 0, 0, false
}

// dstFutexID is the coarse-dependency identity of a futex word: the
// backing node plus the word's file offset — exactly the queue key, so
// waits and wakes through DIFFERENT mappings announce one identity.
func dstFutexID(key dstFutexKey) uintptr {
	return uintptr(unsafe.Pointer(key.node)) + uintptr(key.off)
}

func dstFutexWait(addr *uint32, val uint32, timeoutNs int64, hasTimeout bool) (syscall.Errno, bool) {
	key, errno, handled := dstFutexResolve(addr)
	if !handled || errno != 0 {
		return errno, handled
	}
	// Coarse DPOR dependency: wait-vs-wake order on one word is the
	// lost-wake surface itself — announced pre-decision as a READ of the
	// word's identity (the wake is the write; read pairs commute). A
	// woken return ACQUIRES the waker's released history below.
	dstCoarseDep(dstFutexID(key), false, 0)
	host, proc := dstFSCurrentNode()
	w := &dstFutexWaiter{ch: make(chan struct{}), host: host, proc: proc}
	// The value check reads the word through the HARNESS page-cache view
	// (node.data), never the caller's mapping: a SUT mprotect(PROT_NONE)
	// cannot unmap it and — because the whole check+enqueue runs under
	// dstFS.mu — a truncate cannot cut it mid-section, so no load under
	// the bucket lock can ever fault (a fault teardown re-entering
	// dstFutex.mu would deadlock). The bounds re-check under dstFS.mu
	// closes the resolve-to-here window. Lock order: dstFS.mu →
	// dstFutex.mu; the bucket lock stays a leaf.
	dstFS.mu.Lock()
	if !dstFutexBackedLocked(key.node, key.off) {
		dstFS.mu.Unlock()
		return syscall.EFAULT, true
	}
	word := (*uint32)(unsafe.Pointer(&dstNodeViewLocked(key.node)[key.off]))
	dstFutex.mu.Lock()
	dstFutexRollLocked()
	// Value check and enqueue in one critical section with the wake's
	// dequeue: a peer's store+FUTEX_WAKE either sees this waiter queued
	// (and wakes it) or this load — ordered after any completed WAKE's
	// mutex release — sees the new value (EAGAIN). No third interleaving
	// exists — the lost-wake window futex(2) closes.
	if atomic.LoadUint32(word) != val {
		dstFutex.mu.Unlock()
		dstFS.mu.Unlock()
		return syscall.EAGAIN, true
	}
	dstFutex.q[key] = append(dstFutex.q[key], w)
	dstFutex.mu.Unlock()
	dstFS.mu.Unlock()
	if !hasTimeout {
		<-w.ch
		dstCoarseHB(dstFutexID(key), 1) // woken: acquire the waker's history
		return 0, true
	}
	timer := time.NewTimer(time.Duration(timeoutNs)) // bubble virtual clock
	defer timer.Stop()
	select {
	case <-w.ch:
		dstCoarseHB(dstFutexID(key), 1) // woken: acquire the waker's history
		return 0, true
	case <-timer.C:
	}
	// Timed out. If a wake dequeued this waiter first, the wake wins (it
	// spent a slot on us): report woken, not ETIMEDOUT.
	dstFutex.mu.Lock()
	removed := dstFutexRemoveLocked(key, w)
	dstFutex.mu.Unlock()
	if removed {
		return syscall.ETIMEDOUT, true
	}
	dstCoarseHB(dstFutexID(key), 1) // wake won the race: acquire its history
	return 0, true
}

func dstFutexWake(addr *uint32, n uint32) (int, syscall.Errno, bool) {
	key, errno, handled := dstFutexResolve(addr)
	if !handled || errno != 0 {
		return 0, errno, handled
	}
	// Coarse DPOR dependency: the wake writes the word's identity and
	// RELEASES this goroutine's history to whichever waiter it wakes.
	dstCoarseDep(dstFutexID(key), true, 2)
	dstFutex.mu.Lock()
	defer dstFutex.mu.Unlock()
	dstFutexRollLocked()
	q := dstFutex.q[key]
	// The kernel wakes before it checks the requested count (++ret >=
	// nr_wake), so val <= 0 still wakes one waiter — host-probed for both
	// val=0 and negative val (which arrives sign-extended; int32 recovers
	// it). Model the same floor.
	limit := int(int32(n))
	if limit < 1 {
		limit = 1
	}
	woken := 0
	for len(q) > 0 && woken < limit {
		close(q[0].ch)
		q = q[1:]
		woken++
	}
	if len(q) == 0 {
		delete(dstFutex.q, key)
	} else {
		dstFutex.q[key] = q
	}
	return woken, 0, true
}

// dstFutexRemoveLocked removes w from key's queue; false means a wake (or
// teardown) already consumed it. Caller holds dstFutex.mu.
func dstFutexRemoveLocked(key dstFutexKey, w *dstFutexWaiter) bool {
	q := dstFutex.q[key]
	for i, x := range q {
		if x == w {
			q = append(q[:i:i], q[i+1:]...)
			if len(q) == 0 {
				delete(dstFutex.q, key)
			} else {
				dstFutex.q[key] = q
			}
			return true
		}
	}
	return false
}

// dstFutexTeardownProc drops every waiter the dead process parked, exactly
// as the kernel unhashes a dying task's futex waiters: a later FUTEX_WAKE
// neither counts them nor spends wake slots on them. The dead goroutine
// never runs again, so closing its channel is unobservable.
func dstFutexTeardownProc(proc uint32) {
	dstFutexTeardown(func(w *dstFutexWaiter) bool { return w.proc == proc })
}

// dstFutexTeardownHost drops every waiter parked by any process of a
// power-lost host: a reboot destroys the kernel's in-memory futex queues
// outright, while the durable file node — the queue key — survives the
// restore, so without this a rebooted host's fresh waiter would sit behind
// a dead one and a FUTEX_WAKE(1) would starve it.
func dstFutexTeardownHost(host uint32) {
	dstFutexTeardown(func(w *dstFutexWaiter) bool { return w.host == host })
}

func dstFutexTeardown(dead func(*dstFutexWaiter) bool) {
	dstFutex.mu.Lock()
	defer dstFutex.mu.Unlock()
	dstFutexRollLocked()
	for key, q := range dstFutex.q {
		kept := q[:0]
		for _, w := range q {
			if dead(w) {
				close(w.ch)
				continue
			}
			kept = append(kept, w)
		}
		if len(kept) == 0 {
			delete(dstFutex.q, key)
		} else {
			dstFutex.q[key] = kept
		}
	}
}
