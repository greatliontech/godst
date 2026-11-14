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
// Mapping addresses ARE observable: the system under test holds the returned
// slice, and a pointer is a value — it can key a map, whose iteration order
// then hangs on its bits. Replay-exactness therefore demands that addresses be
// a pure function of the schedule, the same property the fork already gives
// the heap base (rand.go seeds bootstrapRand deterministically, so the
// randomized-heap-base experiment lands every invocation at the same address).
// Kernel-chosen mmap addresses would break it: mmap_base is randomized per
// exec. So mappings never let the kernel choose. A canonical region is
// reserved once — MAP_FIXED_NOREPLACE, so occupancy refuses loudly rather
// than relocating silently — and every mapping is carved from it MAP_FIXED at
// a bump-allocated offset. Carve order is mmap order is the schedule, and the
// bump pointer resets when a run ends, so one seed yields one address, within
// a process and across invocations alike.
//
// The base must dodge what the address space already holds: the dst heap
// lands near 0x33be27e (deterministic, see above), a PIE text segment inside
// [0x5555.., 0x5655..], the kernel's unhinted descent from mmap_base near the
// 0x7fff.. ceiling — and TSAN's Go shadow must cover it for the -race legs
// (verified empirically for 0x5a00_0000_0000, along with three-invocation
// address stability).

package runtime

