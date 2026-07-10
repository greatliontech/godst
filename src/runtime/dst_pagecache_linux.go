// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

// The simulation's page cache, and the mappings over it.
//
// A simulated file that is mapped keeps its bytes in a memfd — the page cache
// as an object — rather than in a Go slice. Mappings of that file are real
// mmap(MAP_SHARED) mappings of the memfd, and its length is a real ftruncate.
// The kernel then supplies, exactly and for free, everything a hand-written
// model would have to reproduce by hand:
//
//   - a read-only mapping and a writable mapping of one file share bytes but
//     not protections, because each mapping has its own page-table entries. A
//     single anonymous arena has one protection per page and cannot express
//     this, yet a database that maps its data file read-only and writes to it
//     through write(2) depends on it.
//   - a load past end-of-file traps, so a mapping may be a RESERVATION larger
//     than the file it maps.
//   - growing the file makes the next page valid with no remapping; shrinking
//     it makes the pages past the new end trap again, and zeroes the tail of
//     the partial page, and does not resurrect the dropped bytes if the file
//     later grows back over them.
//
// The alternative — inventing zeros for bytes past the end, or refusing to
// model the shape at all — would let the simulation produce executions Linux
// cannot (DST-FAULT-SOUND), which is the whole bug class DST exists to catch.
//
// A trap inside a mapping is not the harness's fault, and it is not a Go
// panic: production delivers SIGBUS to the process that touched the page, and
// nothing in that process can decline it. dstMappingFault therefore kills the
// SIMULATED process — the crash mark plus a park that never returns — leaving
// every other simulated process, and the harness, running. sigpanic consults
// it before it consults gp.paniconfault, so a system under test that called
// debug.SetPanicOnFault cannot recover from a fault the kernel would not have
// let it recover from either.
//
// Nothing address-derived is observable: mapping addresses vary run to run and
// never reach the system under test as a value. A fault is attributed to a
// process, and the file offset it lands on is a pure function of the schedule.

package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/syscall/linux"
	"unsafe"
)

// dstPageCachePageSize is the SIMULATED page size (os.dstMMapPageSize). The
// host MMU enforces protection boundaries at physPageSize granularity, so a
// host page coarser than the simulated one could not trap at the simulated
// boundary: a byte past end-of-file would read as zero on that machine and
// trap on another, making run outcomes machine-dependent. Such a host is
// refused outright rather than diverged from silently.
const dstPageCachePageSize = 4096

const (
	_MFD_CLOEXEC = 0x1
	// The runtime maps only private anonymous memory, so it defines no
	// _MAP_SHARED. The value is 0x1 on every Linux architecture; it lives here
	// rather than in upstream's per-arch defs so a port never conflicts.
	_dstMapShared = 0x1
)

var dstMemfdName = [...]byte{'d', 's', 't', '-', 'p', 'a', 'g', 'e', 'c', 'a', 'c', 'h', 'e', 0}

// dstSpan is one live mapping's address range.
type dstSpan struct {
	base uintptr
	size uintptr
}

// dstSpanSet is immutable once published; mutators build the successor, then
// CAS it in. Readers (dstMappingFaultAddr, reached from sigpanic) take no lock:
// a fault can arrive at any instruction, including one inside a mutator, and a
// reader that blocked on a mutator's lock would deadlock. Allocating outside
// any lock also keeps mallocgc — which may take mheap_.lock or assist the GC —
// off a held runtime mutex.
type dstSpanSet struct {
	spans []dstSpan
}

var dstSpans atomic.Pointer[dstSpanSet]

func dstSpanAdd(base, size uintptr) {
	for {
		old := dstSpans.Load()
		next := &dstSpanSet{}
		if old != nil {
			next.spans = append(next.spans, old.spans...)
		}
		next.spans = append(next.spans, dstSpan{base: base, size: size})
		if dstSpans.CompareAndSwap(old, next) {
			return
		}
	}
}

func dstSpanRemove(base uintptr) {
	for {
		old := dstSpans.Load()
		if old == nil {
			return
		}
		next := &dstSpanSet{}
		for _, s := range old.spans {
			if s.base != base {
				next.spans = append(next.spans, s)
			}
		}
		if dstSpans.CompareAndSwap(old, next) {
			return
		}
	}
}

func dstPageCacheCheckHost() {
	if physPageSize > dstPageCachePageSize || dstPageCachePageSize%physPageSize != 0 {
		throw("dst: host page size is coarser than the simulated 4096-byte page")
	}
}

// dstPageCacheNew creates an empty page cache. The descriptor is the harness's,
// never the simulated process's: no simulated file descriptor ever names it.
func dstPageCacheNew() int32 {
	dstPageCacheCheckHost()
	fd, _, errno := linux.Syscall6(dstSysMemfdCreate,
		uintptr(unsafe.Pointer(&dstMemfdName[0])), _MFD_CLOEXEC, 0, 0, 0, 0)
	if errno != 0 {
		throw("dst: page cache creation failed")
	}
	return int32(fd)
}

