// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector: finalizers and block profiling.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"internal/runtime/gc"
	"internal/runtime/sys"
	"unsafe"
)

const finBlockSize = 4 * 1024

// finBlock is an block of finalizers to be executed. finBlocks
// are arranged in a linked list for the finalizer queue.
//
// finBlock is allocated from non-GC'd memory, so any heap pointers
// must be specially handled. GC currently assumes that the finalizer
// queue does not grow during marking (but it can shrink).
type finBlock struct {
	_       sys.NotInHeap
	alllink *finBlock
	next    *finBlock
	cnt     uint32
	_       int32
	fin     [(finBlockSize - 2*goarch.PtrSize - 2*4) / unsafe.Sizeof(finalizer{})]finalizer
}

var fingStatus atomic.Uint32

// finalizer goroutine status.
const (
	fingUninitialized uint32 = iota
	fingCreated       uint32 = 1 << (iota - 1)
	fingRunningFinalizer
	fingWait
	fingWake
)

var (
	finlock      mutex     // protects the following variables
	fing         *g        // goroutine that runs finalizers
	finq         *finBlock // list of finalizers that are to be executed
	finc         *finBlock // cache of free blocks
	finptrmask   [finBlockSize / goarch.PtrSize / 8]byte
	finqueued    uint64 // monotonic count of queued finalizers
	finexecuted  uint64 // monotonic count of executed finalizers
	findiscarded uint64 // monotonic count discarded because the owning process invocation died

	// Finalizers queued before a DST run are process-level work, not part of the
	// run's bubble. They are detached before dstActive is set, ignored by the
	// in-bubble drain, and released back to fing after dstDeactivate.
	dstDeferredFinq *finBlock

	// dstDrainingFinq is the chain the DST bubble drain is currently running
	// (runFinqBlocks publishes it block-by-block on the drain path). Non-nil
	// only while the drain is mid-chain, so a callback panic or Goexit — which
	// abandons the drain's frame — leaves the unrun remainder discoverable for
	// dstDiscardQueuedFinq instead of leaking it. Protected by the single-P
	// cooperative schedule: only the drain writes it, and the driver reads it
	// only after the drain has died.
	dstDrainingFinq *finBlock
)

// Run-local finalizer queue accounting. finqueued/finexecuted are process-global
// and include prior-run callbacks that may already be running on fing. While DST
// is active, pending means queued-by-this-run but not yet executed-by-this-run.
var dstFinqRunBaseQueued, dstFinqRunExecuted atomic.Uint64

var allfin *finBlock // list of all blocks

// NOTE: Layout known to queuefinalizer.
type finalizer struct {
	fn       *funcval       // function to call (may be a heap pointer)
	arg      unsafe.Pointer // ptr to object (may be a heap pointer)
	nret     uintptr        // bytes of return values from fn
	fint     *_type         // type of first argument of fn
	ot       *ptrtype       // type of ptr to object (may be a heap pointer)
	dstSeq   uintptr        // DST per-run registration sequence (from specialfinalizer.dstSeq); the bubble drain sorts by it. 0 for non-DST/foreign.
	dstEpoch uint64         // DST run generation paired with dstPid
	dstPid   int32          // DST process invocation owner; 0 for process-level or foreign work
}

// lockRankMayQueueFinalizer records the lock ranking effects of a
// function that may call queuefinalizer.
func lockRankMayQueueFinalizer() {
	lockWithRankMayAcquire(&finlock, getLockRank(&finlock))
}