import (
	"internal/goarch"
	"internal/runtime/atomic"
	"internal/runtime/sys"
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

// A SUT mapping's registry entry outlives the mapping as a TOMBSTONE: its
// address space is PROT_NONE-covered and never reused within the run, so the
// entry keeps enough truth to attribute a later fault to what the range USED
// to be — the difference between a process touching memory it unmapped (its
// own SIGSEGV death, as in production), code reaching a dead machine's or dead
// process's memory (no production analog: the memory does not exist — a named
// model-boundary abort), and a genuine harness bug (everything else).
//
// Model-internal file views (dstSpanRetired) NEVER escape to the SUT: when a
// file outgrows its view or its last reference drops, the retired range is
// PROT_NONE-covered and returned to a deterministic lowest-address coalescing
// free list so aggregate capacity across growing/closed files does not exhaust
// the region. While unallocated, a retired range still carries its retired
// entry so an internal dangling read throws by name; once re-carved, the newer
// entry shadows it (readers scan newest-to-oldest) and growth compacts shadowed
// retired intervals out of dstSpans.
const (
	dstSpanLive     = iota
	dstSpanUnmapped // the owner unmapped it: a later touch is the toucher's SIGSEGV
	dstSpanCrashed  // its process or machine died: a later touch is a named abort
	dstSpanRetired  // a model-internal view, replaced or closed: reusable by future carves
)

// dstSpan is one mapping's address range and its state.
type dstSpan struct {
	base  uintptr
	size  uintptr
	state uint8
}

// dstFreeSpan is a half-open [base, base+size) interval, kept sorted and
// coalesced in dstMapRegion.free and during dstSpanCompact.
type dstFreeSpan struct {
	base uintptr
	size uintptr
}

// dstSpanSet is an append-only vector published by pointer. Readers
// (dstMappingFaultAddr in sigpanic) are lock-free so a fault on the mutator's
// own thread never self-deadlocks; writers serialize on dstMapRegion.lock.
type dstSpanSet struct {
	spans []dstSpan // entries [0, n) are published; capacity is pre-allocated
	n     atomic.Uintptr
}

var dstSpans atomic.Pointer[dstSpanSet]

func dstIntervalInsert(spans []dstFreeSpan, base, size uintptr) []dstFreeSpan {
	end := base + size
	i := 0
	for i < len(spans) && spans[i].base+spans[i].size < base {
		i++
	}
	j := i
	for j < len(spans) && spans[j].base <= end {
		j++
	}
	if i < j {
		if spans[i].base < base {
			base = spans[i].base
		}
		if lastEnd := spans[j-1].base + spans[j-1].size; lastEnd > end {
			end = lastEnd
		}
		spans[i] = dstFreeSpan{base: base, size: end - base}
		copy(spans[i+1:], spans[j:])
		return spans[:len(spans)-(j-i-1)]
	}
	spans = append(spans, dstFreeSpan{})
	copy(spans[i+1:], spans[i:])
	spans[i] = dstFreeSpan{base: base, size: size}
	return spans
}

// dstSpanCompact trims retired spans shadowed by newer spans, preserving the
// exact classification dstMappingFaultAddr's newest-to-oldest scan observes at
// every address while keeping the table bounded by live + tombstoned + free
// fragments. Caller holds dstMapRegion.lock.
func dstSpanCompact(old []dstSpan) []dstSpan {
	var cov []dstFreeSpan
	rev := make([]dstSpan, 0, len(old))
	for i := len(old) - 1; i >= 0; i-- {
		s := old[i]
		st := atomic.Load8(&s.state)
		if st != dstSpanRetired {
			rev = append(rev, dstSpan{base: s.base, size: s.size, state: st})
			cov = dstIntervalInsert(cov, s.base, s.size)
			continue
		}
		cur := s.base
		end := s.base + s.size
		k := 0
		for k < len(cov) && cov[k].base+cov[k].size <= cur {
			k++
		}
		keptAny := false
		for k < len(cov) && cov[k].base < end {
			if cov[k].base > cur {
				rev = append(rev, dstSpan{base: cur, size: cov[k].base - cur, state: dstSpanRetired})
				keptAny = true
			}
			if covEnd := cov[k].base + cov[k].size; covEnd > cur {
				cur = covEnd
			}
			k++
		}
		if cur < end {
			rev = append(rev, dstSpan{base: cur, size: end - cur, state: dstSpanRetired})
			keptAny = true
		}
		if keptAny {
			cov = dstIntervalInsert(cov, s.base, s.size)
		}
	}
	for l, r := 0, len(rev)-1; l < r; l, r = l+1, r-1 {
		rev[l], rev[r] = rev[r], rev[l]
	}
	return rev
}

// dstSpanAddLocked publishes a new live mapping span. Caller holds
// dstMapRegion.lock (m.locks > 0 suppresses GC start/assist across the make).
func dstSpanAddLocked(base, size uintptr) {
	set := dstSpans.Load()
	n := uintptr(0)
	if set != nil {
		n = set.n.Load()
	}
	if set == nil || n == uintptr(cap(set.spans)) {
		var kept []dstSpan
		if set != nil {
			kept = dstSpanCompact(set.spans[:n])
		}
		grown := &dstSpanSet{spans: make([]dstSpan, max(16, 2*(len(kept)+1)))}
		copy(grown.spans, kept)
		n = uintptr(len(kept))
		grown.n.Store(n)
		dstSpans.Store(grown)
		set = grown
	}
	set.spans[n] = dstSpan{base: base, size: size}
	set.n.Store(n + 1) // publish after the entry is whole
}

// dstSpanKillLocked transitions the live span at base to state. Caller holds
// dstMapRegion.lock.
func dstSpanKillLocked(base uintptr, state uint8) {
	set := dstSpans.Load()
	if set == nil {
		return
	}
	for i := int(set.n.Load()) - 1; i >= 0; i-- {
		if set.spans[i].base == base && atomic.Load8(&set.spans[i].state) == dstSpanLive {
			atomic.Store8(&set.spans[i].state, state)
			return
		}
	}
}

func dstPageCacheCheckHost() {
	if goarch.PtrSize == 4 {
		throw("dst: memory mappings need a 64-bit host (the canonical mapping region does not fit a 32-bit address space)")
	}
	if physPageSize > dstPageCachePageSize || dstPageCachePageSize%physPageSize != 0 {
		throw("dst: host page size is coarser than the simulated 4096-byte page")
	}
}

// The runtime maps only private anonymous memory, so it defines none of these.
// They live here rather than in upstream's per-arch defs so a port never
// conflicts. MAP_SHARED and MAP_FIXED are identical on every Linux
// architecture; MAP_FIXED_NOREPLACE is asm-generic, adopted unchanged by mips
// and power; MAP_NORESERVE is per-arch (dst_pagecache_region_*.go).
const (
	_dstMapFixed          = 0x10
	_dstMapFixedNoReplace = 0x100000
	_dstSeekData          = 3
	_dstSeekHole          = 4
	_dstEnxio             = 0x6
)

// dstMapRegion is the carve state. reserved flips once; next is the bump
// offset from dstMapRegionBase; maxNext tracks the high-water bump offset for
// run-boundary reset; free holds retired internal file views sorted by base
// and coalesced so lowest-address first-fit stays deterministic.
var dstMapRegion struct {
	lock     mutex
	reserved atomic.Uint32
	next     uintptr
	maxNext  uintptr
	free     []dstFreeSpan
}

func dstMapRegionReserveLocked() {
	if dstMapRegion.reserved.Load() != 0 {
		return
	}
	dstPageCacheCheckHost()
	p, err := mmap(unsafe.Pointer(uintptr(dstMapRegionBase)), dstMapRegionSize, _PROT_NONE,
		_MAP_ANON|_MAP_PRIVATE|_dstMapFixedNoReplace|_dstMapNoReserve, -1, 0)
	if err != 0 || uintptr(p) != dstMapRegionBase {
		unlock(&dstMapRegion.lock)
		print("dst: cannot reserve the mapping region at ", hex(dstMapRegionBase), " (errno ", err, ")\n")
		throw("dst: the canonical mapping region is unavailable on this host")
	}
	dstMapRegion.reserved.Store(1)
}

// dstMapRegionCarveLocked hands out n bytes from the lowest fitting retired
// span or bumps next, returning ^uintptr(0) when the region cannot hold n — a
// deterministic outcome, since both free-list order and bump offset are pure
// functions of the run's mapping sequence. Caller holds dstMapRegion.lock.
func dstMapRegionCarveLocked(n uintptr) uintptr {
	dstMapRegionReserveLocked()
	for i := 0; i < len(dstMapRegion.free); i++ {
		f := dstMapRegion.free[i]
		if f.size >= n {
			base := f.base
			if f.size == n {
				copy(dstMapRegion.free[i:], dstMapRegion.free[i+1:])
				dstMapRegion.free = dstMapRegion.free[:len(dstMapRegion.free)-1]
			} else {
				dstMapRegion.free[i].base += n
				dstMapRegion.free[i].size -= n
			}
			return base
		}
	}
	off := dstMapRegion.next
	if n > dstMapRegionSize-off {
		return ^uintptr(0)
	}
	dstMapRegion.next = off + n
	if dstMapRegion.next > dstMapRegion.maxNext {
		dstMapRegion.maxNext = dstMapRegion.next
	}
	return dstMapRegionBase + off
}

func dstMapRegionReturnLocked(base, n uintptr) {
	dstMapRegion.free = dstIntervalInsert(dstMapRegion.free, base, n)
	if m := len(dstMapRegion.free); m > 0 {
		last := dstMapRegion.free[m-1]
		if last.base+last.size == dstMapRegionBase+dstMapRegion.next {
			dstMapRegion.next = last.base - dstMapRegionBase
			dstMapRegion.free = dstMapRegion.free[:m-1]
		}
	}
}

// dstPageCacheResetRegion rewinds the region for the next run: every carve is
// re-covered inaccessible in one stroke and the allocator state returns to
// zero, so the next run's first mapping lands at the same address the last
// run's did. The caller guarantees the old run's mappings are all dead — this
// runs at the run boundary, after the page caches are released.
//
//go:linkname dstPageCacheResetRegion
func dstPageCacheResetRegion() {
	lock(&dstMapRegion.lock)
	used := dstMapRegion.maxNext
	if used == 0 {
		dstMapRegion.next = 0
		dstMapRegion.free = nil
		dstSpans.Store(nil)
		unlock(&dstMapRegion.lock)
		return
	}
	p, err := mmap(unsafe.Pointer(uintptr(dstMapRegionBase)), used, _PROT_NONE,
		_MAP_ANON|_MAP_PRIVATE|_dstMapFixed|_dstMapNoReserve, -1, 0)
	if err != 0 || uintptr(p) != dstMapRegionBase {
		unlock(&dstMapRegion.lock)
		throw("dst: mapping region reset failed")
	}
	dstMapRegion.next = 0
	dstMapRegion.maxNext = 0
	dstMapRegion.free = nil
	dstSpans.Store(nil)
	unlock(&dstMapRegion.lock)
}

// dstPageCacheFDs is an atomically-published bitmap (*dstPageCacheFDSet) of
// live page-cache fd numbers, indexed by fd. The simulated process must not be
// able to reach these descriptors: the syscall boundary consults
// dstPageCacheFDReserved and answers EBADF for a bubble goroutine, exactly as
// for a fd the SUT never opened — a passed-through close would kill a live
// file's cache (fatal at the next resize or mmap), and a freed number reused
// by a later memfd_create would silently alias another file's bytes. Readers
// are lock-free (atomic pointer load + atomic word load) and nosplit-safe: the
// raw syscall boundary runs without a P. Writers serialize on
// dstPageCacheFDLock, and the memfd_create/closefd syscalls happen INSIDE that
// lock together with the bit flip: holding a runtime mutex (m.locks > 0)
// suppresses cooperative preemption, so no other goroutine can run — and
// sweep-close an unregistered newborn memfd, or catch the gap between
// unregister and the host close — while a descriptor exists in one state but
// is recorded in the other.
//
// The bitmap lives off the GC heap via persistentalloc so host-chosen memfd
// descriptor numbers and cross-run table warmth never perturb dstHeapAlloc
// (DST-GC-1) or dstProcAlloc (MemoryLimit).
var dstPageCacheFDs atomic.UnsafePointer
var dstPageCacheFDLock mutex

type dstPageCacheFDSet struct {
	_ sys.NotInHeap
	n uintptr
}

//go:nosplit
func dstPageCacheFDWord(set *dstPageCacheFDSet, i uintptr) *uint32 {
	return (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(set)) + unsafe.Sizeof(dstPageCacheFDSet{}) + i*4))
}

