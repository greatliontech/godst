// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package runtime

import (
	"internal/abi"
	"internal/byteorder"
	"internal/runtime/atomic"
	"unsafe"
)

//go:nosplit
func dstIsCrashedG(gp *g) bool {
	return gp.dstPid < 0
}

//go:nosplit
func dstCanPreemptG(gp *g) bool {
	return gp.dstKernelDepth == 0
}

//go:linkname dstKernelEnter
//go:nosplit
func dstKernelEnter() {
	getg().dstKernelDepth++
}

//go:linkname dstKernelExit
//go:nosplit
func dstKernelExit() {
	gp := getg()
	gp.dstKernelDepth--
	if gp.dstKernelDepth < 0 {
		throw("runtime: negative dstKernelDepth")
	}
	if gp.dstKernelDepth == 0 && gp.preempt {
		gp.stackguard0 = stackPreempt
	}
}

//go:linkname dstKernelSectionEnter
//go:nosplit
func dstKernelSectionEnter() {
	dstKernelEnter()
	dstKernelRaceEnter()
}

//go:linkname dstKernelSectionExit
//go:nosplit
func dstKernelSectionExit() {
	dstKernelRaceExit()
	dstKernelExit()
}

//go:linkname internal_sync_runtime_dstKernelSectionEnter internal/sync.runtime_dstKernelSectionEnter
//go:nosplit
func internal_sync_runtime_dstKernelSectionEnter() {
	dstKernelSectionEnter()
}

//go:linkname internal_sync_runtime_dstKernelSectionExit internal/sync.runtime_dstKernelSectionExit
//go:nosplit
func internal_sync_runtime_dstKernelSectionExit() {
	dstKernelSectionExit()
}

//go:linkname internal_sync_runtime_SemacquireKernel internal/sync.runtime_SemacquireKernel
func internal_sync_runtime_SemacquireKernel(addr *uint32) {
	semacquire1(addr, false, 0, 0, waitReasonSemacquire)
}

// --- debug.go hooks ---

// dstGOMAXPROCSSTWHook, when non-nil, runs in GOMAXPROCS/SetDefaultGOMAXPROCS
// after the not-active gate and before the stop-the-world.
var dstGOMAXPROCSSTWHook func()

func dstMaybeRunGOMAXPROCSSTWHook() {
	if dstGOMAXPROCSSTWHook != nil {
		dstGOMAXPROCSSTWHook()
	}
}

//go:linkname dstSetGOMAXPROCSSTWHook
func dstSetGOMAXPROCSSTWHook(f func()) { dstGOMAXPROCSSTWHook = f }

//go:linkname dstGOMAXPROCSAutoFP
func dstGOMAXPROCSAutoFP() bool {
	lock(&sched.lock)
	auto := !sched.customGOMAXPROCS
	unlock(&sched.lock)
	return auto
}

func dstSimNumCPUForCaller() (int, bool) {
	if !dstSimIdentityActive() {
		return 0, false
	}
	n := dstSimNumCPU
	if id, ok := dstHostIdentFor(getg().dstHost); ok && id.numcpu > 0 {
		n = int(id.numcpu)
	}
	if n > 0 {
		return n, true
	}
	return 0, false
}

// --- malloc.go hooks ---

func dstMallocgcPost(x unsafe.Pointer, elemsize uintptr, typ *_type) {
	_ = x
	if cur := getg().m.curg; cur != nil && cur.dstSimG {
		if dstIsInternalPooledType(typ) {
			if typ != dstInternalPooledTypes.g {
				dstPooledAlloc.Add(int64(elemsize))
			}
		} else if getg() == cur {
			dstHeapAlloc.Add(int64(elemsize))
			dstProcAllocAdd(cur.dstProc, int64(elemsize))
		}
		// Note: gcStart is already schedule-transparent with respect to other
		// simulation goroutines (dstEnterGCDisassociate sets gp.dstGCInternal,
		// which firstSystemG resumes ahead of dstSchedSelect), so kernel sections
		// need not suppress the heap GC trigger.
		if getg() == cur {
			if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
				gcStart(t)
			}
		}
	}
}

// --- mgc.go / mgcmark.go / mgcpacer.go / mheap.go hooks ---

// dstGCForce runs a full forced GC cycle via runtime.GC while bypassing the
// foreign-caller refusal, without splitting runtime.GC into a wrapper+helper.
func dstGCForce() {
	gp := getg()
	gp.dstInternalGC = true
	defer func() { gp.dstInternalGC = false }()
	GC()
}

func dstBeforeForcedGC() {
	if !getg().dstInternalGC {
		dstRefuseForeignForcedGC()
	}
}

func dstAfterCycleCaptureForcedGC() {
	gp := getg()
	if !gp.dstInternalGC && gp != dstSimRootG {
		dstRefuseForeignForcedGC()
	}
}

//go:linkname dstGoroutineLeakPendingFP
func dstGoroutineLeakPendingFP() bool {
	return work.goroutineLeak.pending.Load()
}

//go:linkname dstGoroutineLeakGCFP
func dstGoroutineLeakGCFP() {
	goroutineLeakGC()
}

func dstGCTriggerHeapTest() bool {
	hm := gcController.heapMarked
	base := dstHeapBase.Load()
	bubbleMarked := uint64(0)
	if hm > base {
		bubbleMarked = hm - base
	}
	if pm := dstPooledMarked.Load(); bubbleMarked > pm {
		bubbleMarked -= pm
	} else {
		bubbleMarked = 0
	}
	if dstMemLimit > 0 && bubbleMarked+dstHeapAlloc.Load() >= uint64(dstMemLimit) {
		return true
	}
	gp := gcController.gcPercent.Load()
	switch {
	case gp < 0 && dstMemLimit > 0:
		return false
	case gp < 0:
		return dstHeapAlloc.Load() >= defaultHeapMinimum
	default:
		target := bubbleMarked * uint64(gp) / 100
		if target < gcController.heapMinimum {
			target = gcController.heapMinimum
		}
		return dstHeapAlloc.Load() >= target
	}
}

