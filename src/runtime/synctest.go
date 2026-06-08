// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// A synctestBubble is a set of goroutines started by synctest.Run.
type synctestBubble struct {
	mu      mutex
	timers  timers
	id      uint64 // unique id
	now     int64  // current fake time
	root    *g     // caller of synctest.Run
	waiter  *g     // caller of synctest.Wait
	main    *g     // goroutine started by synctest.Run
	waiting bool   // true if a goroutine is calling synctest.Wait
	done    bool   // true if main has exited

	// The bubble is active (not blocked) so long as running > 0 || active > 0.
	//
	// running is the number of goroutines which are not "durably blocked":
	// Goroutines which are either running, runnable, or non-durably blocked
	// (for example, blocked in a syscall).
	//
	// active is used to keep the bubble from becoming blocked,
	// even if all goroutines in the bubble are blocked.
	// For example, park_m can choose to immediately unpark a goroutine after parking it.
	// It increments the active count to keep the bubble active until it has determined
	// that the park operation has completed.
	total   int // total goroutines
	running int // non-blocked goroutines
	active  int // other sources of activity

	// DST: the persistent finalizer-drain goroutine. Under DST, finalizers are
	// not run by the async system goroutine (fing); they accumulate in finq and
	// are run on gcDrain at each quiescence point, so they run on a goroutine
	// with g.bubble set, deterministically scheduled (see dstDrainAtQuiescence).
	// nil when DST is not active for this bubble. gcDrainExit, set under
	// bubble.mu before the final wake, tells gcDrain to exit so it does not
	// outlive the Run and trip the total != 1 deadlock check.
	gcDrain     *g
	gcDrainExit bool
}

// changegstatus is called when the non-lock status of a g changes.
// It is never called with a Gscanstatus.
func (bubble *synctestBubble) changegstatus(gp *g, oldval, newval uint32) {
	// Determine whether this change in status affects the idleness of the bubble.
	// If this isn't a goroutine starting, stopping, durably blocking,
	// or waking up after durably blocking, then return immediately without
	// locking bubble.mu.
	//
	// For example, stack growth (newstack) will changegstatus
	// from _Grunning to _Gcopystack. This is uninteresting to synctest,
	// but if stack growth occurs while bubble.mu is held, we must not recursively lock.
	totalDelta := 0
	wasRunning := true
	switch oldval {
	case _Gdead, _Gdeadextra:
		wasRunning = false
		totalDelta++
	case _Gwaiting:
		if gp.waitreason.isIdleInSynctest() {
			wasRunning = false
		}
	}
	isRunning := true
	switch newval {
	case _Gdead, _Gdeadextra:
		isRunning = false
		totalDelta--
		if gp == bubble.main {
			bubble.done = true
		}
	case _Gwaiting:
		if gp.waitreason.isIdleInSynctest() {
			isRunning = false
		}
	}
	// It's possible for wasRunning == isRunning while totalDelta != 0;
	// for example, if a new goroutine is created in a non-running state.
	if wasRunning == isRunning && totalDelta == 0 {
		return
	}

	lock(&bubble.mu)
	bubble.total += totalDelta
	if wasRunning != isRunning {
		if isRunning {
			bubble.running++
		} else {
			bubble.running--
			if raceenabled && newval != _Gdead && newval != _Gdeadextra {
				// Record that this goroutine parking happens before
				// any subsequent Wait.
				racereleasemergeg(gp, bubble.raceaddr())
			}
		}
	}
	if bubble.total < 0 {
		fatal("total < 0")
	}
	if bubble.running < 0 {
		fatal("running < 0")
	}
	wake := bubble.maybeWakeLocked()
	unlock(&bubble.mu)
	if wake != nil {
		goready(wake, 0)
	}
}

// incActive increments the active-count for the bubble.
// A bubble does not become durably blocked while the active-count is non-zero.
func (bubble *synctestBubble) incActive() {
	lock(&bubble.mu)
	bubble.active++
	unlock(&bubble.mu)
}

// decActive decrements the active-count for the bubble.
func (bubble *synctestBubble) decActive() {
	lock(&bubble.mu)
	bubble.active--
	if bubble.active < 0 {
		throw("active < 0")
	}
	wake := bubble.maybeWakeLocked()
	unlock(&bubble.mu)
	if wake != nil {
		goready(wake, 0)
	}
}