// dstPageCacheFDRegisterLocked flips fd's reserved bit. Caller holds
// dstPageCacheFDLock.
func dstPageCacheFDRegisterLocked(fd int32, on bool) {
	if fd < 0 {
		return
	}
	set := (*dstPageCacheFDSet)(dstPageCacheFDs.Load())
	w := uintptr(fd) >> 5
	if set == nil || w >= set.n {
		if !on {
			return
		}
		nw := max(uintptr(16), (w+1)*2)
		grown := (*dstPageCacheFDSet)(persistentalloc(
			unsafe.Sizeof(dstPageCacheFDSet{})+nw*4,
			goarch.PtrSize,
			&memstats.other_sys,
		))
		grown.n = nw
		if set != nil {
			for i := uintptr(0); i < set.n; i++ {
				*dstPageCacheFDWord(grown, i) = atomic.Load(dstPageCacheFDWord(set, i))
			}
		}
		dstPageCacheFDs.Store(unsafe.Pointer(grown))
		set = grown
	}
	mask := uint32(1) << (uint(fd) & 31)
	if on {
		atomic.Or32(dstPageCacheFDWord(set, w), mask)
	} else {
		atomic.And32(dstPageCacheFDWord(set, w), ^mask)
	}
}

// dstPageCacheFDReserved reports whether fd is a live harness page-cache
// descriptor. Called from the syscall package's fence boundary via linkname;
// nosplit and allocation-free (the raw boundary runs without a P).
//
//go:linkname dstPageCacheFDReserved
//go:nosplit
func dstPageCacheFDReserved(fd uintptr) bool {
	set := (*dstPageCacheFDSet)(dstPageCacheFDs.Load())
	if set == nil {
		return false
	}
	w := fd >> 5
	if w >= set.n {
		return false
	}
	return atomic.Load(dstPageCacheFDWord(set, w))&(1<<(fd&31)) != 0
}

