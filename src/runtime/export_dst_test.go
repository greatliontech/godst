// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package runtime

import (
	"internal/abi"
	"internal/runtime/gc"
	"unsafe"
)

type DSTFakeTimer = timer

func DSTFakeTimersTestReset(epoch uint64) uint64 {
	lock(&dstFakeTimers.lock)
	old := dstRunEpoch.Load()
	dstRunEpoch.Store(epoch)
	dstFakeTimers.epoch.Store(epoch - 1)
	dstFakeTimers.head.Store(nil)
	unlock(&dstFakeTimers.lock)
	return old
}

func DSTFakeTimersTestRestore(epoch uint64) {
	lock(&dstFakeTimers.lock)
	dstFakeTimers.head.Store(nil)
	dstFakeTimers.epoch.Store(epoch)
	dstRunEpoch.Store(epoch)
	unlock(&dstFakeTimers.lock)
}

func DSTFakeTimersTestRegister(t *DSTFakeTimer) {
	dstRegisterFakeTimer(t)
}

func DSTFakeTimersTestRollPaused(t *DSTFakeTimer, entered, release *uint32) {
	dstFakeTimersRollTestRelease.Store(release)
	dstFakeTimersRollTestEntered.Store(entered)
	dstFakeTimersRoll()
	dstRegisterFakeTimer(t)
	dstFakeTimersRollTestRelease.Store(nil)
}

func DSTFakeTimersTestCount() int {
	lock(&dstFakeTimers.lock)
	defer unlock(&dstFakeTimers.lock)
	n := 0
	for t := dstFakeTimers.head.Load(); t != nil; t = t.dstFakeNext {
		n++
	}
	return n
}

func DSTFakeTimersTestInit(t *DSTFakeTimer, host uint32, when, period int64) {
	t.dstHost = host
	t.when = when
	t.period = period
}

func DSTFakeTimersTestRemap(host uint32, now, oldPPB, newPPB int64, timers ...*DSTFakeTimer) []int64 {
	dstRemapHostTimers(host, now, oldPPB, newPPB)
	when := make([]int64, len(timers))
	for i, t := range timers {
		t.lock()
		when[i] = t.when
		t.unlock()
	}
	return when
}

func DSTTestStableCandidateSelection(physical []uint64, ordinal uint32) uint64 {
	gs := make([]g, len(physical))
	pp := new(p)
	for i, seq := range physical {
		gs[i].dstSimG = true
		gs[i].dstSeq = seq
		pp.runq[i].set(&gs[i])
	}
	c := dstCandidates{pp: pp, ringN: uint32(len(physical))}
	return c.at(c.simSeqIdx(uint32(len(physical)), ordinal)).dstSeq
}

func DSTTestPCTTieSelection(physicalSeq, physicalGoid []uint64) uint64 {
	gs := make([]g, len(physicalSeq))
	pp := new(p)
	for i, seq := range physicalSeq {
		gs[i].dstSimG = true
		gs[i].dstSeq = seq
		gs[i].goid = physicalGoid[i]
		gs[i].dstPrio = 1
		pp.runq[i].set(&gs[i])
	}
	old := dstPCT
	dstPCT = dstPCTState{}
	defer func() { dstPCT = old }()
	c := dstCandidates{pp: pp, ringN: uint32(len(physicalSeq))}
	return c.at(dstPCTSelect(&c, uint32(len(physicalSeq)))).dstSeq
}

func DSTTestHarnessTransparency(strategy int) [3]bool {
	oldKind, oldBubble := dstSchedKind, dstSimBubble
	defer func() { dstSchedKind, dstSimBubble = oldKind, oldBubble }()
	if strategy == 0 {
		dstSchedKind = dstSchedRandom
	} else {
		dstSchedKind = dstSchedPCT
	}
	root := &g{dstG: dstG{dstSimG: true}}
	drain := &g{dstG: dstG{dstSimG: true}}
	gc := &g{dstG: dstG{dstSimG: true, dstGCInternal: true}}
	dstSimBubble = &synctestBubble{root: root, dstBubble: dstBubble{gcDrain: drain}}
	return [3]bool{dstIsInfraCandidate(root), dstIsInfraCandidate(drain), dstIsInfraCandidate(gc)}
}

