// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package runtime_test

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/simulation"
	"unsafe"
)

const pcPage = runtime.DSTPageCachePageSize

func mappingBytes(base uintptr, n uintptr) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(base)), n)
}

// pcSink keeps a load from being optimized away: an elided load is an elided
// fault, and a test that expects one would pass for the wrong reason.
var pcSink atomic.Uint32

// newPageCache returns a page cache of the given length plus a writable
// mapping of reserveN bytes over it — a reservation, since reserveN may exceed
// the length.
func newPageCache(t *testing.T, length int64, reserveN uintptr) (fd int32, base uintptr) {
	t.Helper()
	fd = runtime.DSTPageCacheNew()
	runtime.DSTPageCacheResize(fd, length)
	base = runtime.DSTPageCacheMap(fd, reserveN, runtime.DSTProtRead|runtime.DSTProtWrite)
	t.Cleanup(func() {
		runtime.DSTPageCacheUnmap(base, reserveN, runtime.DSTSpanUnmapped)
		runtime.DSTPageCacheClose(fd)
	})
	return fd, base
}

// TestDSTPageCacheResizeSemantics pins what truncation does to bytes, which is
// the half of the contract that does not require a fault to observe: growing
// yields zeros, shrinking zeroes the tail of the partial page, and re-growing
// does not resurrect the bytes the shrink dropped.
func TestDSTPageCacheResizeSemantics(t *testing.T) {
	fd, base := newPageCache(t, pcPage, 2*pcPage)
	mem := mappingBytes(base, 2*pcPage)

	mem[0] = 'a'
	mem[pcPage-1] = 't' // the tail of the partial page, once we shrink to 1

	// Grow: the second page becomes valid, with no remapping, and reads zero.
	runtime.DSTPageCacheResize(fd, 2*pcPage)
	if mem[pcPage] != 0 {
		t.Errorf("grown page = %q, want zero", mem[pcPage])
	}
	mem[pcPage] = 'z'

	// Shrink to one byte: page 1 is gone, and page 0's tail past the new end
	// must be zeroed, as truncate(2) requires.
	runtime.DSTPageCacheResize(fd, 1)
	if mem[0] != 'a' {
		t.Errorf("byte before the new end = %q, want 'a'", mem[0])
	}
	if mem[pcPage-1] != 0 {
		t.Errorf("partial page tail = %q, want zero after shrink", mem[pcPage-1])
	}

	// Re-grow over the dropped pages: the bytes must not come back.
	runtime.DSTPageCacheResize(fd, 2*pcPage)
	if mem[pcPage] != 0 {
		t.Errorf("re-grown page = %q, want zero: truncation dropped it", mem[pcPage])
	}
}

// TestDSTPageCacheSharesBytesNotProtections is what a single anonymous arena
// cannot model, and what a database that maps its data file read-only while
// writing to it through write(2) depends on: two mappings of one file see each
// other's bytes, yet each carries its own protection.
func TestDSTPageCacheSharesBytesNotProtections(t *testing.T) {
	fd, rw := newPageCache(t, pcPage, pcPage)
	ro := runtime.DSTPageCacheMap(fd, pcPage, runtime.DSTProtRead)
	defer runtime.DSTPageCacheUnmap(ro, pcPage, runtime.DSTSpanUnmapped)

	mappingBytes(rw, pcPage)[0] = 'X'
	if got := mappingBytes(ro, pcPage)[0]; got != 'X' {
		t.Errorf("read-only mapping sees %q, want the writer's 'X': the pages are not shared", got)
	}
	// The protections themselves are pinned by TestDSTMappingFaultReadOnlyWrite,
	// which needs a simulated process in order to survive observing the fault.
}

