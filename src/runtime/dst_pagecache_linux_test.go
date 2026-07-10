// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package runtime_test

import (
	"os"
	"runtime"
	"runtime/debug"
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
		runtime.DSTPageCacheUnmap(base, reserveN)
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
	defer runtime.DSTPageCacheUnmap(ro, pcPage)

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
	fd := runtime.DSTPageCacheNew()
	runtime.DSTPageCacheResize(fd, pcPage)
	base := runtime.DSTPageCacheMap(fd, pcPage, runtime.DSTProtRead)

	if !runtime.DSTMappingFaultAddr(base) {
		t.Errorf("first byte of a mapping not attributed to it")
	}
	if !runtime.DSTMappingFaultAddr(base + pcPage - 1) {
		t.Errorf("last byte of a mapping not attributed to it")
	}
	if runtime.DSTMappingFaultAddr(base + pcPage) {
		t.Errorf("byte just past a mapping attributed to it")
	}
	if runtime.DSTMappingFaultAddr(base - 1) {
		t.Errorf("byte just before a mapping attributed to it")
	}
	var local int
	if runtime.DSTMappingFaultAddr(uintptr(unsafe.Pointer(&local))) {
		t.Errorf("a stack address attributed to a mapping")
	}
	if runtime.DSTMappingFaultAddr(0) {
		t.Errorf("a nil dereference attributed to a mapping")
	}

	runtime.DSTPageCacheUnmap(base, pcPage)
	runtime.DSTPageCacheClose(fd)
	if runtime.DSTMappingFaultAddr(base) {
		t.Errorf("an unmapped range still claims its addresses")
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
	defer runtime.DSTPageCacheUnmap(ro, pcPage)
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