// dstPageCacheNew creates an empty page cache. The descriptor is the harness's,
// never the simulated process's: no simulated file descriptor ever names it,
// and it is registered as reserved so the simulated process cannot reach it
// through the raw or named syscall surfaces.
//
//go:linkname dstPageCacheNew
func dstPageCacheNew() int32 {
	dstPageCacheCheckHost()
	lock(&dstPageCacheFDLock)
	fd, _, errno := linux.Syscall6(dstSysMemfdCreate,
		uintptr(unsafe.Pointer(&dstMemfdName[0])), _MFD_CLOEXEC, 0, 0, 0, 0)
	if errno != 0 {
		unlock(&dstPageCacheFDLock)
		throw("dst: page cache creation failed")
	}
	dstPageCacheFDRegisterLocked(int32(fd), true)
	unlock(&dstPageCacheFDLock)
	return int32(fd)
}

// dstPageCacheResize sets the file's length: the one operation behind both
// growth and truncation, including the partial page's tail zeroing and the
// trapping of pages that fall past the new end while still mapped.
//
//go:linkname dstPageCacheResize
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
//
// It returns 0 when the region cannot hold the mapping — the caller's
// deterministic ENOMEM — and throws on any other failure, which inside our own
// reservation is a harness bug, not an outcome production has.
//
//go:linkname dstPageCacheMap
func dstPageCacheMap(fd int32, n uintptr, prot int32) uintptr {
	if n == 0 {
		throw("dst: empty page cache mapping")
	}
	if rem := n % dstPageCachePageSize; rem != 0 {
		n += dstPageCachePageSize - rem
	}
	lock(&dstMapRegion.lock)
	base := dstMapRegionCarveLocked(n)
	if base == ^uintptr(0) {
		unlock(&dstMapRegion.lock)
		return 0
	}
	p, err := mmap(unsafe.Pointer(base), n, prot, _dstMapShared|_dstMapFixed, fd, 0)
	if err != 0 || uintptr(p) != base {
		unlock(&dstMapRegion.lock)
		throw("dst: page cache mapping failed")
	}
	dstSpanAddLocked(base, n)
	unlock(&dstMapRegion.lock)
	return base
}