func queuefinalizer(p unsafe.Pointer, fn *funcval, nret uintptr, fint *_type, ot *ptrtype, dstEpoch uint64, dstSeq uintptr, dstPid int32) {
	if gcphase != _GCoff {
		// Currently we assume that the finalizer queue won't
		// grow during marking so we don't have to rescan it
		// during mark termination. If we ever need to lift
		// this assumption, we can do it by adding the
		// necessary barriers to queuefinalizer (which it may
		// have automatically).
		throw("queuefinalizer during GC")
	}

	lock(&finlock)

	q := &finq
	deferred := false
	if dstActive() && dstEpoch != dstRunEpoch.Load() {
		// Not this run's work: the finalizer was registered before the run, by
		// a goroutine outside the simulation bubble, or by a previous run (see
		// dstCallbackEpoch). Defer it past dstDeactivate with the pre-bubble
		// queue rather than letting the bubble drain run it: the drained set
		// must be a pure function of the run's own activity, and a foreign
		// callback would advance the drain's per-g RNG stream. finqueued still
		// counts it (process truth — it executes on fing after release), so
		// the run ledger balances it as handled below, exactly as the
		// dead-drain discard does; finPending stays exact.
		q = &dstDeferredFinq
		dstFinqRunExecuted.Add(1)
		deferred = true
	}
	if *q == nil || (*q).cnt == uint32(len((*q).fin)) {
		block := finAllocBlockLocked()
		block.next = *q
		*q = block
	}
	f := &(*q).fin[(*q).cnt]
	atomic.Xadd(&(*q).cnt, +1) // Sync with markroots
	f.fn = fn
	f.nret = nret
	f.fint = fint
	f.ot = ot
	f.arg = p
	f.dstSeq = dstSeq // carried from the special; the bubble drain sorts its batch by it
	f.dstEpoch = dstEpoch
	f.dstPid = dstPid
	finqueued++
	unlock(&finlock)
	if !deferred {
		// A deferred entry must not arm a fing wake mid-run; the release at
		// dstDeactivate arms it when the work actually becomes fing's.
		fingStatus.Or(fingWake)
	}
}

// finAllocBlockLocked returns a free finBlock (from the cache, or freshly
// allocated and registered on allfin so markroots scans it). Caller holds
// finlock.
func finAllocBlockLocked() *finBlock {
	if finc == nil {
		finc = (*finBlock)(persistentalloc(finBlockSize, 0, &memstats.gcMiscSys))
		finc.alllink = allfin
		allfin = finc
		if finptrmask[0] == 0 {
			// Build pointer mask for Finalizer array in block.
			// Check assumptions made in finalizer1 array above.
			words := unsafe.Sizeof(finalizer{}) / goarch.PtrSize
			if (words != 8 && words != 9 ||
				unsafe.Offsetof(finalizer{}.fn) != 0 ||
				unsafe.Offsetof(finalizer{}.arg) != goarch.PtrSize ||
				unsafe.Offsetof(finalizer{}.nret) != 2*goarch.PtrSize ||
				unsafe.Offsetof(finalizer{}.fint) != 3*goarch.PtrSize ||
				unsafe.Offsetof(finalizer{}.ot) != 4*goarch.PtrSize ||
				unsafe.Offsetof(finalizer{}.dstSeq) != 5*goarch.PtrSize ||
				unsafe.Offsetof(finalizer{}.dstEpoch) != 6*goarch.PtrSize ||
				unsafe.Offsetof(finalizer{}.dstPid) != 6*goarch.PtrSize+unsafe.Sizeof(uint64(0))) {
				throw("finalizer out of sync")
			}
			for i := range finptrmask {
				var mask byte
				for bit := 0; bit < 8; bit++ {
					word := i*8 + bit
					if word%int(words) == 0 || word%int(words) == 1 || word%int(words) == 3 || word%int(words) == 4 {
						mask |= 1 << bit
					}
				}
				finptrmask[i] = mask
			}
		}
	}
	block := finc
	finc = block.next
	return block
}

//go:nowritebarrier
func iterate_finq(callback func(*funcval, unsafe.Pointer, uintptr, *_type, *ptrtype)) {
	for fb := allfin; fb != nil; fb = fb.alllink {
		for i := uint32(0); i < fb.cnt; i++ {
			f := &fb.fin[i]
			callback(f.fn, f.arg, f.nret, f.fint, f.ot)
		}
	}
}

func wakefing() *g {
	if ok := fingStatus.CompareAndSwap(fingCreated|fingWait|fingWake, fingCreated); ok {
		return fing
	}
	return nil
}

func createfing() {
	// Not under DST: fing is created via `go runFinalizers()`, which draws from
	// the calling goroutine's per-g DST RNG stream (newproc1) and persists across
	// Runs (fingStatus stays fingCreated), so creating it during a Run — e.g. on a
	// SUT whose first SetFinalizer is inside dst.Run — would perturb the bubble
	// goroutine's stream in a process-history-dependent way, breaking the
	// reproducible-in-isolation property. Under DST the bubble drain runs
	// finalizers instead, so fing is not needed during a Run; this mirrors the
	// createGs gate for cleanups (mcleanup.go). When fing already exists (the
	// common case — a stdlib finalizer creates it at startup) this is a no-op.
	if dstActive() {
		return
	}
	// start the finalizer goroutine exactly once
	if fingStatus.Load() == fingUninitialized && fingStatus.CompareAndSwap(fingUninitialized, fingCreated) {
		go runFinalizers()
	}
}