var (
	dstAllowConcurrentMarkProbe   bool
	dstGCAssistDisassociates      atomic.Uint64
	dstUpdateMaxProcsSTWs         atomic.Uint64
	dstSysmonMaxProcsActiveSkips  atomic.Uint64
	dstSysmonMaxProcsRecheckBails atomic.Uint64
	dstSysmonMaxProcsRaceArm      atomic.Uint32
	dstSysmonMaxProcsPinnedM      uintptr
	dstSuppressSysmonMaxProcs     atomic.Bool
	dstSysmonMaxProcsTestG        atomic.Uintptr
	dstAssistSink                 any
)

func dstUseStopTheWorldGC() bool {
	return dstActive() && !dstAllowConcurrentMarkProbe
}

func dstEnterGCDisassociate(gp *g) bool {
	prev := gp.dstGCInternal
	gp.dstGCInternal = gp.dstSimG
	return prev
}

func dstEnterGCAssistDisassociate(gp *g) bool {
	if gp.dstSimG {
		dstGCAssistDisassociates.Add(1)
	}
	return dstEnterGCDisassociate(gp)
}

func dstExitGCDisassociate(gp *g, prev bool) {
	gp.dstGCInternal = prev
}

func dstNoteUpdateMaxProcsSTW() {
	dstUpdateMaxProcsSTWs.Add(1)
}

//go:nowritebarrierrec
func dstNoteSysmonMaxProcsActiveSkip() {
	dstSysmonMaxProcsActiveSkips.Add(1)
}

//go:nowritebarrierrec
func dstSysmonUpdateMaxProcsAllowed() bool {
	if !dstSuppressSysmonMaxProcs.Load() {
		return true
	}
	return uintptr(unsafe.Pointer(getg())) == dstSysmonMaxProcsTestG.Load()
}

//go:nowritebarrierrec
func dstSysmonUpdateMaxProcsBeforeLock() {
	if testG := dstSysmonMaxProcsTestG.Load(); testG != 0 && uintptr(unsafe.Pointer(getg())) != testG {
		return
	}
	if !dstSysmonMaxProcsRaceArm.CompareAndSwap(1, 0) {
		return
	}
	// Pin the current M across the race window so the woken helper cannot
	// complete stopTheWorldGC and re-park before sysmonUpdateGOMAXPROCS
	// reaches its under-lock idle recheck, even on a loaded multi-core host.
	mp := acquirem()
	dstSysmonMaxProcsPinnedM = uintptr(unsafe.Pointer(mp))
	if !updateMaxProcsG.idle.Load() {
		dstSysmonMaxProcsPinnedM = 0
		releasem(mp)
		throw("dst: updateMaxProcsG unexpectedly non-idle at race hook")
	}
	lock(&updateMaxProcsG.lock)
	if !updateMaxProcsG.idle.Load() {
		unlock(&updateMaxProcsG.lock)
		dstSysmonMaxProcsPinnedM = 0
		releasem(mp)
		throw("dst: updateMaxProcsG lost idle under lock at race hook")
	}
	updateMaxProcsG.procs = gomaxprocs
	updateMaxProcsG.idle.Store(false)
	var list gList
	list.push(updateMaxProcsG.g)
	injectglist(&list)
	unlock(&updateMaxProcsG.lock)
}

//go:nowritebarrierrec
func dstSysmonUpdateMaxProcsAfterLock() {
	if mp := dstSysmonMaxProcsPinnedM; mp != 0 && mp == uintptr(unsafe.Pointer(getg().m)) {
		dstSysmonMaxProcsPinnedM = 0
		releasem((*m)(unsafe.Pointer(mp)))
	}
}

//go:nowritebarrierrec
func dstNoteSysmonMaxProcsRecheckBail() {
	dstSysmonMaxProcsRecheckBails.Add(1)
}

//go:linkname dstGCAssistDisassociatesFP
func dstGCAssistDisassociatesFP() uint64 { return dstGCAssistDisassociates.Load() }

//go:linkname dstUpdateMaxProcsGStartedFP
func dstUpdateMaxProcsGStartedFP() bool { return debug.updatemaxprocs != 0 }

//go:linkname dstUpdateMaxProcsSTWsFP
func dstUpdateMaxProcsSTWsFP() uint64 { return dstUpdateMaxProcsSTWs.Load() }

//go:linkname dstSysmonMaxProcsActiveSkipsFP
func dstSysmonMaxProcsActiveSkipsFP() uint64 { return dstSysmonMaxProcsActiveSkips.Load() }

//go:linkname dstSysmonMaxProcsRecheckBailsFP
func dstSysmonMaxProcsRecheckBailsFP() uint64 { return dstSysmonMaxProcsRecheckBails.Load() }

//go:linkname dstTestDriveBubbleGCAssist
func dstTestDriveBubbleGCAssist() bool {
	gp := getg()
	if gp.bubble == nil || !gp.dstSimG {
		return false
	}
	if atomic.Load(&gcBlackenEnabled) == 0 {
		dstAllowConcurrentMarkProbe = true
		gcStart(gcTrigger{kind: gcTriggerCycle, n: work.cycles.Load() + 1})
		dstAllowConcurrentMarkProbe = false
	}
	if atomic.Load(&gcBlackenEnabled) == 0 {
		return false
	}
	gcController.bgScanCredit.Store(0)
	gp.gcAssistBytes = -(1 << 20)
	gp.preempt = true
	dstAssistSink = make([]byte, 256)
	dstAssistSink = nil
	return true
}

//go:linkname dstTestSetAutoMaxProcsMismatch
func dstTestSetAutoMaxProcsMismatch() int {
	def := int(defaultGOMAXPROCS(0))
	alt := def + 1
	if alt < 2 {
		alt = 2
	}
	GOMAXPROCS(alt)
	lock(&sched.lock)
	sched.customGOMAXPROCS = false
	unlock(&sched.lock)
	return alt
}

//go:linkname dstTestArmSysmonMaxProcsRecheckRace
func dstTestArmSysmonMaxProcsRecheckRace() {
	dstSysmonMaxProcsRaceArm.Store(1)
}