// dstPageCacheResize sets the file's length: the one operation behind both
// growth and truncation, including the partial page's tail zeroing and the
// trapping of pages that fall past the new end while still mapped.
func dstPageCacheResize(fd int32, size int64) {
	if size < 0 {
		throw("dst: negative page cache size")
	}
	if errno := dstFtruncate(fd, size); errno != 0 {
		throw("dst: page cache resize failed")
	}
}

// dstPageCacheMap maps [0, n) of the page cache with prot and returns its base.
// n may extend past the file's current end: those pages trap until the file
// grows over them, which is what makes a reservation mapping expressible.
//
// Mapping always starts at file offset 0, and a caller wanting a window at
// offset off maps off+n bytes and indexes from there. runtime.mmap's off
// parameter cannot be used: it means bytes on 386 (the assembly shifts it down
// to pages itself) but pages on arm (it is handed to mmap2 unshifted), and
// every in-tree caller passes 0, so the divergence is untested. Passing a file
// offset through it would map the wrong bytes on some architectures and the
// right ones on the machine we happen to test on.
func dstPageCacheMap(fd int32, n uintptr, prot int32) uintptr {
	if n == 0 {
		throw("dst: empty page cache mapping")
	}
	p, err := mmap(nil, n, prot, _dstMapShared, fd, 0)
	if err != 0 {
		throw("dst: page cache mapping failed")
	}
	base := uintptr(p)
	dstSpanAdd(base, n)
	return base
}

// dstPageCacheUnmap unregisters the mapping, then unmaps it. In that order: a
// fault in the range after the address space is gone but while it is still
// registered would be attributed to a simulated process that no longer has the
// mapping. Unregistering first means such a fault is reported as the harness
// bug it is.
func dstPageCacheUnmap(base, n uintptr) {
	dstSpanRemove(base)
	munmap(unsafe.Pointer(base), n)
}

func dstPageCacheClose(fd int32) {
	closefd(fd)
}

// dstMappingFaultAddr reports whether addr lies inside a live mapping, i.e.
// whether the fault is a simulated process touching a page its file does not
// have, or writing through a read-only mapping.
func dstMappingFaultAddr(addr uintptr) bool {
	set := dstSpans.Load()
	if set == nil {
		return false
	}
	for _, s := range set.spans {
		if addr >= s.base && addr < s.base+s.size {
			return true
		}
	}
	return false
}

// dstMappingFault converts a hardware fault inside a mapping into the death of
// the simulated process that took it. It never returns.
//
// A goroutine with no simulated process (pid <= 0) has no process to kill: the
// fault is the harness's own, and throwing preserves that. Otherwise the whole
// process dies — every goroutine it owns, wherever they are blocked — and this
// one parks forever, which is what a SIGBUS'd thread does as far as the rest of
// the system can tell.
func dstMappingFault(addr uintptr) {
	gp := getg()
	pid := gp.dstPid
	if pid <= 0 {
		print("dst: fault at ", hex(addr), " inside a simulated mapping, outside any simulated process\n")
		throw("dst: mapping fault outside a simulated process")
	}
	// A pid outlives its simulation only if a mapping did too, which is a
	// harness bug: say so rather than dereference a nil bubble inside the guard.
	if dstSimBubble == nil {
		print("dst: fault at ", hex(addr), " inside a simulated mapping after the simulation ended\n")
		throw("dst: mapping outlived its simulation")
	}
	// Killing the process that owns the run body would leave the bubble with no
	// goroutine able to finish it — a hang, reported as "all goroutines are
	// asleep" pages away from the cause. Name the cause instead. (Crash makes
	// the same check and panics; here a panic would be recoverable, and this
	// fault is not.) dstSimBubble is the bubble the crash machinery itself
	// marks against, so the guard must ask about that one.
	if dstCrashKillsBubbleMain(dstSimBubble, pid) {
		print("dst: fault at ", hex(addr), " inside a simulated mapping\n")
		throw("dst: a mapping fault crashed the process running the simulation body")
	}
	dstCrashProcessPid(pid)
	dstParkCrashedSelf()
	throw("dst: a process crashed by a mapping fault resumed")
}

// dstMappingSigpanic is sigpanic's hook. It returns only when the fault was not
// a simulated mapping's.
//
// It is reached only when sigpanic is: a fault taken with a runtime lock held,
// while mallocing, on the system stack, or in a syscall does not become a
// panic at all (canpanic), and aborts the harness. The simulation therefore
// touches mapped bytes only from ordinary goroutine context — where a system
// under test's own loads and stores already live.
func dstMappingSigpanic(gp *g) {
	if gp.sig != _SIGSEGV && gp.sig != _SIGBUS {
		return
	}
	if !dstMappingFaultAddr(gp.sigcode1) {
		return
	}
	dstMappingFault(gp.sigcode1)
}