// TestDSTMappingFaultAddr pins the classifier that decides whether a fault
// belongs to a simulated mapping. Its precision is load-bearing in both
// directions: a false negative aborts the harness where a process should have
// died, and a false positive launders a genuine harness nil-dereference into a
// simulated process death.
func TestDSTMappingFaultAddr(t *testing.T) {
	// Tombstones persist until the run boundary, so a neighbor test's released
	// mapping would otherwise claim adjacent addresses here.
	runtime.DSTPageCacheReset()
	fd := runtime.DSTPageCacheNew()
	runtime.DSTPageCacheResize(fd, pcPage)
	base := runtime.DSTPageCacheMap(fd, pcPage, runtime.DSTProtRead)

	if st, ok := runtime.DSTMappingFaultAddr(base); !ok || st != 0 {
		t.Errorf("first byte of a mapping = (%d,%v), want live", st, ok)
	}
	if st, ok := runtime.DSTMappingFaultAddr(base + pcPage - 1); !ok || st != 0 {
		t.Errorf("last byte of a mapping = (%d,%v), want live", st, ok)
	}
	if _, ok := runtime.DSTMappingFaultAddr(base + pcPage); ok {
		t.Errorf("byte just past a mapping attributed to it")
	}
	if _, ok := runtime.DSTMappingFaultAddr(base - 1); ok {
		t.Errorf("byte just before a mapping attributed to it")
	}
	var local int
	if _, ok := runtime.DSTMappingFaultAddr(uintptr(unsafe.Pointer(&local))); ok {
		t.Errorf("a stack address attributed to a mapping")
	}
	if _, ok := runtime.DSTMappingFaultAddr(0); ok {
		t.Errorf("a nil dereference attributed to a mapping")
	}

	// A released range is a TOMBSTONE, not unclaimed: the registry keeps what
	// the range used to be, so a later fault is attributed to that — the
	// toucher's own death for unmapped memory, a named abort for a crashed
	// owner's — instead of reading as a harness bug.
	runtime.DSTPageCacheUnmap(base, pcPage, runtime.DSTSpanUnmapped)
	runtime.DSTPageCacheClose(fd)
	if st, ok := runtime.DSTMappingFaultAddr(base); !ok || st != runtime.DSTSpanUnmapped {
		t.Errorf("an unmapped range = (%d,%v), want the unmapped tombstone", st, ok)
	}
	fd2 := runtime.DSTPageCacheNew()
	runtime.DSTPageCacheResize(fd2, pcPage)
	dead := runtime.DSTPageCacheMap(fd2, pcPage, runtime.DSTProtRead)
	runtime.DSTPageCacheUnmap(dead, pcPage, runtime.DSTSpanCrashed)
	runtime.DSTPageCacheClose(fd2)
	if st, ok := runtime.DSTMappingFaultAddr(dead); !ok || st != runtime.DSTSpanCrashed {
		t.Errorf("a crashed owner's range = (%d,%v), want the crashed tombstone", st, ok)
	}
	runtime.DSTPageCacheReset()
	if _, ok := runtime.DSTMappingFaultAddr(base); ok {
		t.Errorf("the run-boundary reset left a tombstone claiming addresses")
	}
}

// faultInProcess runs touch inside a simulated process and reports whether that
// process died, whether it recovered, and whether a peer process outlived it.
// The victim installs debug.SetPanicOnFault — the strongest attempt the
// language affords to survive a fault. It must not work here: production
// delivers SIGBUS, and no process declines that.
func faultInProcess(t *testing.T, touch func()) (died, recovered, peerRan bool) {
	t.Helper()
	var rec, past, peer atomic.Bool
	var victimPID atomic.Int64
	var killErr error

	simulation.Test(t, 1, func(t *testing.T) {
		simulation.Host("h", simulation.HostConfig{}, func() {
			started := make(chan struct{})
			go simulation.Process("victim", func() {
				defer func() {
					if r := recover(); r != nil {
						rec.Store(true)
					}
				}()
				debug.SetPanicOnFault(true)
				victimPID.Store(int64(os.Getpid()))
				close(started)
				touch()
				past.Store(true) // unreachable if touch faulted
			})
			<-started
			simulation.Process("peer", func() {
				killErr = syscall.Kill(int(victimPID.Load()), 0)
				peer.Store(true)
			})
		})
	})
	return !past.Load() && killErr == syscall.ESRCH, rec.Load(), peer.Load()
}