func DSTTestPCTChangeCount(depth int) int {
	oldDepth, oldSteps, oldPCT, oldRand := dstPCTDepth, dstPCTSteps, dstPCT, dstSchedRand
	defer func() { dstPCTDepth, dstPCTSteps, dstPCT, dstSchedRand = oldDepth, oldSteps, oldPCT, oldRand }()
	dstPCTDepth, dstPCTSteps = int32(depth), 100
	dstSchedRootPCT()
	return int(dstPCT.nchange)
}

// UpdateMaxProcsGStarted reports whether the GOMAXPROCS auto-update helper
// goroutine is enabled (it is not started under GODEBUG=updatemaxprocs=0).
// Callers synchronize with the asynchronously spawned helper's initial park via
// UpdateMaxProcsGIdle rather than reading updateMaxProcsG.g unsynchronized.
func UpdateMaxProcsGStarted() bool {
	return debug.updatemaxprocs != 0
}

// UpdateMaxProcsGIdle reports whether the GOMAXPROCS auto-update helper is
// parked waiting for a sysmon push. idle==false with no pending update means
// the helper died — the protocol breakage the helper-survival regression
// guards against.
func UpdateMaxProcsGIdle() bool {
	return updateMaxProcsG.idle.Load()
}

// WakeUpdateMaxProcsG performs the same push sysmon does to hand the helper an
// update (with the current gomaxprocs as the target value, so the resulting
// procresize is a no-op). Returns false without pushing if the helper is not
// idle.
func WakeUpdateMaxProcsG() bool {
	if !updateMaxProcsG.idle.Load() {
		return false
	}
	lock(&updateMaxProcsG.lock)
	if !updateMaxProcsG.idle.Load() {
		unlock(&updateMaxProcsG.lock)
		return false
	}
	updateMaxProcsG.procs = gomaxprocs
	updateMaxProcsG.idle.Store(false)
	var list gList
	list.push(updateMaxProcsG.g)
	injectglist(&list)
	unlock(&updateMaxProcsG.lock)
	return true
}

// CustomGOMAXPROCS reports whether automatic GOMAXPROCS updating is disabled
// by a manual setting.
func CustomGOMAXPROCS() bool {
	lock(&sched.lock)
	custom := sched.customGOMAXPROCS
	unlock(&sched.lock)
	return custom
}

func DstFaultRandDraw() uint64                  { return dstFaultRandUint64() }
func DstSchedRandPeek() uint64                  { return dstSchedRand }
func DstFaultSchedRootsDiffer(seed uint64) bool { return dstFaultRoot(seed) != dstSchedRoot(seed) }

func DstFreshGHeapBytes() uint64   { return dstFreshGHeapBytes() }
func DstPooledAllocBytes() uint64  { return dstPooledAlloc.Load() }
func DstPooledMarkedBytes() uint64 { return dstPooledMarked.Load() }
func DstFreshGHeapBytesWant() uint64 {
	s := abi.TypeFor[g]().Size_
	if s > minSizeForMallocHeader {
		s += mallocHeaderSize
	}
	if s <= gc.SmallSizeMax-8 {
		return uint64(gc.SizeClassToSize[gc.SizeToSizeClass8[divRoundUp(s, gc.SmallSizeDiv)]])
	}
	return uint64(gc.SizeClassToSize[gc.SizeToSizeClass128[divRoundUp(s-gc.SmallSizeMax, gc.LargeSizeDiv)]])
}

func DSTDisabledVisibilityProbe(g1p, g2p unsafe.Pointer) (inAt, inAnySim, inAnyVisible bool, inSimCount uint32, outAt, outAnySim bool) {
	g1 := (*g)(g1p)
	g2 := (*g)(g2p)
	g1.dstSimG = true
	defer func() { g1.dstSimG = false }()
	pp := new(p)
	pp.runq[0].set(g1)
	pp.runq[1].set(g2)
	c := dstCandidates{pp: pp, h: 0, ringN: 2}
	const total = 2
	lock(&sched.lock)
	old := sched.disable.user
	sched.disable.user = true
	c.disableUser = true
	inAt = c.at(0) != nil || c.at(1) != nil
	inAnySim = c.hasSimG(total)
	inAnyVisible = c.anyVisible(total)
	inSimCount = c.simCount(total)
	sched.disable.user = old
	c.disableUser = false
	unlock(&sched.lock)
	outAt = c.at(0) != nil && c.at(1) != nil
	outAnySim = c.hasSimG(total)
	return
}