func finalizercommit(gp *g, lock unsafe.Pointer) bool {
	unlock((*mutex)(lock))
	// fingStatus should be modified after fing is put into a waiting state
	// to avoid waking fing in running state, even if it is about to be parked.
	fingStatus.Or(fingWait)
	return true
}

func finalizercommitDSTBlocked(gp *g, lock unsafe.Pointer) bool {
	unlock((*mutex)(lock))
	// Leave a wake request behind: fing may be parked with pre-bubble work still
	// held in its local runFinqBlocks frame, so no later queuefinalizer call is
	// guaranteed to wake it after DST deactivates.
	fingStatus.Or(fingWait | fingWake)
	return true
}

func dstParkFingIfBlocked() bool {
	if !dstCallbackWorkersBlocked() {
		return false
	}
	lock(&finlock)
	if !dstCallbackWorkersBlocked() {
		unlock(&finlock)
		return false
	}
	gopark(finalizercommitDSTBlocked, unsafe.Pointer(&finlock), waitReasonFinalizerWait, traceBlockSystemGoroutine, 1)
	return true
}

func finReadQueueStats() (queued, executed uint64) {
	lock(&finlock)
	queued = finqueued
	executed = finexecuted
	unlock(&finlock)
	return
}

// This is the goroutine that runs all of the finalizers.
func runFinalizers() {
	gp := getg()
	lock(&finlock)
	fing = gp
	unlock(&finlock)

	for {
		lock(&finlock)
		if dstCallbackWorkersBlocked() {
			gopark(finalizercommitDSTBlocked, unsafe.Pointer(&finlock), waitReasonFinalizerWait, traceBlockSystemGoroutine, 1)
			continue
		}
		fb := finq
		finq = nil
		if fb == nil {
			gopark(finalizercommit, unsafe.Pointer(&finlock), waitReasonFinalizerWait, traceBlockSystemGoroutine, 1)
			continue
		}
		unlock(&finlock)
		runFinqBlocks(fb)
	}
}