// maybeWakeLocked returns a g to wake if the bubble is durably blocked.
func (bubble *synctestBubble) maybeWakeLocked() *g {
	if bubble.running > 0 || bubble.active > 0 {
		return nil
	}
	// Increment the bubble active count, since we've determined to wake something.
	// The woken goroutine will decrement the count.
	// We can't just call goready and let it increment bubble.running,
	// since we can't call goready with bubble.mu held.
	//
	// Incrementing the active count here is only necessary if something has gone wrong,
	// and a goroutine that we considered durably blocked wakes up unexpectedly.
	// Two wakes happening at the same time leads to very confusing failure modes,
	// so we take steps to avoid it happening.
	bubble.active++
	next := bubble.timers.wakeTime()
	if next > 0 && next <= bubble.now {
		// A timer is scheduled to fire. Wake the root goroutine to handle it.
		return bubble.root
	}
	if gp := bubble.waiter; gp != nil {
		// A goroutine is blocked in Wait. Wake it.
		return gp
	}
	// All goroutines in the bubble are durably blocked, and nothing has called Wait.
	// Wake the root goroutine.
	return bubble.root
}

func (bubble *synctestBubble) raceaddr() unsafe.Pointer {
	// Address used to record happens-before relationships created by the bubble.
	//
	// Wait creates a happens-before relationship between itself and
	// the blocking operations which caused other goroutines in the bubble to park.
	return unsafe.Pointer(bubble)
}

var bubbleGen atomic.Uint64 // bubble ID counter