//go:linkname dstTestSuppressSysmonMaxProcs
func dstTestSuppressSysmonMaxProcs(suppress bool) {
	if suppress {
		dstSysmonMaxProcsTestG.Store(uintptr(unsafe.Pointer(getg())))
		dstSuppressSysmonMaxProcs.Store(true)
		// Synchronize with any in-flight sysmon tick already inside sysmonlock.
		lock(&sched.sysmonlock)
		unlock(&sched.sysmonlock)
		return
	}
	dstSuppressSysmonMaxProcs.Store(false)
	dstSysmonMaxProcsTestG.Store(0)
}

//go:linkname dstTestCallSysmonUpdateGOMAXPROCS
func dstTestCallSysmonUpdateGOMAXPROCS() {
	sysmonUpdateGOMAXPROCS()
}

func dstGCResetLive() {
	dstHeapAlloc.Store(0)
	dstPooledMarked.Store(dstPooledAlloc.Load())
}

func dstEnterSetGCPercent() {
	lock(&dstGCControlLock)
	if dstForeignForcedGC() {
		unlock(&dstGCControlLock)
		dstPanicForeignForcedGC()
	}
}

func dstExitSetGCPercent() {
	unlock(&dstGCControlLock)
}

func dstRegisterSpecialStamp(sp unsafe.Pointer) {
	if st := dstCallbackStampAtRegistration(); st != (dstCallbackStamp{}) {
		lock(&mheap_.speciallock)
		dstSpecialStampInsertLocked(sp, st)
		unlock(&mheap_.speciallock)
	}
}

func dstDiscardSpecialStampLocked(sp unsafe.Pointer) {
	dstSpecialStampTakeLocked(sp)
}

// --- proc.go hooks & unified runnable-set scheduler ---

func dstGoready(gp *g) {
	if dstActive() && dstSchedKind == dstSchedScheduled {
		readier := getg()
		if readier.dstKernelDepth > 0 || (raceenabled && readier.raceignore != 0) {
			return
		}
		dstRecordReadyEdge(readier, gp)
	}
}

func dstAllgadd() {
	if dstActive() {
		if cur := getg().m.curg; cur != nil && cur.dstSimG {
			dstPooledAlloc.Add(int64(dstPooledGBytes))
		}
	}
}

func dstNewprocReadyEdge(callergp, newg *g) {
	if dstActive() && dstSchedKind == dstSchedScheduled {
		if raceenabled && int32(callergp.raceignore) > callergp.dstKernelDepth {
			return
		}
		dstRecordReadyEdge(callergp, newg)
	}
}

func dstNewprocPostYield() {
	if dstActive() && dstSchedKind == dstSchedScheduled && dstPostGoYield && getg().dstKernelDepth == 0 {
		dstYieldPoint()
	}
}

func dstNewproc1Init(newg, callergp *g) {
	newg.dstKernelDepth = 0
	if !dstActive() {
		return
	}
	if callergp.dstrand != 0 {
		newg.dstrand = dstrandUint64(callergp)
		if newg.dstrand == 0 {
			newg.dstrand = 1
		}
	}
	if callergp.dstBugRand != 0 {
		newg.dstBugRand = dstBugRandUint64(callergp)
		if newg.dstBugRand == 0 {
			newg.dstBugRand = 1
		}
	}
	newg.dstHost = callergp.dstHost
	newg.dstHostScope = callergp.dstHostScope
	newg.dstProc = callergp.dstProc
	newg.dstProcSlot = callergp.dstProcSlot
	newg.dstPid = callergp.dstPid
	newg.dstSimG = newg.bubble != nil && newg.bubble == dstSimBubble
	dstClearSchedState(newg)
	if newg.dstSimG {
		dstEnsureSeq(newg)
	}
	if dstSchedKind == dstSchedPCT && gomaxprocs == 1 &&
		(newg.dstSimG || (callergp == dstSimRootG && dstSimBubble == nil)) {
		dstPCTAssignPrio(newg)
	}
}

func dstRunqputOverflow(pp *p, gp *g, h, t uint32) bool {
	if dstActive() && gomaxprocs == 1 {
		if !pp.dstRunqOvf.empty() || t-h >= uint32(len(pp.runq)) {
			pp.dstRunqOvf.pushBack(gp)
			dstSchedOvfPuts++
			return true
		}
	}
	return false
}

// removeAt removes and returns the i-th element (0-based from the head) of q.
// Caller must hold whatever lock guards q.
func (q *gQueue) removeAt(i uint32) *g {
	if i == 0 {
		return q.pop()
	}
	prev := q.head.ptr()
	for ; i > 1; i-- {
		prev = prev.schedlink.ptr()
	}
	gp := prev.schedlink.ptr()
	prev.schedlink = gp.schedlink
	if q.tail.ptr() == gp {
		q.tail.set(prev)
	}
	gp.schedlink = 0
	q.size--
	return gp
}

func dstFindRunnable(pp *p) (gp *g, inheritTime bool) {
	lock(&sched.lock)
	var c dstCandidates
	var total uint32
	for {
		c = dstCandidates{pp: pp, hasNext: pp.runnext != 0, disableUser: sched.disable.user, h: pp.runqhead}
		c.ringN = pp.runqtail - c.h
		c.ovfN = uint32(pp.dstRunqOvf.size)
		total = c.ringN + c.ovfN + uint32(sched.runq.size)
		if c.hasNext {
			total++
		}
		if total == 0 {
			unlock(&sched.lock)
			return nil, false
		}
		if k, ok := c.firstCrashedG(total); ok {
			c.removeAt(k)
			continue
		}
		if c.disableUser && !c.anyVisible(total) {
			if gcBlackenEnabled == 0 {
				throw("dst: user scheduling disabled outside the GC mark phase")
			}
			unlock(&sched.lock)
			return nil, false
		}
		break
	}
	dstSchedDecisions++
	if dstSimBubble != nil {
		dstSchedSinceQuiesce++
		if bound := dstWedgeDecisionBound(); bound > 0 && dstSchedSinceQuiesce >= bound {
			dstWedgeThrowScheduler(&c, total)
		}
	}
	var sel uint32
	sysK, sysOK := c.firstSystemG(total)
	sysTransparent := false
	if sysOK {
		gp := c.at(sysK)
		sysTransparent = dstTransparentCandidate(gp)
	}
	if sysOK && (sysTransparent || !dstSchedPrevSys || !c.hasSimG(total)) {
		sel = sysK
		dstSchedSysScheds++
		if !sysTransparent {
			dstSchedPrevSys = true
			if dstSchedKind == dstSchedScheduled {
				if gp := c.at(sysK); gp != nil && gp != dstSimRootG && !isSystemGoroutine(gp, false) {
					dstSchedForeignSeen = true
				}
			}
		}
	} else {
		sel = dstSchedSelect(&c, total)
		dstSchedPrevSys = false
	}
	gp, inheritTime = c.removeAt(sel)
	unlock(&sched.lock)
	return gp, inheritTime
}