// runFinqBlocks runs every finalizer in the chain of blocks starting at fb, in
// reverse-LIFO order within each block, and returns the drained blocks to the
// free cache. The caller must not hold finlock and must have already detached fb
// from finq (set finq = nil) under finlock.
//
// It is shared by the async system finalizer goroutine (runFinalizers) and the
// DST bubble drain (dstDrainFinq), so a finalizer runs identically whichever
// goroutine drives it.
func runFinqBlocks(fb *finBlock) {
	gp := getg()
	onFing := gp == fing
	// On the DST bubble drain, publish the chain being run (dstDrainingFinq) so
	// a callback panic or Goexit — which abandons this frame — leaves the unrun
	// remainder discoverable for the teardown discard (dstDiscardQueuedFinq).
	// The ledger is kept per-entry on this path (the block-end add below is
	// skipped) so a mid-block death leaves finqueued == finexecuted+discarded
	// exact.
	//
	// Invariant the publish window relies on: drain death originates only
	// inside the callback's reflectcall, where dstDrainingFinq points at the
	// current (not yet freed) block. After the last entry of a block runs, the
	// block is freed while the pointer still references it — that window is
	// straight-line code (no park, no recoverable panic), so the discard, which
	// runs only after the drain has died, can never observe it. Do not add a
	// park or preemption point between the free below and the next publish.
	onDrain := dstBuild && gp.bubble != nil && gp == gp.bubble.gcDrain
	var (
		frame    unsafe.Pointer
		framecap uintptr
	)
	argRegs := intArgRegs
	if raceenabled {
		racefingo()
	}
	for fb != nil {
		if onDrain { // onDrain embeds dstBuild: folds untagged
			dstDrainingFinq = fb
		}
		n := fb.cnt
		var executed uint64
		for i := n; i > 0; i-- {
			for dstBuild && onFing && dstParkFingIfBlocked() {
			}
			f := &fb.fin[i-1]

			var regs abi.RegArgs
			// The args may be passed in registers or on stack. Even for
			// the register case, we still need the spill slots.
			// TODO: revisit if we remove spill slots.
			//
			// Unfortunately because we can have an arbitrary
			// amount of returns and it would be complex to try and
			// figure out how many of those can get passed in registers,
			// just conservatively assume none of them do.
			framesz := unsafe.Sizeof((any)(nil)) + f.nret
			if framecap < framesz {
				// The frame does not contain pointers interesting for GC,
				// all not yet finalized objects are stored in finq.
				// If we do not mark it as FlagNoScan,
				// the last finalized object is not collected.
				frame = mallocgc(framesz, nil, true)
				framecap = framesz
			}
			if f.fint == nil {
				throw("missing type in finalizer")
			}
			r := frame
			if argRegs > 0 {
				r = unsafe.Pointer(&regs.Ints)
			} else {
				// frame is effectively uninitialized
				// memory. That means we have to clear
				// it before writing to it to avoid
				// confusing the write barrier.
				*(*[2]uintptr)(frame) = [2]uintptr{}
			}
			switch f.fint.Kind() {
			case abi.Pointer:
				// direct use of pointer
				*(*unsafe.Pointer)(r) = f.arg
			case abi.Interface:
				ityp := (*interfacetype)(unsafe.Pointer(f.fint))
				// set up with empty interface
				(*eface)(r)._type = &f.ot.Type
				(*eface)(r).data = f.arg
				if len(ityp.Methods) != 0 {
					// convert to interface with methods
					// this conversion is guaranteed to succeed - we checked in SetFinalizer
					(*iface)(r).tab = assertE2I(ityp, (*eface)(r)._type)
				}
			default:
				throw("bad type kind in finalizer")
			}
			if onFing {
				fingStatus.Or(fingRunningFinalizer)
			}
			invoked := true
			if dstBuild {
				invoked = dstCallbackOwnerAlive(f.dstEpoch, f.dstPid)
			}
			if invoked {
				if dstBuild {
					oldPid := gp.dstPid
					if onDrain && f.dstPid > 0 {
						gp.dstPid = f.dstPid
					}
					reflectcall(nil, unsafe.Pointer(f.fn), frame, uint32(framesz), uint32(framesz), uint32(framesz), &regs)
					gp.dstPid = oldPid
				} else {
					reflectcall(nil, unsafe.Pointer(f.fn), frame, uint32(framesz), uint32(framesz), uint32(framesz), &regs)
				}
				executed++
			}
			if onFing {
				fingStatus.And(^fingRunningFinalizer)
			}

			// Drop finalizer queue heap references
			// before hiding them from markroot.
			// This also ensures these will be
			// clear if we reuse the finalizer.
			f.fn = nil
			f.arg = nil
			f.ot = nil
			atomic.Store(&fb.cnt, i-1)
			if onDrain {
				lock(&finlock)
				if invoked {
					finexecuted++
				} else {
					findiscarded++
				}
				unlock(&finlock)
				if dstActive() {
					dstFinqRunExecuted.Add(1)
				}
			}
		}
		next := fb.next
		lock(&finlock)
		if !onDrain {
			finexecuted += executed
			findiscarded += uint64(n) - executed
		}
		fb.next = finc
		finc = fb
		unlock(&finlock)
		fb = next
	}
	if onDrain {
		dstDrainingFinq = nil
	}
}

// finPending reports whether any finalizers have been queued but not yet
// executed. Used by the DST quiescence drain to decide whether to wake the drain
// goroutine and to detect when the finalizer fixpoint is reached. finqueued and
// finexecuted are process-cumulative, but their equality is exact: they are equal
// iff finq is empty and no finalizer is mid-run.
func finPending() bool {
	if dstActive() {
		lock(&finlock)
		queued := finqueued - dstFinqRunBaseQueued.Load()
		unlock(&finlock)
		return queued != dstFinqRunExecuted.Load()
	}
	lock(&finlock)
	pending := finqueued != finexecuted+findiscarded
	unlock(&finlock)
	return pending
}

func dstResetFinqRunCounters() {
	lock(&finlock)
	dstFinqRunBaseQueued.Store(finqueued)
	unlock(&finlock)
	dstFinqRunExecuted.Store(0)
}

// dstDeferPreBubbleFinq detaches finalizers queued before a DST bubble starts so
// they cannot run in the bubble drain. The detached blocks still count in
// finqueued, but finPending subtracts them while dstActive so the run's fixpoint
// only observes callbacks queued by the run itself.
func dstDeferPreBubbleFinq() {
	lock(&finlock)
	fb := finq
	finq = nil
	if fb != nil {
		last := fb
		for last.next != nil {
			last = last.next
		}
		last.next = dstDeferredFinq
		dstDeferredFinq = fb
	}
	unlock(&finlock)
}