//go:linkname synctestRun internal/synctest.Run
func synctestRun(f func()) {
	if debug.asynctimerchan.Load() != 0 {
		panic("synctest.Run not supported with asynctimerchan!=0")
	}

	gp := getg()
	if gp.bubble != nil {
		panic("synctest.Run called from within a synctest bubble")
	}
	bubble := &synctestBubble{
		id:      bubbleGen.Add(1),
		total:   1,
		running: 1,
		root:    gp,
	}
	const synctestBaseTime = 946684800000000000 // midnight UTC 2000-01-01
	bubble.now = synctestBaseTime
	lockInit(&bubble.mu, lockRankSynctest)
	lockInit(&bubble.timers.mu, lockRankTimers)

	gp.bubble = bubble
	defer func() {
		gp.bubble = nil
	}()

	// This is newproc, but also records the new g in bubble.main.
	pc := sys.GetCallerPC()
	systemstack(func() {
		fv := *(**funcval)(unsafe.Pointer(&f))
		bubble.main = newproc1(fv, gp, pc, false, waitReasonZero)
		if dstActive() {
			// Re-root the per-g DST tree at this bubble so the bubble's
			// randomness is independent of what ran before it in this process: a
			// bubble (test) is then reproducible in isolation. Without this,
			// bubble.main would inherit the caller's tree position, which depends
			// on global goroutine-creation order. Safe here: bubble.main is not
			// yet runnable on any queue. See dstBubbleRoot.
			bubble.main.dstrand = dstBubbleRoot(dstSeed.Load())
			// Re-root the scheduling RNG at this bubble too, so the seeded
			// interleaving (which runnable goroutine proceeds next) is reproducible
			// in isolation, independent of what scheduled before this bubble. See
			// dstSchedRand.
			dstSchedRand = dstSchedRoot(dstSeed.Load())
			if dstSchedKind == dstSchedPCT {
				// Re-root the PCT state (change points, step counter) for this bubble.
				// Goroutine priorities are assigned at creation (newproc1), so the
				// bubble's goroutines — created after this — get fresh priorities from
				// the just-re-rooted scheduling RNG. Note bubble.main (created at
				// newproc1 above, *before* this re-root) drew its priority from the
				// activation-rooted stream state; that is still deterministic per seed
				// (only deterministic, bubble-less draws occur between activation and
				// here at P=1), it just comes from a different stream position than its
				// children. Do not reorder the main creation after the re-root expecting
				// to "fix" this — it would change the stream and is unnecessary.
				dstSchedRootPCT()
			}
		}
		pp := getg().m.p.ptr()
		runqput(pp, bubble.main, true)
		if dstActive() {
			// Start the per-bubble finalizer drain. Created here, exactly once per
			// Run, so it advances the root's DST RNG stream a fixed number of times
			// (bubble.main was already created and independently re-rooted above, and
			// the root creates no other goroutines) — a per-quiescence spawn would
			// perturb the seeds of goroutines the root creates. It inherits g.bubble
			// from the root (gp.bubble, set above) via newproc1, so finalizers it runs
			// may touch bubble channels (invariant DST-FIN-1). See synctestGCDrain.
			//
			// Record the drain's entry PC before newproc1 so isSystemGoroutine (called
			// from newproc1) classifies it as a user goroutine and gives it the bubble.
			// Set here rather than via a static initializer to avoid an init cycle.
			synctestGCDrainPC = abi.FuncPCABIInternal(synctestGCDrain)
			drainf := synctestGCDrain
			drainfv := *(**funcval)(unsafe.Pointer(&drainf))
			bubble.gcDrain = newproc1(drainfv, gp, pc, false, waitReasonZero)
			runqput(pp, bubble.gcDrain, true)
		}
		wakep()
	})

	lock(&bubble.mu)
	bubble.active++
	for {
		unlock(&bubble.mu)
		systemstack(func() {
			// Clear gp.m.curg while running timers,
			// so timer goroutines inherit their child race context from g0.
			curg := gp.m.curg
			gp.m.curg = nil
			gp.bubble.timers.check(bubble.now, bubble)
			gp.m.curg = curg
		})
		gopark(synctestidle_c, nil, waitReasonSynctestRun, traceBlockSynctest, 0)
		// The bubble has reached quiescence (all goroutines but the root durably
		// blocked). Under DST, run the deterministic finalizer drain before
		// advancing virtual time, so the set of finalizers run by the next
		// quiescence point is the deterministic dead set (invariant DST-FIN-2).
		// No-op (no GC) when this bubble has no DST drain.
		bubble.dstDrainAtQuiescence()
		lock(&bubble.mu)
		if bubble.active < 0 {
			throw("active < 0")
		}
		next := bubble.timers.wakeTime()
		if next == 0 {
			break
		}
		if next < bubble.now {
			throw("time went backwards")
		}
		if bubble.done {
			// Time stops once the bubble's main goroutine has exited.
			break
		}
		bubble.now = next
	}
	unlock(&bubble.mu)

	// Under DST, drain any finalizers made dead by the run finishing and stop the
	// drain goroutine so it does not outlive the bubble and inflate total past the
	// root, which would spuriously trip the total != 1 deadlock check below
	// (invariant DST-FIN-3). No-op when this bubble has no DST drain.
	bubble.dstStopGCDrain()

	lock(&bubble.mu)
	total := bubble.total
	unlock(&bubble.mu)
	if raceenabled {
		// Establish a happens-before relationship between bubbled goroutines exiting
		// and Run returning.
		raceacquireg(gp, gp.bubble.raceaddr())
	}
	if total != 1 {
		var reason string
		if bubble.done {
			reason = "deadlock: main bubble goroutine has exited but blocked goroutines remain"
		} else {
			reason = "deadlock: all goroutines in bubble are blocked"
		}
		panic(synctestDeadlockError{reason: reason, bubble: bubble})
	}
	if gp.timer != nil && gp.timer.isFake {
		// Verify that we haven't marked this goroutine's sleep timer as fake.
		// This could happen if something in Run were to call timeSleep.
		throw("synctest root goroutine has a fake timer")
	}
}

type synctestDeadlockError struct {
	reason string
	bubble *synctestBubble
}

func (e synctestDeadlockError) Error() string {
	return e.reason
}

func synctestidle_c(gp *g, _ unsafe.Pointer) bool {
	lock(&gp.bubble.mu)
	canIdle := true
	if gp.bubble.running == 0 && gp.bubble.active == 1 {
		// All goroutines in the bubble have blocked or exited.
		canIdle = false
	} else {
		gp.bubble.active--
	}
	unlock(&gp.bubble.mu)
	return canIdle
}

