// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"unsafe"
)

var (
	findiscarded    uint64
	dstDeferredFinq *finBlock
	dstDrainingFinq *finBlock
)

var dstFinqRunBaseQueued, dstFinqRunExecuted atomic.Uint64

func queuefinalizer(p unsafe.Pointer, fn *funcval, nret uintptr, fint *_type, ot *ptrtype) {
	dstEpoch, dstSeq, dstPid := dstTakeStagedSpecialStamps()
	if gcphase != _GCoff {
		throw("queuefinalizer during GC")
	}

	lock(&finlock)

	q := &finq
	deferred := false
	if dstActive() && dstEpoch != dstRunEpoch.Load() {
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
	atomic.Xadd(&(*q).cnt, +1)
	f.fn = fn
	f.nret = nret
	f.fint = fint
	f.ot = ot
	f.arg = p
	if dstFinSlabMemo.block != *q {
		dstFinSlabMemo.block = *q
		dstFinSlabMemo.st = dstFinSlabFor(*q)
	}
	dstFinSlabMemo.st[(*q).cnt-1] = dstCallbackStamp{seq: dstSeq, epoch: dstEpoch, pid: dstPid}
	finqueued++
	unlock(&finlock)
	if !deferred {
		fingStatus.Or(fingWake)
	}
}

func finAllocBlockLocked() *finBlock {
	if finc == nil {
		finc = (*finBlock)(persistentalloc(finBlockSize, 0, &memstats.gcMiscSys))
		dstFinSlabAttachLocked(finc)
		finc.alllink = allfin
		allfin = finc
		if finptrmask[0] == 0 {
			if (unsafe.Sizeof(finalizer{}) != 5*goarch.PtrSize ||
				unsafe.Offsetof(finalizer{}.fn) != 0 ||
				unsafe.Offsetof(finalizer{}.arg) != goarch.PtrSize ||
				unsafe.Offsetof(finalizer{}.nret) != 2*goarch.PtrSize ||
				unsafe.Offsetof(finalizer{}.fint) != 3*goarch.PtrSize ||
				unsafe.Offsetof(finalizer{}.ot) != 4*goarch.PtrSize) {
				throw("finalizer out of sync")
			}
			for i := range finptrmask {
				finptrmask[i] = finalizer1[i%len(finalizer1)]
			}
		}
	}
	block := finc
	finc = block.next
	return block
}

func finalizercommitDSTBlocked(gp *g, lock unsafe.Pointer) bool {
	unlock((*mutex)(lock))
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

// This is the goroutine that runs all of the finalizers.
func runFinalizers() {
	gp := getg()
	lock(&finlock)
	fing = gp
	unlock(&finlock)

	var frame unsafe.Pointer
	var framecap uintptr
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
		frame, framecap = runFinqBlocks(fb, frame, framecap, false)
	}
}

func runFinqBlocks(fb *finBlock, frame unsafe.Pointer, framecap uintptr, onDrain bool) (unsafe.Pointer, uintptr) {
	gp := getg()
	argRegs := intArgRegs
	if raceenabled {
		racefingo()
	}
	for fb != nil {
		slab := dstFinSlabFor(fb)
		if onDrain {
			dstDrainingFinq = fb
		}
		n := fb.cnt
		var executed uint64
		for i := n; i > 0; i-- {
			for !onDrain && dstParkFingIfBlocked() {
			}
			f := &fb.fin[i-1]

			var regs abi.RegArgs
			framesz := unsafe.Sizeof((any)(nil)) + f.nret
			if framecap < framesz {
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
				*(*[2]uintptr)(frame) = [2]uintptr{}
			}
			switch f.fint.Kind() {
			case abi.Pointer:
				*(*unsafe.Pointer)(r) = f.arg
			case abi.Interface:
				ityp := (*interfacetype)(unsafe.Pointer(f.fint))
				(*eface)(r)._type = &f.ot.Type
				(*eface)(r).data = f.arg
				if len(ityp.Methods) != 0 {
					(*iface)(r).tab = assertE2I(ityp, (*eface)(r)._type)
				}
			default:
				throw("bad type kind in finalizer")
			}
			if !onDrain {
				fingStatus.Or(fingRunningFinalizer)
			}
			invoked := dstCallbackOwnerAlive(slab[i-1].epoch, slab[i-1].pid)
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
				slab[i-1].epoch = 0
			}
			if invoked {
				oldPid := gp.dstPid
				if onDrain && slab[i-1].pid > 0 {
					gp.dstPid = slab[i-1].pid
				}
				reflectcall(nil, unsafe.Pointer(f.fn), frame, uint32(framesz), uint32(framesz), uint32(framesz), &regs)
				gp.dstPid = oldPid
				executed++
			}
			if !onDrain {
				fingStatus.And(^fingRunningFinalizer)
			}

			f.fn = nil
			f.arg = nil
			f.ot = nil
			atomic.Store(&fb.cnt, i-1)
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
	return frame, framecap
}

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

func dstReleaseDeferredFinq() {
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

func dstDrainFinq() {
	var frame unsafe.Pointer
	var framecap uintptr
	for {
		lock(&finlock)
		fb := finq
		finq = nil
		unlock(&finlock)
		if fb == nil {
			return
		}
		dstSortFinqBySeq(fb)
		frame, framecap = runFinqBlocks(fb, frame, framecap, true)
	}
}

func dstSortFinqBySeq(fb *finBlock) {
	n := 0
	for b := fb; b != nil; b = b.next {
		n += int(b.cnt)
	}
	if n <= 1 {
		return
	}
	all := make([]dstFinWithStamp, n)
	var slabs []*[len(finBlock{}.fin)]dstCallbackStamp
	k := 0
	for b := fb; b != nil; b = b.next {
		slab := dstFinSlabFor(b)
		slabs = append(slabs, slab)
		for i := 0; i < int(b.cnt); i++ {
			all[k] = dstFinWithStamp{fin: b.fin[i], st: slab[i]}
			k++
		}
	}
	dstSortByStampSeq(all, func(p *dstFinWithStamp) uintptr { return p.st.seq })
	k = 0
	bi := 0
	for b := fb; b != nil; b = b.next {
		slab := slabs[bi]
		bi++
		cnt := int(b.cnt)
		for i := 0; i < cnt; i++ {
			b.fin[cnt-1-i] = all[k].fin
			slab[cnt-1-i] = all[k].st
			k++
		}
	}
}

func dstSortByStampSeq[T any](a []T, seq func(*T) uintptr) {
	n := len(a)
	if n <= 1 {
		return
	}
	buf := make([]T, n)
	src, dst := a, buf
	for width := 1; width < n; width *= 2 {
		for i := 0; i < n; i += 2 * width {
			lo := i
			mid := min(i+width, n)
			hi := min(i+2*width, n)
			l, r, k := lo, mid, lo
			for l < mid && r < hi {
				if seq(&src[l]) <= seq(&src[r]) {
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

func dstDiscardFinChainLocked(fb *finBlock) uint64 {
	var n uint64
	for fb != nil {
		next := fb.next
		slab := dstFinSlabFor(fb)
		cnt := atomic.Load(&fb.cnt)
		for i := cnt; i > 0; i-- {
			f := &fb.fin[i-1]
			if slab[i-1].epoch != 0 {
				n++
			}
			f.fn = nil
			f.arg = nil
			f.ot = nil
		}
		atomic.Store(&fb.cnt, 0)
		fb.next = finc
		finc = fb
		fb = next
	}
	findiscarded += n
	return n
}