// dstReleaseDeferredFinq returns pre-bubble finalizers to the ordinary finalizer
// goroutine after a DST run is deactivated. They never execute with dstActive set
// and never enter the run's bubble drain.
func dstReleaseDeferredFinq() {
	// The finalizer goroutine may not exist: its creation is deferred while
	// DST is active (createfing's gate), so in a process whose FIRST
	// SetFinalizer happened inside a run the released chain would otherwise
	// strand with no fing to wake. DST is already inactive here.
	createfing()
	lock(&finlock)
	fb := dstDeferredFinq
	dstDeferredFinq = nil
	if fb != nil {
		last := fb
		for last.next != nil {
			last = last.next
		}
		last.next = finq
		finq = fb
	}
	unlock(&finlock)
	if fb != nil {
		fingStatus.Or(fingWake)
	}
}

// dstDrainFinq runs every currently-queued finalizer on the calling goroutine,
// returning once finq is empty. Used by the DST bubble finalizer drain so that
// finalizers run on a bubble goroutine (deterministically scheduled, with
// g.bubble set) rather than on the async system finalizer goroutine fing — which
// has g.bubble == nil and would fatal on a bubble channel op (invariant
// DST-FIN-1). The caller must not hold finlock.
func dstDrainFinq() {
	for {
		lock(&finlock)
		fb := finq
		finq = nil
		unlock(&finlock)
		if fb == nil {
			return
		}
		// Order the batch by registration sequence before running it, so two
		// same-cycle finalizers with interacting side effects run in a replay-stable
		// order (gc.md D4) rather than heap-address sweep order.
		dstSortFinqBySeq(fb)
		// Ledger (finexecuted and dstFinqRunExecuted) is kept per-entry inside
		// runFinqBlocks on the drain path, so a callback panic or Goexit cannot
		// lose a block's worth of accounting.
		runFinqBlocks(fb)
	}
}

// dstSortFinqBySeq reorders the detached finalizer chain fb IN PLACE so runFinqBlocks —
// which executes reverse-LIFO within each block, blocks in chain order — runs the
// finalizers in ASCENDING registration sequence (finalizer.dstSeq), the replay-stable
// order (gc.md D4). It preserves the block structure (same blocks, same per-block
// counts), so the discard/ledger machinery in runFinqBlocks is untouched. Its scratch
// slices are the drain goroutine's own (SUT-external) allocations of the deterministic
// batch size, so they add a deterministic, replay-safe amount to the heap trigger.
func dstSortFinqBySeq(fb *finBlock) {
	n := 0
	for b := fb; b != nil; b = b.next {
		n += int(b.cnt)
	}
	if n <= 1 {
		return
	}
	all := make([]finalizer, n)
	k := 0
	for b := fb; b != nil; b = b.next {
		for i := 0; i < int(b.cnt); i++ {
			all[k] = b.fin[i]
			k++
		}
	}
	dstSortFinalizersBySeq(all)
	// Re-lay so runFinqBlocks's reverse-per-block-in-chain-order traversal is ascending:
	// filling fin[cnt-1], fin[cnt-2], … with consecutive ascending entries makes the
	// block execute them fin[cnt-1] first (ascending), and blocks run in chain order.
	k = 0
	for b := fb; b != nil; b = b.next {
		cnt := int(b.cnt)
		for i := 0; i < cnt; i++ {
			b.fin[cnt-1-i] = all[k]
			k++
		}
	}
}

// dstSortFinalizersBySeq sorts a ascending by dstSeq — a stable, non-recursive
// bottom-up merge sort (package runtime cannot import sort). O(n log n), deterministic.
func dstSortFinalizersBySeq(a []finalizer) {
	n := len(a)
	if n <= 1 {
		return
	}
	buf := make([]finalizer, n)
	src, dst := a, buf
	for width := 1; width < n; width *= 2 {
		for i := 0; i < n; i += 2 * width {
			lo := i
			mid := min(i+width, n)
			hi := min(i+2*width, n)
			l, r, k := lo, mid, lo
			for l < mid && r < hi {
				if src[l].dstSeq <= src[r].dstSeq {
					dst[k] = src[l]
					l++
				} else {
					dst[k] = src[r]
					r++
				}
				k++
			}
			for l < mid {
				dst[k] = src[l]
				l++
				k++
			}
			for r < hi {
				dst[k] = src[r]
				r++
				k++
			}
		}
		src, dst = dst, src
	}
	if &src[0] != &a[0] {
		copy(a, src)
	}
}