// synctestGCDrainPC is the entry PC of synctestGCDrain, set by
// synctestRun before the first drain is created and read by isSystemGoroutine to
// identify the drain goroutine by its start function. Held in a var (rather than
// referenced via abi.FuncPCABIInternal at the isSystemGoroutine use site) so that
// isSystemGoroutine carries no static reference to the drain's body, which would
// close a package initialization cycle through mallocgc/mallocScanTable. Zero
// until the first DST bubble runs; no real goroutine has startpc 0.
var synctestGCDrainPC uintptr

// synctestGCDrain is the body of the per-bubble DST GC-callback drain goroutine
// (started by synctestRun under dstActive). It parks until the synctest driver
// wakes it at a quiescence point, runs every queued finalizer and cleanup — on
// this goroutine, whose g.bubble is the Run's bubble, so a callback may do bubble
// channel ops without fatal (invariants DST-FIN-1, DST-CLEANUP-1) — then re-parks.
// It exits when the driver sets gcDrainExit at Run end, so it does not outlive the
// bubble. Finalizers run before cleanups, deterministically.
//
// Identified by PC in isSystemGoroutine so it is treated as a user goroutine; do
// not rename without updating that check.
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

// synctestGCDrainCommit commits the drain goroutine's park. The wake is
// driven entirely by the synctest driver (dstDrainAtQuiescence / stop), which
// only wakes the drain once it is parked and the bubble is quiescent, so there is
// no concurrent wake to guard against; the park unconditionally commits.
func synctestGCDrainCommit(gp *g, _ unsafe.Pointer) bool {
	return true
}

// dstDrainAtQuiescence runs the deterministic finalizer + cleanup drain at a
// synctest quiescence point. It is called by the driver after the bubble has
// reached quiescence (all goroutines but the root durably blocked), with
// bubble.mu NOT held. It is a no-op (and runs no GC) when the bubble has no DST
// drain.
//
// It forces one fresh STW GC (deterministic under DST), which discovers the
// objects unreachable from the quiescent live set and queues their finalizers and
// cleanups (the GC's sweep flushes per-P cleanup blocks; folding in any already
// queued by mid-burst heap triggers), then wakes gcDrain to run them, so the
// finalizer/cleanup set observed at this quiescence is the deterministic dead set
// (invariants DST-FIN-2, DST-CLEANUP-2).
//
// Exactly one GC per *quiescence* (not a fixpoint): an object kept alive only by
// another finalizable object's still-pending callback is *in* the quiescent live
// set (the GC marks it to keep it for that callback), so it is not yet dead here
// and must not run — its callback runs at a later quiescence once the earlier one
// has run, exactly as production resolves finalizer/cleanup chains across
// successive GC cycles. An unbounded per-quiescence fixpoint would also loop
// forever on a callback that re-registers itself (SetFinalizer/AddCleanup of the
// object from its own callback). At *Run end*, where there is no later quiescence,
// dstStopGCDrain instead loops this a bounded number of rounds to resolve chains
// fully in-bubble (see dstRunEndDrainRounds); that is why this returns whether it
// made progress.
//
// The drain itself runs *all* currently-queued finalizers then cleanups
// (dstDrainFinq/dstDrainCleanups loop until empty), absorbing any a callback
// queues by allocating. The gopark then parks the root until the drain has
// finished and the bubble is quiescent again — which also waits for any bubble
// goroutine a callback unblocked (e.g. one that sends on a channel another
// goroutine is receiving on) to run and re-block, before virtual time advances.
// It reports whether it drained a level (the GC discovered dead finalizable/
// cleanup-bearing objects and ran their callbacks); false means the GC found
// nothing, which the Run-end fixpoint uses to detect that a finalizer/cleanup
// chain is fully resolved.
func (bubble *synctestBubble) dstDrainAtQuiescence() bool {
	if bubble.gcDrain == nil {
		return false
	}
	GC()
	if !finPending() && !cleanupPending() {
		return false
	}
	// Wake the drain to run the queued finalizers and cleanups, then park the root
	// until the drain has finished and the bubble is quiescent again. Reuses the
	// driver's own idle park, so the active/running accounting is maintained
	// exactly as in the main loop.
	goready(bubble.gcDrain, 0)
	gopark(synctestidle_c, nil, waitReasonSynctestRun, traceBlockSynctest, 0)
	return true
}