// TestDSTMappingFaultPastEOF: a mapping may reserve address space past the
// file's end, and a load from it is production's SIGBUS. The process that took
// it dies whole; its peer and the harness run on.
func TestDSTMappingFaultPastEOF(t *testing.T) {
	_, base := newPageCache(t, pcPage, 2*pcPage) // one page of file, two of reservation
	mem := mappingBytes(base, 2*pcPage)

	died, recovered, peerRan := faultInProcess(t, func() {
		pcSink.Store(uint32(mem[0]))      // inside the file: fine
		pcSink.Store(uint32(mem[pcPage])) // past end-of-file: SIGBUS
	})
	if !died {
		t.Errorf("the process survived a load from a page past its file's end")
	}
	if recovered {
		t.Errorf("the process recovered from a fault production delivers as SIGBUS")
	}
	if !peerRan {
		t.Errorf("the peer process did not run: the fault took down more than its process")
	}
}

// TestDSTMappingFaultShrinkUnderLiveMapping: truncating a file that is mapped
// is legal, and it is the pages past the new end that trap — not the truncate
// call. This is the shape a database performs on every commit.
func TestDSTMappingFaultShrinkUnderLiveMapping(t *testing.T) {
	fd, base := newPageCache(t, 2*pcPage, 2*pcPage)
	mem := mappingBytes(base, 2*pcPage)
	mem[pcPage] = 'z'
	runtime.DSTPageCacheResize(fd, pcPage) // shrink out from under the live mapping

	died, recovered, peerRan := faultInProcess(t, func() {
		pcSink.Store(uint32(mem[0]))      // still within the file
		pcSink.Store(uint32(mem[pcPage])) // cut away by the shrink: SIGBUS
	})
	if !died {
		t.Errorf("the process survived a load from a page the truncate cut away")
	}
	if recovered {
		t.Errorf("the process recovered from a fault production delivers as SIGBUS")
	}
	if !peerRan {
		t.Errorf("the peer process did not run")
	}
}

// TestDSTMappingFaultReadOnlyWrite: a store through a read-only mapping is a
// fault, enforced by the hardware rather than by bookkeeping.
func TestDSTMappingFaultReadOnlyWrite(t *testing.T) {
	fd, _ := newPageCache(t, pcPage, pcPage)
	ro := runtime.DSTPageCacheMap(fd, pcPage, runtime.DSTProtRead)
	defer runtime.DSTPageCacheUnmap(ro, pcPage, runtime.DSTSpanUnmapped)
	mem := mappingBytes(ro, pcPage)

	died, recovered, peerRan := faultInProcess(t, func() {
		pcSink.Store(uint32(mem[0])) // reading is allowed
		mem[0] = 'w'                 // writing is not
	})
	if !died {
		t.Errorf("the process survived a store through a read-only mapping")
	}
	if recovered {
		t.Errorf("the process recovered from a protection fault")
	}
	if !peerRan {
		t.Errorf("the peer process did not run")
	}
}

// TestDSTMappingAddressDeterministicAcrossInvocations is DST-FAULT-REPLAY for
// addresses: the system under test holds the slice a mapping returns, so its
// address is observable — it can key a map and steer iteration — and replay
// therefore requires it be a pure function of the schedule, across process
// invocations, exactly as the fork already pins the heap base. Kernel-chosen
// mmap addresses fail this: mmap_base is randomized per exec.
func TestDSTMappingAddressDeterministicAcrossInvocations(t *testing.T) {
	got1 := runTestProgDST(t, "DSTMappingAddr")
	got2 := runTestProgDST(t, "DSTMappingAddr")
	if got1 != got2 {
		t.Fatalf("one mapping sequence, two invocations, two address sets:\n%q\n%q", got1, got2)
	}
	if !strings.HasPrefix(got1, fmt.Sprintf("%#x", uintptr(runtime.DSTMapRegionBase))) {
		t.Fatalf("first mapping = %q, want the region base %#x", got1, uintptr(runtime.DSTMapRegionBase))
	}
}