func dstWedgeThrowScheduler(c *dstCandidates, total uint32) {
	printlock()
	print("DST-WEDGE: the simulation bubble made ", dstSchedSinceQuiesce,
		" scheduler decisions without reaching durable quiescence.\n")
	print("Some goroutine set stays perpetually runnable (a busy-poll: a Gosched/select spin, mutex\n")
	print("ping-pong, or another non-durable park loop), so all-goroutines-durably-blocked never holds:\n")
	print("virtual time cannot advance, timers never fire, and the run is permanently stuck in zero\n")
	print("virtual time — invisible to the deadlock detector, which needs every goroutine durably\n")
	print("blocked. If the workload legitimately makes this many decisions between quiescences, raise\n")
	print("testing/simulation Options.WedgeDecisionLimit.\n")
	print("\nrunnable simulation goroutines at the failing decision:\n")
	for k := uint32(0); k < total; k++ {
		gp := c.at(k)
		if gp == nil || dstIsInfraCandidate(gp) {
			continue
		}
		print("\n")
		goroutineheader(gp)
		if readgstatus(gp)&^_Gscan == _Grunning {
			print("\tgoroutine running; stack unavailable\n")
			printcreatedby(gp)
		} else {
			traceback(^uintptr(0), ^uintptr(0), 0, gp)
		}
	}
	printunlock()
	throw("DST-WEDGE: simulation bubble cannot reach durable quiescence (perpetually runnable goroutine)")
}

func dstSchedSelect(c *dstCandidates, total uint32) uint32 {
	switch dstSchedKind {
	case dstSchedPCT:
		k := dstPCTSelect(c, total)
		if dstSchedTraceOn {
			dstTraceRecord(dstTraceSiteSched, c.simCount(total), c.simOrdinalOf(total, k), dstEnsureSeq(c.at(k)))
		}
		return k
	case dstSchedScheduled:
		return dstScheduledSelect(c, total)
	}
	n := c.simCount(total)
	j := dstSchedRandn(n)
	k := c.simSeqIdx(total, j)
	if dstSchedTraceOn {
		dstTraceRecord(dstTraceSiteSched, n, j, dstEnsureSeq(c.at(k)))
	}
	return k
}

func (c *dstCandidates) simOrdinalOf(total, k uint32) uint32 {
	seq := dstEnsureSeq(c.at(k))
	var ord uint32
	for i := uint32(0); i < total; i++ {
		if gp := c.at(i); gp != nil && !dstIsInfraCandidate(gp) && dstEnsureSeq(gp) < seq {
			ord++
		}
	}
	return ord
}

func dstPCTSelect(c *dstCandidates, total uint32) uint32 {
	dstPCT.step++
	best := ^uint32(0)
	var bestPrio int64
	var bestSeq uint64
	for k := uint32(0); k < total; k++ {
		g := c.at(k)
		if g == nil || dstIsInfraCandidate(g) {
			continue
		}
		seq := dstEnsureSeq(g)
		if best == ^uint32(0) || g.dstPrio > bestPrio || (g.dstPrio == bestPrio && seq < bestSeq) {
			best, bestPrio, bestSeq = k, g.dstPrio, seq
		}
	}
	if best == ^uint32(0) {
		throw("dst: no simulation candidate in PCT selection")
	}
	for i := int32(0); i < dstPCT.nchange; i++ {
		if !dstPCT.applied[i] && dstPCT.step == dstPCT.changeAt[i] {
			c.at(best).dstPrio = dstPCT.changeLow[i]
			dstPCT.applied[i] = true
		}
	}
	return best
}

func (c *dstCandidates) simSeqIdx(total, n uint32) uint32 {
	var after uint64
	for ordinal := uint32(0); ordinal <= n; ordinal++ {
		best := ^uint32(0)
		var bestSeq uint64
		for k := uint32(0); k < total; k++ {
			gp := c.at(k)
			if gp == nil || dstIsInfraCandidate(gp) {
				continue
			}
			seq := dstEnsureSeq(gp)
			if seq > after && (best == ^uint32(0) || seq < bestSeq) {
				best, bestSeq = k, seq
			}
		}
		if best == ^uint32(0) {
			throw("dst: simulation candidate ordinal out of range")
		}
		if ordinal == n {
			return best
		}
		after = bestSeq
	}
	throw("dst: unreachable simulation candidate selection")
	return 0
}

type dstCandidates struct {
	pp          *p
	hasNext     bool
	disableUser bool
	h           uint32
	ringN       uint32
	ovfN        uint32
	ovfCursorK  uint32
	ovfCursor   guintptr
	glbCursorK  uint32
	glbCursor   guintptr
}

func (c *dstCandidates) firstSystemG(total uint32) (uint32, bool) {
	if dstSimBubble != nil && dstSimBubble.gcDrain != nil {
		for k := uint32(0); k < total; k++ {
			if c.at(k) == dstSimBubble.gcDrain {
				return k, true
			}
		}
	}
	if dstSimBubble != nil {
		best := ^uint32(0)
		var bestSeq uint64
		for k := uint32(0); k < total; k++ {
			gp := c.at(k)
			if gp == nil || !gp.dstSimG || !gp.dstGCInternal {
				continue
			}
			if best == ^uint32(0) || gp.dstSeq < bestSeq {
				best, bestSeq = k, gp.dstSeq
			}
		}
		if best != ^uint32(0) {
			return best, true
		}
	}
	if dstSimBubble != nil && dstSimBubble.root != nil {
		for k := uint32(0); k < total; k++ {
			if c.at(k) == dstSimBubble.root {
				return k, true
			}
		}
	}
	for k := uint32(0); k < total; k++ {
		gp := c.at(k)
		if gp == nil {
			continue
		}
		if dstIsInfraCandidate(gp) {
			return k, true
		}
	}
	return 0, false
}

