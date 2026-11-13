// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"unsafe"
)

func queuefinalizer(p unsafe.Pointer, fn *funcval, nret uintptr, fint *_type, ot *ptrtype) {
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

	if finq == nil || finq.cnt == uint32(len(finq.fin)) {
		if finc == nil {
			finc = (*finBlock)(persistentalloc(finBlockSize, 0, &memstats.gcMiscSys))
			finc.alllink = allfin
			allfin = finc
			if finptrmask[0] == 0 {
				// Build pointer mask for Finalizer array in block.
				// Check assumptions made in finalizer1 array above.
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
		block.next = finq
		finq = block
	}
	f := &finq.fin[finq.cnt]
	atomic.Xadd(&finq.cnt, +1) // Sync with markroots
	f.fn = fn
	f.nret = nret
	f.fint = fint
	f.ot = ot
	f.arg = p
	finqueued++
	unlock(&finlock)
	fingStatus.Or(fingWake)
}

// This is the goroutine that runs all of the finalizers.
func runFinalizers() {
	var (
		frame    unsafe.Pointer
		framecap uintptr
		argRegs  int
	)

	gp := getg()
	lock(&finlock)
	fing = gp
	unlock(&finlock)

	for {
		lock(&finlock)
		fb := finq
		finq = nil
		if fb == nil {
			gopark(finalizercommit, unsafe.Pointer(&finlock), waitReasonFinalizerWait, traceBlockSystemGoroutine, 1)
			continue
		}
		argRegs = intArgRegs
		unlock(&finlock)
		if raceenabled {
			racefingo()
		}
		for fb != nil {
			n := fb.cnt
			for i := n; i > 0; i-- {
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
				fingStatus.Or(fingRunningFinalizer)
				reflectcall(nil, unsafe.Pointer(f.fn), frame, uint32(framesz), uint32(framesz), uint32(framesz), &regs)
				fingStatus.And(^fingRunningFinalizer)

				// Drop finalizer queue heap references
				// before hiding them from markroot.
				// This also ensures these will be
				// clear if we reuse the finalizer.
				f.fn = nil
				f.arg = nil
				f.ot = nil
				atomic.Store(&fb.cnt, i-1)
			}
			next := fb.next
			lock(&finlock)
			finexecuted += uint64(n)
			fb.next = finc
			finc = fb
			unlock(&finlock)
			fb = next
		}
	}
}