func DSTProbeG() unsafe.Pointer { return unsafe.Pointer(getg()) }

func DstTestCleanupTakeMovesStamps() bool {
	newBlock := func() *cleanupBlock {
		b := (*cleanupBlock)(persistentalloc(cleanupBlockSize, tagAlign, &memstats.other_sys))
		dstCleanupSlabAttach(b)
		return b
	}
	mk := func(seq uintptr) dstCallbackStamp {
		return dstCallbackStamp{seq: seq, epoch: 7, pid: 9}
	}
	a, b := newBlock(), newBlock()
	as, bs := dstCleanupSlabFor(a), dstCleanupSlabFor(b)
	a.n = 2
	for i := range as {
		as[i] = dstCallbackStamp{seq: 0xDEAD, epoch: 0xDEAD, pid: 3}
	}
	b.n = 3
	for i := uint32(0); i < b.n; i++ {
		bs[i] = mk(uintptr(100 + i))
	}
	a.take(b)
	if a.n != 5 || b.n != 0 {
		return false
	}
	for i := uint32(0); i < 3; i++ {
		if as[2+i] != mk(uintptr(100+i)) {
			return false
		}
		if bs[i] != (dstCallbackStamp{}) {
			return false
		}
	}
	c, d := newBlock(), newBlock()
	cs, ds := dstCleanupSlabFor(c), dstCleanupSlabFor(d)
	c.n = uint32(len(c.cleanups)) - 1
	cs[len(cs)-1] = dstCallbackStamp{seq: 0xDEAD, epoch: 0xDEAD, pid: 3}
	d.n = 2
	ds[0], ds[1] = mk(200), mk(201)
	c.take(d)
	if c.n != uint32(len(c.cleanups)) || d.n != 1 {
		return false
	}
	if cs[len(cs)-1] != mk(201) {
		return false
	}
	if ds[1] != (dstCallbackStamp{}) || ds[0] != mk(200) {
		return false
	}
	return true
}

func dstTestVisitGFields(gp *g, stamp bool) (residue bool) {
	visit := func(zero bool, set func()) {
		if stamp {
			set()
		} else if !zero {
			residue = true
		}
	}
	visit(!gp.dstInternalGC, func() { gp.dstInternalGC = true })
	visit(gp.dstrand == 0, func() { gp.dstrand = 0xdeadbeef })
	visit(gp.dstBugRand == 0, func() { gp.dstBugRand = 0xfeedface })
	visit(gp.dstTimerSeq == 0, func() { gp.dstTimerSeq = 11 })
	visit(gp.dstHost == 0, func() { gp.dstHost = 7 })
	visit(gp.dstHostScope == nil, func() { gp.dstHostScope = new(dstHostScope) })
	visit(gp.dstProc == 0, func() { gp.dstProc = 7 })
	visit(gp.dstProcSlot == 0, func() { gp.dstProcSlot = 7 })
	visit(gp.dstPid == 0, func() { gp.dstPid = 7 })
	visit(gp.dstPrio == 0, func() { gp.dstPrio = 7 })
	visit(gp.dstSeq == 0, func() { gp.dstSeq = 7 })
	visit(gp.dstAccAddr == 0, func() { gp.dstAccAddr = 7 })
	visit(gp.dstAccSize == 0, func() { gp.dstAccSize = 7 })
	visit(!gp.dstAccWrite, func() { gp.dstAccWrite = true })
	visit(gp.dstAccPC == 0, func() { gp.dstAccPC = 7 })
	visit(gp.dstAccCount == 0, func() { gp.dstAccCount = 7 })
	visit(!gp.dstAccPend, func() { gp.dstAccPend = true })
	visit(!gp.dstAccAuto, func() { gp.dstAccAuto = true })
	visit(!gp.dstSimG, func() {})
	return residue
}

func DSTTestStampSelfG() uintptr {
	gp := getg()
	dstTestVisitGFields(gp, true)
	return uintptr(unsafe.Pointer(gp))
}

func DSTTestSelfGResidue() (gaddr uintptr, residue bool) {
	gp := getg()
	return uintptr(unsafe.Pointer(gp)), dstTestVisitGFields(gp, false)
}