func dstIsInfraCandidate(gp *g) bool {
	if dstTransparentCandidate(gp) {
		return true
	}
	if !gp.dstSimG {
		return true
	}
	return false
}

func (c *dstCandidates) hasSimG(total uint32) bool {
	for k := uint32(0); k < total; k++ {
		if gp := c.at(k); gp != nil && !dstIsInfraCandidate(gp) {
			return true
		}
	}
	return false
}

func (c *dstCandidates) simCount(total uint32) uint32 {
	n := uint32(0)
	for k := uint32(0); k < total; k++ {
		if gp := c.at(k); gp != nil && !dstIsInfraCandidate(gp) {
			n++
		}
	}
	return n
}

func (c *dstCandidates) simIdx(total, j uint32) uint32 {
	for k := uint32(0); k < total; k++ {
		if gp := c.at(k); gp != nil && !dstIsInfraCandidate(gp) {
			if j == 0 {
				return k
			}
			j--
		}
	}
	throw("dst: simIdx out of range")
	return 0
}

func (c *dstCandidates) anyVisible(total uint32) bool {
	for k := uint32(0); k < total; k++ {
		if c.at(k) != nil {
			return true
		}
	}
	return false
}

func (c *dstCandidates) firstCrashedG(total uint32) (uint32, bool) {
	for k := uint32(0); k < total; k++ {
		gp := c.at(k)
		if gp != nil && gp.dstPid < 0 {
			return k, true
		}
	}
	return 0, false
}

func (c *dstCandidates) at(k uint32) *g {
	gp := c.atRaw(k)
	if gp != nil && c.disableUser && !schedEnabled(gp) {
		return nil
	}
	return gp
}

func (c *dstCandidates) atRaw(k uint32) *g {
	pp := c.pp
	if k < c.ringN {
		return pp.runq[(c.h+k)%uint32(len(pp.runq))].ptr()
	}
	if k < c.ringN+c.ovfN {
		gi := k - c.ringN
		n, from := pp.dstRunqOvf.head.ptr(), uint32(0)
		if c.ovfCursor != 0 && c.ovfCursorK <= gi {
			n, from = c.ovfCursor.ptr(), c.ovfCursorK
		}
		for ; from < gi; from++ {
			n = n.schedlink.ptr()
		}
		c.ovfCursor.set(n)
		c.ovfCursorK = gi
		return n
	}
	if c.hasNext && k == c.ringN+c.ovfN {
		return pp.runnext.ptr()
	}
	gi := k - c.ringN - c.ovfN
	if c.hasNext {
		gi--
	}
	n, from := sched.runq.head.ptr(), uint32(0)
	if c.glbCursor != 0 && c.glbCursorK <= gi {
		n, from = c.glbCursor.ptr(), c.glbCursorK
	}
	for ; from < gi; from++ {
		n = n.schedlink.ptr()
	}
	c.glbCursor.set(n)
	c.glbCursorK = gi
	return n
}

func (c *dstCandidates) removeAt(k uint32) (*g, bool) {
	c.ovfCursor, c.ovfCursorK = 0, 0
	c.glbCursor, c.glbCursorK = 0, 0
	pp := c.pp
	if k < c.ringN {
		idx := (c.h + k) % uint32(len(pp.runq))
		gp := pp.runq[idx].ptr()
		for i := k; i > 0; i-- {
			pp.runq[(c.h+i)%uint32(len(pp.runq))] = pp.runq[(c.h+i-1)%uint32(len(pp.runq))]
		}
		atomic.StoreRel(&pp.runqhead, c.h+1)
		return gp, false
	}
	if k < c.ringN+c.ovfN {
		return pp.dstRunqOvf.removeAt(k - c.ringN), false
	}
	if c.hasNext && k == c.ringN+c.ovfN {
		gp := pp.runnext.ptr()
		pp.runnext = 0
		return gp, true
	}
	gi := k - c.ringN - c.ovfN
	if c.hasNext {
		gi--
	}
	return sched.runq.removeAt(gi), false
}

// --- rand.go hooks ---

func dstFixedSeed(seed *[32]byte) {
	x := uint64(0x6470736565643031) // "dpseed01"
	for i := 0; i < len(seed); i += 8 {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		byteorder.BEPutUint64(seed[i:i+8], z^(z>>31))
	}
}

//go:nosplit
func dstRand() (uint64, bool) {
	gp := getg()
	if dstActive() && gp.dstrand != 0 {
		return dstrandUint64(gp), true
	}
	return 0, false
}