// dstRunEndDrainRounds bounds the Run-end finalizer/cleanup fixpoint so a callback
// that re-registers itself (SetFinalizer(p, fn) inside fn, or AddCleanup of p
// from p's own cleanup) cannot spin the drain forever. Real finalizer/cleanup
// chains are shallow, so the loop almost always converges in one or two rounds;
// the cap only bounds the pathological self-resurrecting case, whose residual
// callbacks fall through to the post-Run reap as they did before this fixpoint.
const dstRunEndDrainRounds = 256

// dstStopGCDrain runs a final drain and stops the drain goroutine at Run
// end. Called by the driver after the run loop, when the bubble is quiescent,
// with bubble.mu NOT held. No-op when the bubble has no DST drain.
//
// The drain is a bubble goroutine and so counts toward bubble.total; if it
// survived the Run, total would be 2 (root + drain) at a clean exit and trip the
// total != 1 deadlock check. So tell it to exit, wake it, and wait for it to die
// (its exit decrements total) before the driver reads total (invariant DST-FIN-3).
func (bubble *synctestBubble) dstStopGCDrain() {
	if bubble.gcDrain == nil {
		return
	}
	// Run finalizers/cleanups made dead by the run finishing (e.g. bubble.main's
	// locals), draining finalizer/cleanup *chains* to a fixpoint. Unlike a
	// quiescence point during the run — where exactly one GC runs, so a chain
	// resolves one level per quiescence as in production (DST-FIN-2) — at Run end
	// the SUT has exited and there is no later quiescence, so a chained callback
	// left pending (object B reachable only through object A's still-pending
	// finalizer) would leak to the post-Run reap on the async fing/cleanup
	// goroutine (g.bubble == nil) and a channel-touching tail would fatal. Loop
	// until a GC discovers nothing new, so the whole chain runs in-bubble; bounded
	// by dstRunEndDrainRounds against a self-re-registering callback.
	for i := 0; i < dstRunEndDrainRounds; i++ {
		if !bubble.dstDrainAtQuiescence() {
			break
		}
	}
	lock(&bubble.mu)
	bubble.gcDrainExit = true
	unlock(&bubble.mu)
	goready(bubble.gcDrain, 0)
	// Park the root until the drain has observed gcDrainExit and exited; its
	// _Grunning->_Gdead transition decrements total and wakes the root.
	gopark(synctestidle_c, nil, waitReasonSynctestRun, traceBlockSynctest, 0)
	bubble.gcDrain = nil
}

//go:linkname synctestWait internal/synctest.Wait
func synctestWait() {
	gp := getg()
	if gp.bubble == nil {
		panic("goroutine is not in a bubble")
	}
	lock(&gp.bubble.mu)
	// We use a bubble.waiting bool to detect simultaneous calls to Wait rather than
	// checking to see if bubble.waiter is non-nil. This avoids a race between unlocking
	// bubble.mu and setting bubble.waiter while parking.
	if gp.bubble.waiting {
		unlock(&gp.bubble.mu)
		panic("wait already in progress")
	}
	gp.bubble.waiting = true
	unlock(&gp.bubble.mu)
	gopark(synctestwait_c, nil, waitReasonSynctestWait, traceBlockSynctest, 0)

	lock(&gp.bubble.mu)
	gp.bubble.active--
	if gp.bubble.active < 0 {
		throw("active < 0")
	}
	gp.bubble.waiter = nil
	gp.bubble.waiting = false
	unlock(&gp.bubble.mu)

	// Establish a happens-before relationship on the activity of the now-blocked
	// goroutines in the bubble.
	if raceenabled {
		raceacquireg(gp, gp.bubble.raceaddr())
	}
}

func synctestwait_c(gp *g, _ unsafe.Pointer) bool {
	lock(&gp.bubble.mu)
	if gp.bubble.running == 0 && gp.bubble.active == 0 {
		// This shouldn't be possible, since gopark increments active during unlockf.
		throw("running == 0 && active == 0")
	}
	gp.bubble.waiter = gp
	unlock(&gp.bubble.mu)
	return true
}