// dstPageCacheUnmap tombstones the mapping's registry entry with state — a
// later fault into the range is attributed to what the range WAS — and
// re-covers the address space inaccessible rather than unmapping it: a hole in
// the region is space the kernel may hand to an unrelated mmap, and a foreign
// mapping inside the region would confuse that attribution. Covered space
// stays ours; adjacent PROT_NONE anonymous mappings merge, so the cover does
// not accumulate VMAs. Model-internal views (dstSpanRetired) are returned to
// dstMapRegion.free for deterministic reuse.
//
//go:linkname dstPageCacheUnmap
func dstPageCacheUnmap(base, n uintptr, state uint8) {
	if rem := n % dstPageCachePageSize; rem != 0 {
		n += dstPageCachePageSize - rem
	}
	lock(&dstMapRegion.lock)
	dstSpanKillLocked(base, state)
	p, err := mmap(unsafe.Pointer(base), n, _PROT_NONE,
		_MAP_ANON|_MAP_PRIVATE|_dstMapFixed|_dstMapNoReserve, -1, 0)
	if err != 0 || uintptr(p) != base {
		unlock(&dstMapRegion.lock)
		throw("dst: page cache unmap failed")
	}
	if state == dstSpanRetired {
		dstMapRegionReturnLocked(base, n)
	}
	unlock(&dstMapRegion.lock)
}

// dstPageCacheNextData returns the first kernel-allocated [dataOff, holeOff)
// byte interval in fd at or after off within [0, size), using SEEK_DATA and
// SEEK_HOLE so sparse multi-hundred-GiB memfds are scanned in O(allocated
// extents) without faulting hole pages.
//
//go:linkname dstPageCacheNextData
func dstPageCacheNextData(fd int32, off, size int64) (dataOff, holeOff int64, ok bool) {
	if off < 0 {
		throw("dst: negative page cache seek offset")
	}
	if off >= size {
		return 0, 0, false
	}
	d, errno := dstLseek(fd, off, _dstSeekData)
	if errno == _dstEnxio {
		return 0, 0, false
	}
	if errno != 0 {
		throw("dst: page cache SEEK_DATA failed")
	}
	if d >= size {
		return 0, 0, false
	}
	h, errno := dstLseek(fd, d, _dstSeekHole)
	if errno != 0 {
		throw("dst: page cache SEEK_HOLE failed")
	}
	if h > size {
		h = size
	}
	if h <= d {
		return 0, 0, false
	}
	return d, h, true
}

func dstSpanCount() int {
	set := dstSpans.Load()
	if set == nil {
		return 0
	}
	return int(set.n.Load())
}