// dstDiscardQueuedFinq discards every finalizer queued by the run — finq plus
// the unrun remainder of a chain a dying drain abandoned (dstDrainingFinq) —
// without running them. Used at teardown after the drain died (gcDrainDied):
// nothing in-bubble can run them anymore, and releasing them to fing would run
// bubble-stamped callbacks on a bubble-less goroutine, which fatals on a bubble
// channel op (invariant DST-FIN-1). Finalizers are best-effort by the
// SetFinalizer contract; the discard is deterministic. Entries are accounted as
// executed so the queue ledger stays exact, and the blocks are returned to the
// free cache so markroot stops pinning their arguments.
func dstDiscardQueuedFinq() {
	lock(&finlock)
	fb := finq
	finq = nil
	if dstDrainingFinq != nil {
		if fb != nil {
			last := dstDrainingFinq
			for last.next != nil {
				last = last.next
			}
			last.next = fb
		}
		fb = dstDrainingFinq
		dstDrainingFinq = nil
	}
	n := dstDiscardFinChainLocked(fb)
	unlock(&finlock)
	if n != 0 && dstActive() {
		dstFinqRunExecuted.Add(int64(n))
	}
}

// dstDiscardFinChainLocked nils the entries of every block in the chain,
// accounts them as executed in the process-global ledger, and returns the
// blocks to the free cache. Caller holds finlock. Returns the number of
// entries discarded.
func dstDiscardFinChainLocked(fb *finBlock) uint64 {
	var n uint64
	for fb != nil {
		next := fb.next
		cnt := atomic.Load(&fb.cnt)
		for i := cnt; i > 0; i-- {
			f := &fb.fin[i-1]
			f.fn = nil
			f.arg = nil
			f.ot = nil
		}
		n += uint64(cnt)
		atomic.Store(&fb.cnt, 0)
		fb.next = finc
		finc = fb
		fb = next
	}
	finexecuted += n
	return n
}

func isGoPointerWithoutSpan(p unsafe.Pointer) bool {
	// 0-length objects are okay.
	if p == unsafe.Pointer(&zerobase) {
		return true
	}

	// Global initializers might be linker-allocated.
	//	var Foo = &Object{}
	//	func main() {
	//		runtime.SetFinalizer(Foo, nil)
	//	}
	// The relevant segments are: noptrdata, data, bss, noptrbss.
	// We cannot assume they are in any order or even contiguous,
	// due to external linking.
	for datap := &firstmoduledata; datap != nil; datap = datap.next {
		if datap.noptrdata <= uintptr(p) && uintptr(p) < datap.enoptrdata ||
			datap.data <= uintptr(p) && uintptr(p) < datap.edata ||
			datap.bss <= uintptr(p) && uintptr(p) < datap.ebss ||
			datap.noptrbss <= uintptr(p) && uintptr(p) < datap.enoptrbss {
			return true
		}
	}
	return false
}

// blockUntilEmptyFinalizerQueue blocks until either the finalizer
// queue is emptied (and the finalizers have executed) or the timeout
// is reached. Returns true if the finalizer queue was emptied.
// This is used by the runtime, sync, and unique tests.
func blockUntilEmptyFinalizerQueue(timeout int64) bool {
	start := nanotime()
	for nanotime()-start < timeout {
		lock(&finlock)
		// We know the queue has been drained when both finq is nil
		// and the finalizer g has stopped executing.
		empty := finq == nil
		empty = empty && readgstatus(fing) == _Gwaiting && fing.waitreason == waitReasonFinalizerWait
		unlock(&finlock)
		if empty {
			return true
		}
		Gosched()
	}
	return false
}