//go:linkname synctest_isInBubble internal/synctest.IsInBubble
func synctest_isInBubble() bool {
	return getg().bubble != nil
}

//go:linkname synctest_acquire internal/synctest.acquire
func synctest_acquire() any {
	if bubble := getg().bubble; bubble != nil {
		bubble.incActive()
		return bubble
	}
	return nil
}

//go:linkname synctest_release internal/synctest.release
func synctest_release(bubble any) {
	bubble.(*synctestBubble).decActive()
}

//go:linkname synctest_inBubble internal/synctest.inBubble
func synctest_inBubble(bubble any, f func()) {
	gp := getg()
	if gp.bubble != nil {
		panic("goroutine is already bubbled")
	}
	gp.bubble = bubble.(*synctestBubble)
	defer func() {
		gp.bubble = nil
	}()
	f()
}

// specialBubble is a special used to associate objects with bubbles.
type specialBubble struct {
	_        sys.NotInHeap
	special  special
	bubbleid uint64
}

// Keep these in sync with internal/synctest.
const (
	bubbleAssocUnbubbled     = iota // not associated with any bubble
	bubbleAssocCurrentBubble        // associated with the current bubble
	bubbleAssocOtherBubble          // associated with a different bubble
)

// getOrSetBubbleSpecial checks the special record for p's bubble membership.
//
// If add is true and p is not associated with any bubble,
// it adds a special record for p associating it with bubbleid.
//
// It returns ok==true if p is associated with bubbleid
// (including if a new association was added),
// and ok==false if not.
func getOrSetBubbleSpecial(p unsafe.Pointer, bubbleid uint64, add bool) (assoc int) {
	span := spanOfHeap(uintptr(p))
	if span == nil {
		// This is probably a package var.
		// We can't attach a special to it, so always consider it unbubbled.
		return bubbleAssocUnbubbled
	}

	// Ensure that the span is swept.
	// Sweeping accesses the specials list w/o locks, so we have
	// to synchronize with it. And it's just much safer.
	mp := acquirem()
	span.ensureSwept()

	offset := uintptr(p) - span.base()

	lock(&span.speciallock)

	// Find splice point, check for existing record.
	iter, exists := span.specialFindSplicePoint(offset, _KindSpecialBubble)
	if exists {
		// p is already associated with a bubble.
		// Return true iff it's the same bubble.
		s := (*specialBubble)((unsafe.Pointer)(*iter))
		if s.bubbleid == bubbleid {
			assoc = bubbleAssocCurrentBubble
		} else {
			assoc = bubbleAssocOtherBubble
		}
	} else if add {
		// p is not associated with a bubble,
		// and we've been asked to add an association.
		lock(&mheap_.speciallock)
		s := (*specialBubble)(mheap_.specialBubbleAlloc.alloc())
		unlock(&mheap_.speciallock)
		s.bubbleid = bubbleid
		s.special.kind = _KindSpecialBubble
		s.special.offset = offset
		s.special.next = *iter
		*iter = (*special)(unsafe.Pointer(s))
		spanHasSpecials(span)
		assoc = bubbleAssocCurrentBubble
	} else {
		// p is not associated with a bubble.
		assoc = bubbleAssocUnbubbled
	}

	unlock(&span.speciallock)
	releasem(mp)

	return assoc
}

// synctest_associate associates p with the current bubble.
// It returns false if p is already associated with a different bubble.
//
//go:linkname synctest_associate internal/synctest.associate
func synctest_associate(p unsafe.Pointer) int {
	return getOrSetBubbleSpecial(p, getg().bubble.id, true)
}

// synctest_disassociate disassociates p from its bubble.
//
//go:linkname synctest_disassociate internal/synctest.disassociate
func synctest_disassociate(p unsafe.Pointer) {
	removespecial(p, _KindSpecialBubble)
}

// synctest_isAssociated reports whether p is associated with the current bubble.
//
//go:linkname synctest_isAssociated internal/synctest.isAssociated
func synctest_isAssociated(p unsafe.Pointer) bool {
	return getOrSetBubbleSpecial(p, getg().bubble.id, false) == bubbleAssocCurrentBubble
}