//go:linkname dstPageCacheClose
func dstPageCacheClose(fd int32) {
	// Unregister and close under one lock hold (no preemption window between
	// them), unregister first: after closefd the number is free for reuse by
	// any host allocation, and a stale reserved bit would make an unrelated
	// new fd invisible to the simulated process.
	lock(&dstPageCacheFDLock)
	dstPageCacheFDRegisterLocked(fd, false)
	closefd(fd)
	unlock(&dstPageCacheFDLock)
}

// dstPageCacheProtect changes a live mapping's protection in place: the
// mprotect(2) a system under test asked for, applied to the hardware rather
// than recorded in a ledger. The mapping's bytes are untouched.
//
//go:linkname dstPageCacheProtect
func dstPageCacheProtect(base, n uintptr, prot int32) bool {
	ret, _ := mprotect(unsafe.Pointer(base), n, prot)
	return ret == 0
}

// dstPageCacheFatal aborts the harness with reason: the os layer's capability
// limits (the mapping region cannot hold another view) must be as
// unswallowable as the runtime's own, and a Go panic is not — a system under
// test's recover() would turn "the harness is out of address space" into an
// execution no kernel produces.
//
//go:linkname dstPageCacheFatal
func dstPageCacheFatal(reason string) {
	throw(reason)
}

// dstMappingFaultAddr classifies addr: not a simulated mapping's (found=false),
// or found with the newest covering span's state — live (the file does not have
// the page, or the view is read-only), unmapped, crashed, or retired. Scanning
// newest-to-oldest lets a re-carved span shadow the retired internal view whose
// address range it reused while leaving any un-reused tail classified retired.
func dstMappingFaultAddr(addr uintptr) (state uint8, found bool) {
	set := dstSpans.Load()
	if set == nil {
		return 0, false
	}
	for i := int(set.n.Load()) - 1; i >= 0; i-- {
		s := &set.spans[i]
		if addr >= s.base && addr < s.base+s.size {
			return atomic.Load8(&s.state), true
		}
	}
	return 0, false
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
		print("dst: fault at ", hex(addr), " inside a simulated mapping (live or unmapped), outside any simulated process\n")
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
	if !dstMappingFaultTeardown(gp.dstProc) {
		dstCrashProcessPid(pid)
	}
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
	state, found := dstMappingFaultAddr(gp.sigcode1)
	if !found {
		return
	}
	switch state {
	case dstSpanLive, dstSpanUnmapped:
		// A page the file does not have, a protection violation, or memory the
		// process unmapped and touched anyway: the toucher's own death, exactly
		// as production delivers it.
		dstMappingFault(gp.sigcode1)
	case dstSpanCrashed:
		// Production would deliver the toucher its own SIGSEGV here too; the
		// named abort is the deliberate choice — a mapping reached after its
		// owner exited or crashed means a slice crossed a process boundary,
		// and exposing that beats laundering it as one more process death.
		print("dst: fault at ", hex(gp.sigcode1), " inside a dead owner's mapping\n")
		throw("dst: access to a dead owner's mapping (its process exited or crashed, or its machine died; passing a mapping between simulated processes is outside the model)")
	case dstSpanRetired:
		print("dst: fault at ", hex(gp.sigcode1), " inside a retired internal file view\n")
		throw("dst: access to a retired file view (harness bug: node.data must be re-read under the lock)")
	default:
		throw("dst: mapping tombstone carries an unknown state")
	}
}

// dstSMaxBytesOS is the modeled filesystem's maximum file size — the
// s_maxbytes the os layer's size-growth sites refuse EFBIG past, as the
// vfs does. Derived from the mapping region so an in-bounds file can always
// be viewed: an eighth of the region leaves headroom for the power-of-two
// reserve doubling and for sibling files. Lives in this dst && linux file
// because dstMapRegionSize exists only in that tag universe (the untagged
// dst.go broke every cross-compile that first hosted this). Reached from os
// via linkname; 0 on arches whose region is off — the os side treats that
// as unbounded.
//
//go:linkname dstSMaxBytesOS
func dstSMaxBytesOS() int64 { return int64(dstMapRegionSize / 8) }