//go:nosplit
func dstrandUint64(gp *g) uint64 {
	gp.dstrand += 0x9e3779b97f4a7c15
	z := gp.dstrand
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

//go:nosplit
func dstBugRandUint64(gp *g) uint64 {
	gp.dstBugRand += 0x9e3779b97f4a7c15
	z := gp.dstBugRand
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

//go:nosplit
func dstrandnRaw(gp *g, n uint32) (j uint32, raw uint64) {
	raw = dstrandUint64(gp)
	return uint32((uint64(uint32(raw)) * uint64(n)) >> 32), raw
}

//go:linkname dstReadRandom
func dstReadRandom(b []byte) bool {
	if !dstActive() {
		return false
	}
	gp := getg()
	if gp.dstrand == 0 {
		return false
	}
	for len(b) >= 8 {
		byteorder.BEPutUint64(b, dstrandUint64(gp))
		b = b[8:]
	}
	if len(b) > 0 {
		var tmp [8]byte
		byteorder.BEPutUint64(tmp[:], dstrandUint64(gp))
		copy(b, tmp[:])
	}
	return true
}

func dstBubbleRoot(seed uint64) uint64 {
	seed += 0x9e3779b97f4a7c15
	seed = (seed ^ (seed >> 30)) * 0xbf58476d1ce4e5b9
	seed = (seed ^ (seed >> 27)) * 0x94d049bb133111eb
	return seed ^ (seed >> 31)
}

// --- select.go hooks ---

func dstSelectPollOrder(gp *g, n uint32) uint32 {
	if dstActive() && gp.dstrand != 0 {
		j, raw := dstrandnRaw(gp, n)
		if dstSchedTraceOn {
			dstTraceRecord(dstTraceSiteSelect, n, j, raw)
		}
		return j
	}
	return cheaprandn(n)
}

// --- sema.go hooks ---

func dstNotifyListNotifyOne(l *notifyList, t uint32) {
	for {
		skipped := false
		for p, s := (*sudog)(nil), l.head; s != nil; p, s = s, s.next {
			if s.ticket == t {
				n := s.next
				if p != nil {
					p.next = n
				} else {
					l.head = n
				}
				if n == nil {
					l.tail = p
				}
				s.next = nil
				if s.g.dstPid < 0 {
					skipped = true
					break
				}
				unlock(&l.lock)
				if s.g.bubble != nil && getg().bubble != s.g.bubble {
					println("semaphore wake of synctest goroutine", s.g.goid, "from outside bubble")
					fatal("semaphore wake of synctest goroutine from outside bubble")
				}
				readyWithTime(s, 4)
				return
			}
		}
		if !skipped {
			unlock(&l.lock)
			return
		}
		t++
		if t == l.wait.Load() {
			unlock(&l.lock)
			return
		}
		atomic.Store(&l.notify, t+1)
	}
}

//go:linkname internal_sync_dstMutexWaitVirtual internal/sync.runtime_dstMutexWaitVirtual
func internal_sync_dstMutexWaitVirtual() bool {
	return dstActive() && dstInSimBubble()
}

//go:linkname internal_sync_runtime_dstSemrelease internal/sync.runtime_dstSemrelease
func internal_sync_runtime_dstSemrelease(addr *uint32, handoff bool, skipframes int) (woke bool, crashed int32, shouldYield bool) {
	return dstSemrelease1(addr, handoff, skipframes)
}

//go:linkname internal_sync_runtime_dstSemreleaseYield internal/sync.runtime_dstSemreleaseYield
func internal_sync_runtime_dstSemreleaseYield() {
	goyield()
}

//go:linkname dstSemacquireFP
func dstSemacquireFP(addr *uint32) {
	semacquire1(addr, false, 0, 0, waitReasonSyncMutexLock)
}

//go:linkname dstSemreleaseHandoffFP
func dstSemreleaseHandoffFP(addr *uint32) {
	semrelease1(addr, true, 0)
}

//go:linkname dstSemreleaseDSTHandoffFP
func dstSemreleaseDSTHandoffFP(addr *uint32) {
	_, _, shouldYield := dstSemrelease1(addr, true, 0)
	if shouldYield {
		goyield()
	}
}

//go:linkname sync_runtime_dstSemrelease sync.runtime_dstSemrelease
func sync_runtime_dstSemrelease(addr *uint32, handoff bool, skipframes int) (woke bool, crashed int32, shouldYield bool) {
	return dstSemrelease1(addr, handoff, skipframes)
}

// dstSemrelease1 mirrors semrelease1 while reporting how many crashed waiters
// were skipped from the semaphore wait queue and whether a live waiter was
// woken. When handoff is true, direct G handoff yielding is deferred to the
// caller (via shouldYield) so Mutex.unlockSlow can repair m.state for skipped
// crashed waiters before the woken waiter observes m.state.
func dstSemrelease1(addr *uint32, handoff bool, skipframes int) (woke bool, crashed int32, shouldYield bool) {
	root := semtable.rootFor(addr)
	atomic.Xadd(addr, 1)

	if root.nwait.Load() == 0 {
		return false, 0, false
	}

	lockWithRank(&root.lock, lockRankRoot)
	if root.nwait.Load() == 0 {
		unlock(&root.lock)
		return false, 0, false
	}
	s, t0, tailtime := root.dequeue(addr)
	if s != nil {
		root.nwait.Add(-1)
	}
	for s != nil && dstIsCrashedG(s.g) {
		crashed++
		s, t0, tailtime = root.dequeue(addr)
		if s != nil {
			root.nwait.Add(-1)
		}
	}
	if crashed > 0 && s == nil {
		cansemacquire(addr)
	}
	unlock(&root.lock)
	if s != nil {
		acquiretime := s.acquiretime
		if acquiretime != 0 {
			dt0 := t0 - acquiretime
			dt := dt0
			if s.waiters != 0 {
				dtail := t0 - tailtime
				dt += (dtail + dt0) / 2 * int64(s.waiters)
			}
			mutexevent(dt, 3+skipframes)
		}
		if s.ticket != 0 {
			throw("corrupted semaphore ticket")
		}
		if handoff && cansemacquire(addr) {
			s.ticket = 1
		}
		readyWithTime(s, 5+skipframes)
		return true, crashed, s.ticket == 1 && getg().m.locks == 0 && getg() != getg().m.g0 && dstCanPreemptG(getg())
	}
	return false, crashed, false
}

// --- synctest.go / traceback.go hooks ---

var synctestGCDrainPC uintptr

func dstIsSynctestGCDrainG(gp *g) bool {
	return synctestGCDrainPC != 0 && gp.startpc == synctestGCDrainPC
}

func dstSynctestMaybeWakeLocked(bubble *synctestBubble) {
	if bubble == dstSimBubble {
		dstSchedSinceQuiesce = 0
	}
}

func dstSynctestDeferCleanup(gp *g, bubble *synctestBubble) {
	if dstActive() {
		dstClearSchedState(gp)
	}
	if dstSimBubble == bubble {
		dstSimBubble = nil
	}
	gp.dstSimG = false
}

func dstSynctestClaimBubble(gp *g, bubble *synctestBubble) {
	if dstActive() && gp == dstSimRootG && dstSimBubble == nil {
		dstSimBubble = bubble
		bubble.main.dstSimG = true
		gp.dstSimG = true
		dstSeqCtr.Store(0)
		bubble.main.dstrand = dstBubbleMainRoot(dstSeed.Load())
		if bubble.main.dstrand == 0 {
			bubble.main.dstrand = 1
		}
		bubble.main.dstBugRand = dstBugRandMainRoot(dstSeed.Load())
		if bubble.main.dstBugRand == 0 {
			bubble.main.dstBugRand = 1
		}
		bubble.main.dstTimerSeq = 0
		bubble.main.dstHost = 0
		bubble.main.dstHostScope = nil
		bubble.main.dstProc = 0
		bubble.main.dstProcSlot = 0
		bubble.main.dstPid = int32(dstSimPID)
		dstSchedRand = dstSchedRoot(dstSeed.Load())
		dstFaultRand.Store(dstFaultRoot(dstSeed.Load()))
		dstSchedStatsReset()
		dstHeapAlloc.Store(0)
		if dstSchedKind == dstSchedPCT {
			dstSchedRootPCT()
		} else if dstSchedKind == dstSchedScheduled {
			dstScheduleReset()
		}
		dstEnsureSeq(bubble.main)
		dstEnsureSeq(gp)
	}
}

func dstSynctestSpawnDrain(pp *p, gp *g, pc uintptr, bubble *synctestBubble) {
	if dstActive() && bubble == dstSimBubble {
		synctestGCDrainPC = abi.FuncPCABIInternal(synctestGCDrain)
		drainf := synctestGCDrain
		drainfv := *(**funcval)(unsafe.Pointer(&drainf))
		bubble.gcDrain = newproc1(drainfv, gp, pc, false, waitReasonZero)
		runqput(pp, bubble.gcDrain, true)
	}
}

func dstSynctestDrainAtQuiescence(bubble *synctestBubble) {
	bubble.dstDrainAtQuiescence()
}

func dstSynctestStopGCDrain(bubble *synctestBubble) {
	bubble.dstStopGCDrain()
}

func synctestGCDrain() {
	bubble := getg().bubble
	for {
		gopark(synctestGCDrainCommit, nil, waitReasonSynctestGCDrain, traceBlockSynctest, 0)
		lock(&bubble.mu)
		exit := bubble.gcDrainExit
		unlock(&bubble.mu)
		if exit {
			return
		}
		dstDrainFinq()
		dstDrainCleanups()
	}
}

func synctestGCDrainCommit(gp *g, _ unsafe.Pointer) bool {
	return true
}

func (bubble *synctestBubble) dstDrainAtQuiescence() bool {
	if bubble.gcDrain == nil {
		if bubble.gcDrainDied {
			dstDiscardQueuedCallbacks()
		}
		return false
	}
	dstGCForce()
	if !finPending() && !cleanupPending() {
		return false
	}
	if !bubble.dstDrainParked() {
		return false
	}
	goready(bubble.gcDrain, 0)
	gopark(synctestidle_c, nil, waitReasonSynctestRun, traceBlockSynctest, 0)
	return true
}

func (bubble *synctestBubble) dstDrainParked() bool {
	drain := bubble.gcDrain
	if drain == nil {
		return false
	}
	return readgstatus(drain)&^_Gscan == _Gwaiting && drain.waitreason == waitReasonSynctestGCDrain
}

func dstDiscardQueuedCallbacks() {
	dstDiscardQueuedFinq()
	dstDiscardQueuedCleanups()
}

func dstDiscardAbandonedDrainChains() {
	if fb := dstDrainingFinq; fb != nil {
		dstDrainingFinq = nil
		lock(&finlock)
		dstDiscardFinChainLocked(fb)
		unlock(&finlock)
	}
	if b := dstDrainingCleanup; b != nil {
		dstDrainingCleanup = nil
		dstDiscardCleanupBlock(b)
	}
}

func (bubble *synctestBubble) dstStopGCDrain() {
	if bubble.gcDrain == nil && !bubble.gcDrainDied {
		return
	}
	const poolReapGenerations = 2
	poolReap := poolReapGenerations
	for {
		if bubble.gcDrain == nil {
			break
		}
		if bubble.dstDrainAtQuiescence() {
			poolReap = poolReapGenerations
			continue
		}
		poolReap--
		if poolReap == 0 {
			break
		}
	}
	if bubble.gcDrain == nil {
		if bubble.gcDrainDied {
			quiet := 0
			for quiet < poolReapGenerations {
				dstGCForce()
				if finPending() || cleanupPending() {
					dstDiscardQueuedCallbacks()
					quiet = 0
				} else {
					quiet++
				}
			}
		}
		return
	}
	if !bubble.dstDrainParked() {
		dstDiscardQueuedCallbacks()
		return
	}
	lock(&bubble.mu)
	drain := bubble.gcDrain
	bubble.gcDrainExit = true
	unlock(&bubble.mu)
	goready(drain, 0)
	gopark(synctestidle_c, nil, waitReasonSynctestRun, traceBlockSynctest, 0)
	bubble.gcDrain = nil
}

// --- time.go hooks ---

func dstTimeNowWall(now int64) (int64, int32) {
	wall := now
	if dstActive() {
		if gp := getg(); dstSimBubble != nil && gp.bubble == dstSimBubble {
			wall = dstSatAdd(wall, dstHostWallAdjust(gp.dstHost, now))
		}
	}
	return wall / (1000 * 1000 * 1000), int32(wall % (1000 * 1000 * 1000))
}

// newBaseTimer is newTimer for a DST universe-base-time timer: the dstBase
// mark must be set before the first modify (which rate-converts non-base
// timers under a drifting host), so it cannot be applied after newTimer
// returns. The body mirrors newTimer statement for statement — keep them in
// step (enforced by TestDSTNewBaseTimerMirrorsNewTimer).
//
//go:linkname newBaseTimer time.newBaseTimer
func newBaseTimer(when, period int64, f func(arg any, seq uintptr, delay int64), arg any, c *hchan) *timeTimer {
	t := new(timeTimer)
	t.timer.init(nil, nil)
	t.trace("new")
	if raceenabled {
		racerelease(unsafe.Pointer(&t.timer))
	}
	if c != nil {
		lockInit(&t.sendLock, lockRankTimerSend)
		t.isChan = true
		c.timer = &t.timer
		if c.dataqsiz == 0 {
			throw("invalid timer channel: no capacity")
		}
	}
	if bubble := getg().bubble; bubble != nil {
		t.isFake = true
	}
	t.dstBase = true
	t.modify(when, period, f, arg, 0)
	t.init = true
	return t
}

//go:linkname dstFakeTimersCountFP
func dstFakeTimersCountFP() int {
	lock(&dstFakeTimers.lock)
	defer unlock(&dstFakeTimers.lock)
	dstFakeTimersRollLocked()
	n := 0
	for t := dstFakeTimers.head.Load(); t != nil; t = t.dstFakeNext {
		n++
	}
	return n
}

func dstTimerModify(t *timer, when, period int64) (int64, int64) {
	if dstActive() && t.isFake {
		if gp := getg(); dstSimBubble != nil && gp.bubble == dstSimBubble {
			t.dstHost = gp.dstHost
			t.dstWhenShift = 0
			dstRegisterFakeTimer(t)
			if !t.dstBase {
				when, period = dstTimerArmForDrift(when, period)
			}
		}
	}
	return when, period
}

func dstTimerRand() uint32 {
	if dstActive() && getg().dstrand != 0 {
		r := dstrandUint64(getg())
		if dstSchedTraceOn {
			dstTraceRecord(dstTraceSiteTimer, dstTraceMaxN, uint32(r)&(dstTraceMaxN-1), r)
		}
		return uint32(r)
	}
	return cheaprand()
}

func dstTimerIsFuncCallback(t *timer) bool {
	if t.isChan || t.f == nil {
		return false
	}
	fv := *(**funcval)(unsafe.Pointer(&t.f))
	grf := goroutineReady
	grfv := *(**funcval)(unsafe.Pointer(&grf))
	return fv != nil && fv.fn != grfv.fn
}

func dstTimerStampLocked(t *timer) {
	if !dstActive() || !t.isFake || !dstTimerIsFuncCallback(t) {
		t.dstFuncArmed = false
		t.dstHostScope = nil
		return
	}
	gp := getg()
	if dstSimBubble == nil || gp.bubble != dstSimBubble {
		t.dstFuncArmed = false
		t.dstHostScope = nil
		return
	}
	t.dstFuncArmed = true
	t.dstEpoch = dstRunEpoch.Load()
	t.dstHost = gp.dstHost
	t.dstBoot = dstCurrentHostBootCount(gp.dstHost)
	if gp.dstKernelDepth > 0 {
		t.dstHostScope = nil
		t.dstProc = 0
		t.dstProcSlot = 0
		t.dstPid = 0
	} else {
		t.dstHostScope = gp.dstHostScope
		t.dstProc = gp.dstProc
		t.dstProcSlot = gp.dstProcSlot
		t.dstPid = gp.dstPid
	}
	gp.dstTimerSeq++
	seq := gp.dstTimerSeq
	if gp.dstrand != 0 {
		r := dstSplitmix64(gp.dstrand ^ (seq * 0x9e3779b97f4a7c15) ^ 0x54494d52_52414e44)
		if r == 0 {
			r = 1
		}
		t.dstFuncRand = r
	} else {
		t.dstFuncRand = 0
	}
	if gp.dstBugRand != 0 {
		r := dstSplitmix64(gp.dstBugRand ^ (seq * 0x9e3779b97f4a7c15) ^ 0x54494d52_42554721)
		if r == 0 {
			r = 1
		}
		t.dstFuncBugRand = r
	} else {
		t.dstFuncBugRand = 0
	}
}

func dstTimerCtxAlive(ctx *dstTimerRunCtx) bool {
	if !dstActive() || ctx.epoch != dstRunEpoch.Load() {
		return false
	}
	if ctx.pid < 0 {
		return false
	}
	if ctx.slot != 0 {
		if !dstSlotAlive(ctx.slot) {
			return false
		}
	} else if ctx.pid > 0 && dstSimEnvSet && ctx.pid != int32(dstSimPID) {
		if !dstPidAlive(ctx.host, ctx.pid) {
			return false
		}
	}
	return dstHostBootAlive(ctx.host, ctx.boot) && dstHostScopeBootAlive(ctx.hostScope)
}

func dstTimerConsumeWhenShift(t *timer, delay int64, bubble *synctestBubble) int64 {
	if t.dstWhenShift != 0 {
		delay += t.dstWhenShift
		t.dstWhenShift = 0
	}
	if bubble != nil && bubble == dstSimBubble && t.dstFuncArmed {
		pp := getg().m.p.ptr()
		pp.dstTimerCtx = dstTimerRunCtx{
			armed:       true,
			epoch:       t.dstEpoch,
			host:        t.dstHost,
			boot:        t.dstBoot,
			proc:        t.dstProc,
			slot:        t.dstProcSlot,
			pid:         t.dstPid,
			funcRand:    t.dstFuncRand,
			funcBugRand: t.dstFuncBugRand,
			hostScope:   t.dstHostScope,
		}
		if t.period <= 0 {
			t.dstHostScope = nil
		} else {
			if t.dstFuncRand != 0 {
				t.dstFuncRand = dstSplitmix64(t.dstFuncRand)
				if t.dstFuncRand == 0 {
					t.dstFuncRand = 1
				}
			}
			if t.dstFuncBugRand != 0 {
				t.dstFuncBugRand = dstSplitmix64(t.dstFuncBugRand)
				if t.dstFuncBugRand == 0 {
					t.dstFuncBugRand = 1
				}
			}
		}
	}
	return delay
}

func dstTimerRunCallback(f func(any, uintptr, int64), arg any, seq uintptr, delay int64, bubble *synctestBubble) {
	if bubble == nil || bubble != dstSimBubble {
		f(arg, seq, delay)
		return
	}
	pp := getg().m.p.ptr()
	ctx := pp.dstTimerCtx
	pp.dstTimerCtx = dstTimerRunCtx{}
	if !ctx.armed {
		f(arg, seq, delay)
		return
	}
	dstKernelRaceEnter()
	alive := dstTimerCtxAlive(&ctx)
	dstKernelRaceExit()
	if !alive {
		return
	}
	gp := getg()
	gp.dstHost = ctx.host
	gp.dstHostScope = ctx.hostScope
	gp.dstProc = ctx.proc
	gp.dstProcSlot = ctx.slot
	gp.dstPid = ctx.pid
	gp.dstrand = ctx.funcRand
	gp.dstBugRand = ctx.funcBugRand
	defer func() {
		gp.dstHost = 0
		gp.dstHostScope = nil
		gp.dstProc = 0
		gp.dstProcSlot = 0
		gp.dstPid = 0
		gp.dstrand = 0
		gp.dstBugRand = 0
	}()
	f(arg, seq, delay)
}
