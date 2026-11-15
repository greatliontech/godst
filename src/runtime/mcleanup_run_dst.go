// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

var dstCleanupsDiscarded atomic.Uint64

// enqueue queues a single cleanup for execution.
//
// Called by the sweeper, and only the sweeper.
func (q *cleanupQueue) enqueue(c cleanupFn) {
	dstSt := dstTakeStagedSpecialStamps()
	mp := acquirem()
	pp := mp.p.ptr()
	if dstActive() && dstSt.epoch != dstRunEpoch.Load() {
		lock(&finlock)
		if dstActive() {
			q.dstDeferCleanup(c, dstSt)
			unlock(&finlock)
			pp.cleanupsQueued++
			dstCleanupRunExecuted.Add(1)
			releasem(mp)
			return
		}
		unlock(&finlock)
	}
	b := pp.cleanups
	if b == nil {
		if q.flushed.Load() {
			q.flushed.Store(false)
		}
		b = (*cleanupBlock)(q.free.pop())
		if b == nil {
			b = (*cleanupBlock)(persistentalloc(cleanupBlockSize, tagAlign, &memstats.gcMiscSys))
			dstCleanupSlabAttach(b)
			for {
				next := (*cleanupBlock)(q.all.Load())
				b.alllink = next
				if q.all.CompareAndSwap(unsafe.Pointer(next), unsafe.Pointer(b)) {
					break
				}
			}
		}
		pp.cleanups = b
	}
	full := b.enqueue(c)
	if pp.dstCleanupSlabBlock != b {
		pp.dstCleanupSlabBlock = b
		pp.dstCleanupSlab = dstCleanupSlabFor(b)
	}
	pp.dstCleanupSlab[b.n-1] = dstSt
	if full {
		q.full.push(&b.lfnode)
		pp.cleanups = nil
		q.addWork(1)
	}
	pp.cleanupsQueued++
	releasem(mp)
}

var (
	dstDeferredCleanups       lfstack
	dstDeferredCleanupBlocks  uint64
	dstDeferredCleanupPartial *cleanupBlock
)

func (q *cleanupQueue) dstDeferCleanup(c cleanupFn, st dstCallbackStamp) {
	b := dstDeferredCleanupPartial
	if b == nil {
		b = (*cleanupBlock)(q.free.pop())
		if b == nil {
			b = (*cleanupBlock)(persistentalloc(cleanupBlockSize, tagAlign, &memstats.gcMiscSys))
			dstCleanupSlabAttach(b)
			for {
				next := (*cleanupBlock)(q.all.Load())
				b.alllink = next
				if q.all.CompareAndSwap(unsafe.Pointer(next), unsafe.Pointer(b)) {
					break
				}
			}
		}
		dstDeferredCleanupPartial = b
	}
	full := b.enqueue(c)
	dstCleanupSlabFor(b)[b.n-1] = st
	if full {
		dstDeferredCleanupBlocks++
		dstDeferredCleanups.push(&b.lfnode)
		dstDeferredCleanupPartial = nil
	}
}

var dstCleanupRunBaseQueued, dstCleanupRunExecuted atomic.Uint64

// runCleanups is the entrypoint for all cleanup-running goroutines.
func runCleanups() {
	for {
		if gcCleanups.dstParkWorkerIfBlocked() {
			continue
		}
		b := gcCleanups.dequeue()
		runCleanupBlock(b, false)
	}
}

func runCleanupBlock(b *cleanupBlock, onDrain bool) {
	if raceenabled {
		racefingo()
	}

	if !onDrain {
		gcCleanups.beginRunningCleanups()
	}
	slab := dstCleanupSlabFor(b)
	var executed uint64
	for i := 0; i < int(b.n); i++ {
		for !onDrain && dstCallbackWorkersBlocked() {
			gcCleanups.endRunningCleanups()
			gcCleanups.dstParkWorkerIfBlocked()
			gcCleanups.beginRunningCleanups()
		}
		c := b.cleanups[i]
		b.cleanups[i] = cleanupFn{}

		var racectx uintptr
		if raceenabled {
			racerelease(unsafe.Pointer(c.arg))
			racectx = raceEnterNewCtx()
			raceacquire(unsafe.Pointer(c.arg))
		}

		st := slab[i]
		ownerHost, invoked := dstCallbackOwnerHost(st)
		if onDrain {
			if invoked {
				gcCleanups.executed.Add(1)
			} else {
				dstCleanupsDiscarded.Add(1)
			}
			if dstActive() {
				dstCleanupRunExecuted.Add(1)
			}
		}
		if invoked {
			gp := getg()
			oldHost, oldProc, oldSlot, oldPid := gp.dstHost, gp.dstProc, gp.dstProcSlot, gp.dstPid
			if onDrain && (st.pid > 0 || ownerHost != 0) {
				gp.dstHost = ownerHost
				gp.dstProc = st.proc
				gp.dstProcSlot = st.slot
				if st.pid > 0 {
					gp.dstPid = st.pid
				} else {
					gp.dstPid = int32(dstSimPID)
				}
			}
			c.call(c.fn, c.arg)
			gp.dstHost, gp.dstProc, gp.dstProcSlot, gp.dstPid = oldHost, oldProc, oldSlot, oldPid
			executed++
		}

		if raceenabled {
			raceRestoreCtx(racectx)
		}
	}
	if !onDrain {
		gcCleanups.endRunningCleanups()
		gcCleanups.executed.Add(int64(executed))
		dstCleanupsDiscarded.Add(int64(uint64(b.n) - executed))
	}

	atomic.Store(&b.n, 0)
	gcCleanups.free.push(&b.lfnode)
}

