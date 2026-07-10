// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

// A regular file's bytes live in its page cache from birth: a memfd whose
// length is the file's length, held through a private writable view that
// node.data aliases. Reads and writes copy through that view, and every
// mmap(2) the system under test makes is another mapping of the same memfd —
// so a write() is visible through a mapping, and a store through a mapping is
// visible to read(), because they are the same pages, not because a ledger
// copies them. Sharing, end-of-file enforcement, truncation's tail zeroing,
// and the non-resurrection of dropped bytes are all the kernel's.
//
// Backing is UNIFORM, not lazy: one representation per binary. A lazy split
// (plain slice until first mmap) would store "where the bytes live" as a
// per-node runtime mode, and every mutation site would carry the hazard of
// detaching a backed node's bytes — an append that reallocates moves the file
// out of its page cache and the system under test's mappings silently stop
// seeing writes, a divergence only mmap-heavy runs would ever witness. The
// per-file cost is one descriptor, one view, and a few syscalls at creation,
// against ~512k-descriptor and ~65k-VMA ceilings; a pathological run fails
// loudly at file creation, not silently at map time.
//
// node.data always aliases the view, with capacity equal to the reservation,
// so growth within the reservation is a reslice and never a reallocation.
// Nothing may append to or reassign it: dstNodeSetSizeLocked is the only way
// to change a node's length, and in-place copy the only way to change its
// bytes. The durable image (node.synced) stays an ordinary slice — nothing
// maps it.

package os

import (
	"syscall"
	"unsafe"
)

//go:linkname dstPageCacheNew runtime.dstPageCacheNew
func dstPageCacheNew() int32

//go:linkname dstPageCacheResize runtime.dstPageCacheResize
func dstPageCacheResize(fd int32, size int64)

//go:linkname dstPageCacheMap runtime.dstPageCacheMap
func dstPageCacheMap(fd int32, n uintptr, prot int32) uintptr

//go:linkname dstPageCacheUnmap runtime.dstPageCacheUnmap
func dstPageCacheUnmap(base, n uintptr, state uint8)

// The tombstone states a released mapping's registry entry keeps — mirrors of
// runtime's dstSpan* constants, which decide how a later fault into the range
// is reported (the toucher's death for unmapped; a named abort for crashed and
// retired).
const (
	dstSpanUnmapped uint8 = 1
	dstSpanCrashed  uint8 = 2
	dstSpanRetired  uint8 = 3
)

//go:linkname dstPageCacheClose runtime.dstPageCacheClose
func dstPageCacheClose(fd int32)

//go:linkname dstPageCacheProtect runtime.dstPageCacheProtect
func dstPageCacheProtect(base, n uintptr, prot int32) bool

//go:linkname dstPageCacheResetRegion runtime.dstPageCacheResetRegion
func dstPageCacheResetRegion()

//go:linkname dstPageCacheFatal runtime.dstPageCacheFatal
func dstPageCacheFatal(reason string)

// dstPageCacheMinReserve is the smallest reservation for a node's own view.
// The view is the model's, never the system under test's — SUT mappings are
// separate mmaps of the same memfd — so outgrowing it is remedied by carving
// a bigger one, invisible to everyone else.
const dstPageCacheMinReserve = 64 << 10

// dstNodeCache is a node's page cache and the model's writable view of it.
type dstNodeCache struct {
	fd      int32
	base    uintptr
	reserve uintptr
}

// dstNodeBackLocked gives a newborn regular node its page cache. Caller holds
// dstFS.mu (the create paths do). The carve cannot fail deterministically
// short of the region itself being exhausted, which dstPageCacheMap reports
// as 0 and this turns into an unswallowable fatal: running the harness out of its own region
// at file-creation time is a capability limit, not an outcome production
// gives open(2).
func dstNodeBackLocked(node *dstFSNode) {
	fd := dstPageCacheNew()
	base := dstPageCacheMap(fd, dstPageCacheMinReserve, syscall.PROT_READ|syscall.PROT_WRITE)
	if base == 0 {
		dstPageCacheFatal("dst: the mapping region cannot hold another file view")
	}
	node.pc = &dstNodeCache{fd: fd, base: base, reserve: dstPageCacheMinReserve}
	node.data = dstNodeViewLocked(node)[:0:dstPageCacheMinReserve]
	dstFS.backed = append(dstFS.backed, node)
}

// dstNodeViewLocked is the node's whole reservation as a slice. Only the
// first len(node.data) bytes are backed by the file; the rest trap, which is
// why nothing may index past node.data's length — and why nothing can without
// tripping a bounds check first.
func dstNodeViewLocked(node *dstFSNode) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(node.pc.base)), node.pc.reserve)
}

// dstNodeSetSizeLocked sets a node's length: an ftruncate, so growth reads as
// zeros, a shrink drops the pages past the new end (zeroing the partial
// page's tail), and neither resurrects bytes an earlier shrink dropped —
// observed at once by every mapping the system under test holds. Caller
// holds dstFS.mu.
func dstNodeSetSizeLocked(node *dstFSNode, size int64) {
	if uintptr(size) > node.pc.reserve {
		dstNodeGrowReserveLocked(node, uintptr(size))
	}
	dstPageCacheResize(node.pc.fd, size)
	node.data = dstNodeViewLocked(node)[:size:node.pc.reserve]
}

// dstNodeGrowReserveLocked carves a larger view and retires the old one. The
// system under test's mappings are untouched: only the address the model
// reads and writes through moves.
func dstNodeGrowReserveLocked(node *dstFSNode, need uintptr) {
	reserve := node.pc.reserve
	for reserve < need {
		reserve *= 2
	}
	base := dstPageCacheMap(node.pc.fd, reserve, syscall.PROT_READ|syscall.PROT_WRITE)
	if base == 0 {
		dstPageCacheFatal("dst: the mapping region cannot hold the grown file view")
	}
	dstPageCacheUnmap(node.pc.base, node.pc.reserve, dstSpanRetired)
	node.pc.base = base
	node.pc.reserve = reserve
}

// dstNodeReleaseRunLocked tears down the run that is ending: every page
// cache's descriptor is closed, and one region reset takes back every view
// and every system-under-test mapping at a stroke — a mapping outliving its
// run would leave the runtime's fault registry claiming addresses no
// simulated file owns. Caller holds dstFS.mu; the run boundary guarantees no
// mapping is in flight.
func dstNodeReleaseRunLocked() {
	for _, node := range dstFS.backed {
		dstPageCacheClose(node.pc.fd)
		node.pc = nil
		node.data = nil
	}
	dstFS.backed = nil
	dstPageCacheResetRegion()
}