// SetFinalizer sets the finalizer associated with obj to the provided
// finalizer function. When the garbage collector finds an unreachable block
// with an associated finalizer, it clears the association and runs
// finalizer(obj) in a separate goroutine. This makes obj reachable again,
// but now without an associated finalizer. Assuming that SetFinalizer
// is not called again, the next time the garbage collector sees
// that obj is unreachable, it will free obj.
//
// SetFinalizer(obj, nil) clears any finalizer associated with obj.
//
// New Go code should consider using [AddCleanup] instead, which is much
// less error-prone than SetFinalizer.
//
// The argument obj must be a pointer to an object allocated by calling
// new, by taking the address of a composite literal, or by taking the
// address of a local variable.
// The argument finalizer must be a function that takes a single argument
// to which obj's type can be assigned, and can have arbitrary ignored return
// values. If either of these is not true, SetFinalizer may abort the
// program.
//
// Finalizers are run in dependency order: if A points at B, both have
// finalizers, and they are otherwise unreachable, only the finalizer
// for A runs; once A is freed, the finalizer for B can run.
// If a cyclic structure includes a block with a finalizer, that
// cycle is not guaranteed to be garbage collected and the finalizer
// is not guaranteed to run, because there is no ordering that
// respects the dependencies.
//
// The finalizer is scheduled to run at some arbitrary time after the
// program can no longer reach the object to which obj points.
// There is no guarantee that finalizers will run before a program exits,
// so typically they are useful only for releasing non-memory resources
// associated with an object during a long-running program.
// For example, an [os.File] object could use a finalizer to close the
// associated operating system file descriptor when a program discards
// an os.File without calling Close, but it would be a mistake
// to depend on a finalizer to flush an in-memory I/O buffer such as a
// [bufio.Writer], because the buffer would not be flushed at program exit.
//
// It is not guaranteed that a finalizer will run if the size of *obj is
// zero bytes, because it may share same address with other zero-size
// objects in memory. See https://go.dev/ref/spec#Size_and_alignment_guarantees.
//
// It is not guaranteed that a finalizer will run for objects allocated
// in initializers for package-level variables. Such objects may be
// linker-allocated, not heap-allocated.
//
// Note that because finalizers may execute arbitrarily far into the future
// after an object is no longer referenced, the runtime is allowed to perform
// a space-saving optimization that batches objects together in a single
// allocation slot. The finalizer for an unreferenced object in such an
// allocation may never run if it always exists in the same batch as a
// referenced object. Typically, this batching only happens for tiny
// (on the order of 16 bytes or less) and pointer-free objects.
//
// A finalizer may run as soon as an object becomes unreachable.
// In order to use finalizers correctly, the program must ensure that
// the object is reachable until it is no longer required.
// Objects stored in global variables, or that can be found by tracing
// pointers from a global variable, are reachable. A function argument or
// receiver may become unreachable at the last point where the function
// mentions it. To make an unreachable object reachable, pass the object
// to a call of the [KeepAlive] function to mark the last point in the
// function where the object must be reachable.
//
// For example, if p points to a struct, such as os.File, that contains
// a file descriptor d, and p has a finalizer that closes that file
// descriptor, and if the last use of p in a function is a call to
// syscall.Write(p.d, buf, size), then p may be unreachable as soon as
// the program enters [syscall.Write]. The finalizer may run at that moment,
// closing p.d, causing syscall.Write to fail because it is writing to
// a closed file descriptor (or, worse, to an entirely different
// file descriptor opened by a different goroutine). To avoid this problem,
// call KeepAlive(p) after the call to syscall.Write.
//
// A single goroutine runs all finalizers for a program, sequentially.
// If a finalizer must run for a long time, it should do so by starting
// a new goroutine.
//
// In the terminology of the Go memory model, a call
// SetFinalizer(x, f) “synchronizes before” the finalization call f(x).
// However, there is no guarantee that KeepAlive(x) or any other use of x
// “synchronizes before” f(x), so in general a finalizer should use a mutex
// or other synchronization mechanism if it needs to access mutable state in x.
// For example, consider a finalizer that inspects a mutable field in x
// that is modified from time to time in the main program before x
// becomes unreachable and the finalizer is invoked.
// The modifications in the main program and the inspection in the finalizer
// need to use appropriate synchronization, such as mutexes or atomic updates,
// to avoid read-write races.
func SetFinalizer(obj any, finalizer any) {
	e := efaceOf(&obj)
	etyp := e._type
	if etyp == nil {
		throw("runtime.SetFinalizer: first argument is nil")
	}
	if etyp.Kind() != abi.Pointer {
		throw("runtime.SetFinalizer: first argument is " + toRType(etyp).string() + ", not pointer")
	}
	ot := (*ptrtype)(unsafe.Pointer(etyp))
	if ot.Elem == nil {
		throw("nil elem type!")
	}
	if inUserArenaChunk(uintptr(e.data)) {
		// Arena-allocated objects are not eligible for finalizers.
		throw("runtime.SetFinalizer: first argument was allocated into an arena")
	}
	if debug.sbrk != 0 {
		// debug.sbrk never frees memory, so no finalizers run
		// (and we don't have the data structures to record them).
		return
	}

	// find the containing object
	base, span, _ := findObject(uintptr(e.data), 0, 0)

	if base == 0 {
		if isGoPointerWithoutSpan(e.data) {
			return
		}
		throw("runtime.SetFinalizer: pointer not in allocated block")
	}

	// Move base forward if we've got an allocation header.
	if !span.spanclass.noscan() && !heapBitsInSpan(span.elemsize) && span.spanclass.sizeclass() != 0 {
		base += gc.MallocHeaderSize
	}

	if uintptr(e.data) != base {
		// As an implementation detail we allow to set finalizers for an inner byte
		// of an object if it could come from tiny alloc (see mallocgc for details).
		if ot.Elem == nil || ot.Elem.Pointers() || ot.Elem.Size_ >= maxTinySize {
			throw("runtime.SetFinalizer: pointer not at beginning of allocated block")
		}
	}

	f := efaceOf(&finalizer)
	ftyp := f._type
	if ftyp == nil {
		// switch to system stack and remove finalizer
		systemstack(func() {
			removefinalizer(e.data)

			if debug.checkfinalizers != 0 {
				clearFinalizerContext(uintptr(e.data))
				KeepAlive(e.data)
			}
		})
		return
	}

	if ftyp.Kind() != abi.Func {
		throw("runtime.SetFinalizer: second argument is " + toRType(ftyp).string() + ", not a function")
	}
	ft := (*functype)(unsafe.Pointer(ftyp))
	if ft.IsVariadic() {
		throw("runtime.SetFinalizer: cannot pass " + toRType(etyp).string() + " to finalizer " + toRType(ftyp).string() + " because dotdotdot")
	}
	if ft.InCount != 1 {
		throw("runtime.SetFinalizer: cannot pass " + toRType(etyp).string() + " to finalizer " + toRType(ftyp).string())
	}
	fint := ft.InSlice()[0]
	switch {
	case fint == etyp:
		// ok - same type
		goto okarg
	case fint.Kind() == abi.Pointer:
		if (fint.Uncommon() == nil || etyp.Uncommon() == nil) && (*ptrtype)(unsafe.Pointer(fint)).Elem == ot.Elem {
			// ok - not same type, but both pointers,
			// one or the other is unnamed, and same element type, so assignable.
			goto okarg
		}
	case fint.Kind() == abi.Interface:
		ityp := (*interfacetype)(unsafe.Pointer(fint))
		if len(ityp.Methods) == 0 {
			// ok - satisfies empty interface
			goto okarg
		}
		if itab := assertE2I2(ityp, efaceOf(&obj)._type); itab != nil {
			goto okarg
		}
	}
	throw("runtime.SetFinalizer: cannot pass " + toRType(etyp).string() + " to finalizer " + toRType(ftyp).string())
okarg:
	// compute size needed for return parameters
	nret := uintptr(0)
	for _, t := range ft.OutSlice() {
		nret = alignUp(nret, uintptr(t.Align_)) + t.Size_
	}
	nret = alignUp(nret, goarch.PtrSize)

	// make sure we have a finalizer goroutine
	createfing()

	callerpc := sys.GetCallerPC()
	systemstack(func() {
		if !addfinalizer(e.data, (*funcval)(f.data), nret, fint, ot) {
			throw("runtime.SetFinalizer: finalizer already set")
		}
		if debug.checkfinalizers != 0 {
			setFinalizerContext(e.data, ot.Elem, callerpc, (*funcval)(f.data).fn)
		}
	})
}