// TestDSTPageCacheCarveResetDeterminism pins the within-process half of the
// same property: after a run-boundary reset, an identical mapping sequence
// lands at identical addresses, so one seed yields one address however many
// runs precede it in the process.
func TestDSTPageCacheCarveResetDeterminism(t *testing.T) {
	sequence := func() [2]uintptr {
		fd := runtime.DSTPageCacheNew()
		runtime.DSTPageCacheResize(fd, pcPage)
		a := runtime.DSTPageCacheMap(fd, 64<<10, runtime.DSTProtRead|runtime.DSTProtWrite)
		b := runtime.DSTPageCacheMap(fd, pcPage, runtime.DSTProtRead)
		runtime.DSTPageCacheUnmap(b, pcPage, runtime.DSTSpanUnmapped)
		runtime.DSTPageCacheUnmap(a, 64<<10, runtime.DSTSpanUnmapped)
		runtime.DSTPageCacheClose(fd)
		return [2]uintptr{a, b}
	}
	runtime.DSTPageCacheReset()
	r1 := sequence()
	runtime.DSTPageCacheReset()
	r2 := sequence()
	runtime.DSTPageCacheReset()
	if r1 != r2 {
		t.Fatalf("one sequence, two runs, two address sets: %#x vs %#x", r1, r2)
	}
	if r1[0] != uintptr(runtime.DSTMapRegionBase) {
		t.Errorf("first carve = %#x, want the region base %#x", r1[0], uintptr(runtime.DSTMapRegionBase))
	}
}

// TestDSTPageCacheRegionExhaustionIsDeterministicENOMEM: the region is the
// resource, so running out of it is a pure function of the run's mapping
// sequence — unlike kernel ENOMEM, which is host state. The sentinel is 0,
// which the os layer hands to the system under test as ENOMEM.
func TestDSTPageCacheRegionExhaustionIsDeterministicENOMEM(t *testing.T) {
	runtime.DSTPageCacheReset()
	defer runtime.DSTPageCacheReset()
	fd := runtime.DSTPageCacheNew()
	defer runtime.DSTPageCacheClose(fd)
	runtime.DSTPageCacheResize(fd, pcPage)
	almost := uintptr(runtime.DSTMapRegionSize) - 64<<10
	a := runtime.DSTPageCacheMap(fd, almost, runtime.DSTProtRead)
	if a == 0 {
		t.Fatalf("a %d-byte reservation did not fit an empty region", almost)
	}
	defer runtime.DSTPageCacheUnmap(a, almost, runtime.DSTSpanUnmapped)
	if b := runtime.DSTPageCacheMap(fd, 128<<10, runtime.DSTProtRead); b != 0 {
		t.Fatalf("a carve past the region's end returned %#x, want the 0 sentinel", b)
	}
	if c := runtime.DSTPageCacheMap(fd, 64<<10, runtime.DSTProtRead); c == 0 {
		t.Fatalf("a carve that fits the remainder was refused")
	} else {
		runtime.DSTPageCacheUnmap(c, 64<<10, runtime.DSTSpanUnmapped)
	}
}

// TestDSTCrashedMappingTouchIsNamed: reaching a dead owner's mapping is a
// model-boundary violation with no production analog (the memory does not
// exist), and it must abort with the NAMED reason — not "unexpected fault
// address" (which reads as a harness bug), and not a laundered process death.
func TestDSTCrashedMappingTouchIsNamed(t *testing.T) {
	got := runTestProgDST(t, "DSTCrashedMappingTouch")
	if !strings.Contains(got, "dst: access to a dead owner's mapping") {
		t.Fatalf("touching a crashed owner's mapping reported:\n%s\nwant the named abort", got)
	}
	if strings.Contains(got, "UNREACHED") {
		t.Fatalf("the touch of a dead mapping did not abort:\n%s", got)
	}
}