func dstDrainCleanups() {
	dstSortCleanupsBySeq()
	for gcCleanups.tryTakeWork() {
		b := (*cleanupBlock)(gcCleanups.full.pop())
		dstDrainingCleanup = b
		runCleanupBlock(b, true)
		dstDrainingCleanup = nil
	}
}

func dstSortCleanupsBySeq() {
	var blocks []*cleanupBlock
	for gcCleanups.tryTakeWork() {
		blocks = append(blocks, (*cleanupBlock)(gcCleanups.full.pop()))
	}
	if len(blocks) == 0 {
		return
	}
	n := 0
	for _, b := range blocks {
		n += int(b.n)
	}
	all := make([]dstCleanupWithStamp, 0, n)
	slabs := make([]*[len(cleanupBlock{}.cleanups)]dstCallbackStamp, len(blocks))
	for bi, b := range blocks {
		slab := dstCleanupSlabFor(b)
		slabs[bi] = slab
		for i := uint32(0); i < b.n; i++ {
			all = append(all, dstCleanupWithStamp{c: b.cleanups[i], st: slab[i]})
		}
	}
	dstSortByStampSeq(all, func(p *dstCleanupWithStamp) uintptr { return p.st.seq })
	k := 0
	for bi, b := range blocks {
		slab := slabs[bi]
		for i := uint32(0); i < b.n; i++ {
			b.cleanups[i] = all[k].c
			slab[i] = all[k].st
			k++
		}
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		gcCleanups.full.push(&blocks[i].lfnode)
	}
	gcCleanups.addWork(len(blocks))
}

var dstDrainingCleanup *cleanupBlock

func dstDiscardQueuedCleanups() {
	if b := dstDrainingCleanup; b != nil {
		dstDrainingCleanup = nil
		dstDiscardCleanupBlock(b)
	}
	for gcCleanups.tryTakeWork() {
		b := (*cleanupBlock)(gcCleanups.full.pop())
		dstDiscardCleanupBlock(b)
	}
}

func dstDiscardCleanupBlock(b *cleanupBlock) {
	n := atomic.Load(&b.n)
	var discarded uint64
	for i := 0; i < int(n); i++ {
		if b.cleanups[i].call != nil {
			discarded++
		}
		b.cleanups[i] = cleanupFn{}
	}
	dstCleanupsDiscarded.Add(int64(discarded))
	if dstActive() {
		dstCleanupRunExecuted.Add(int64(discarded))
	}
	atomic.Store(&b.n, 0)
	gcCleanups.free.push(&b.lfnode)
}

func dstDeferPreBubbleCleanups() {
	for gcCleanups.tryTakeWork() {
		b := (*cleanupBlock)(gcCleanups.full.pop())
		dstDeferredCleanupBlocks++
		dstDeferredCleanups.push(&b.lfnode)
	}
}

func dstReleaseDeferredCleanups() {
	if gcCleanups.needG() {
		gcCleanups.createGs()
	}
	lock(&finlock)
	if b := dstDeferredCleanupPartial; b != nil {
		dstDeferredCleanupPartial = nil
		if b.empty() {
			gcCleanups.free.push(&b.lfnode)
		} else {
			dstDeferredCleanupBlocks++
			dstDeferredCleanups.push(&b.lfnode)
		}
	}
	blocks := dstDeferredCleanupBlocks
	for {
		p := dstDeferredCleanups.pop()
		if p == nil {
			break
		}
		gcCleanups.full.push((*lfnode)(p))
	}
	if blocks != 0 {
		gcCleanups.addWork(int(blocks))
	}
	dstDeferredCleanupBlocks = 0
	unlock(&finlock)
}

func (q *cleanupQueue) dstParkWorkerIfBlocked() bool {
	if !dstCallbackWorkersBlocked() {
		return false
	}
	lock(&q.lock)
	if !dstCallbackWorkersBlocked() {
		unlock(&q.lock)
		return false
	}
	q.asleep.Add(1)
	q.sleeping.push(getg())
	goparkunlock(&q.lock, waitReasonCleanupWait, traceBlockSystemGoroutine, 1)
	return true
}

func dstWakeBlockedCleanupWorkers() {
	q := &gcCleanups
	lock(&q.lock)
	wake := q.sleeping.size
	if wake == 0 {
		unlock(&q.lock)
		return
	}
	q.asleep.Add(-wake)
	var list gList
	for range wake {
		list.push(q.sleeping.pop())
	}
	unlock(&q.lock)
	injectglist(&list)
}

func dstResetCleanupRunCounters() {
	queued, _ := gcCleanups.readQueueStats()
	dstCleanupRunBaseQueued.Store(queued)
	dstCleanupRunExecuted.Store(0)
}

func cleanupPending() bool {
	queued, executed := gcCleanups.readQueueStats()
	if dstActive() {
		return queued-dstCleanupRunBaseQueued.Load() != dstCleanupRunExecuted.Load()
	}
	return queued != executed+dstCleanupsDiscarded.Load()
}