// Mark KeepAlive as noinline so that it is easily detectable as an intrinsic.
//
//go:noinline

// KeepAlive marks its argument as currently reachable.
// This ensures that the object is not freed, and its finalizer is not run,
// before the point in the program where KeepAlive is called.
//
// A very simplified example showing where KeepAlive is required:
//
//	type File struct { d int }
//	d, err := syscall.Open("/file/path", syscall.O_RDONLY, 0)
//	// ... do something if err != nil ...
//	p := &File{d}
//	runtime.SetFinalizer(p, func(p *File) { syscall.Close(p.d) })
//	var buf [10]byte
//	n, err := syscall.Read(p.d, buf[:])
//	// Ensure p is not finalized until Read returns.
//	runtime.KeepAlive(p)
//	// No more uses of p after this point.
//
// Without the KeepAlive call, the finalizer could run at the start of
// [syscall.Read], closing the file descriptor before syscall.Read makes
// the actual system call.
//
// Note: KeepAlive should only be used to prevent finalizers from
// running prematurely. In particular, when used with [unsafe.Pointer],
// the rules for valid uses of unsafe.Pointer still apply.
func KeepAlive(x any) {
	// Introduce a use of x that the compiler can't eliminate.
	// This makes sure x is alive on entry. We need x to be alive
	// on entry for "defer runtime.KeepAlive(x)"; see issue 21402.
	if cgoAlwaysFalse {
		println(x)
	}
}
