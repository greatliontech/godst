// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import (
	"sync"
	"syscall"
	"unsafe"
)

const (
	dstMadvCold         = 20
	dstMadvPopulateRead = 22
)

// dstMMapPageSize is the SIMULATED page size: a fixed constant, not the host's
// syscall.Getpagesize(), so a seed accepts exactly the same offsets on a
// 4K-page x86 machine and a 16K-page arm64 one — page geometry is machine
// state, and validating against it would make run outcomes machine-dependent.
// 4096 is the ubiquitous value; every 16K-aligned offset is also 4K-aligned,
// so host-derived offsets stay valid.
const dstMMapPageSize = 4096

//go:linkname dstSetMmapHook syscall.dstSetMmapHook
func dstSetMmapHook(func(fd int, offset int64, length int, prot int, flags int) (data []byte, err syscall.Errno, handled bool))

//go:linkname dstSetMunmapHook syscall.dstSetMunmapHook
func dstSetMunmapHook(func(data []byte) (err syscall.Errno, handled bool))

//go:linkname dstSetMprotectHook syscall.dstSetMprotectHook
func dstSetMprotectHook(func(data []byte, prot int) (err syscall.Errno, handled bool))

//go:linkname dstSetMadviseHook syscall.dstSetMadviseHook
func dstSetMadviseHook(func(data []byte, advice int) (err syscall.Errno, handled bool))

func init() {
	dstSetMmapHook(dstFDMmap)
	dstSetMunmapHook(dstMunmap)
	dstSetMprotectHook(dstMprotect)
	dstSetMadviseHook(dstMadvise)
}

type dstMMapEntry struct {
	data     []byte
	base     []byte
	node     *dstFSNode
	epoch    uint64
	seq      uint64 // per-run registration sequence (see dstMMapRegistry.seq)
	host     uint32
	proc     uint32
	off      int64
	writable bool
}

var dstMMapRegistry struct {
	mu    sync.Mutex
	epoch uint64
	maps  map[*byte][]*dstMMapEntry
	// seq stamps each registration in Mmap-call order — a pure function of the
	// schedule — so any tie among candidates is broken by seq, never by heap
	// address (allocation addresses vary run to run; the fixed -tags dst hash
	// key does not make them reproducible).
	seq uint64
}

func dstMMapRollLocked() {
	if e := dstFSEpoch(); e != dstMMapRegistry.epoch || dstMMapRegistry.maps == nil {
		dstMMapRegistry.epoch = e
		dstMMapRegistry.maps = make(map[*byte][]*dstMMapEntry)
		dstMMapRegistry.seq = 0
	}
}

func dstMMapKey(data []byte) (*byte, syscall.Errno) {
	if len(data) == 0 || len(data) != cap(data) {
		return nil, syscall.EINVAL
	}
	return &data[cap(data)-1], 0
}

func dstMMapRange(data []byte) (uintptr, uintptr, syscall.Errno) {
	if len(data) == 0 {
		return 0, 0, syscall.EINVAL
	}
	start := uintptr(unsafe.Pointer(&data[0]))
	end := start + uintptr(len(data))
	if end < start {
		return 0, 0, syscall.EINVAL
	}
	return start, end, 0
}

func dstFDMmap(fd int, offset int64, length int, prot int, flags int) ([]byte, syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return nil, errno, handled
	}
	if length <= 0 || offset < 0 {
		return nil, syscall.EINVAL, true
	}
	if offset%dstMMapPageSize != 0 {
		return nil, syscall.EINVAL, true
	}
	if prot != syscall.PROT_READ && prot != syscall.PROT_READ|syscall.PROT_WRITE {
		return nil, syscall.EACCES, true
	}
	writable := prot&syscall.PROT_WRITE != 0
	if flags != syscall.MAP_SHARED {
		return nil, syscall.EINVAL, true
	}
	file, ok := entry.backend.(*dstFile)
	if !ok || file.node == nil || file.node.isDir {
		return nil, syscall.ENODEV, true
	}

	file.diskDelay()
	if err := file.enter(); err != nil {
		return nil, dstFDErr(err), true
	}
	defer file.leave()
	if !file.rd || writable && !file.wr {
		return nil, syscall.EACCES, true
	}
	if err := file.diskEIO(); err != nil {
		return nil, dstFDErr(err), true
	}
	if offset > int64(len(file.node.data)) || int64(length) > int64(len(file.node.data))-offset {
		return nil, syscall.EINVAL, true
	}

	dstMMapSyncLocked(file.node)
	host, proc := dstFSCurrentNode()
	dstMMapRegistry.mu.Lock()
	dstMMapRollLocked()
	data, base, errno := dstMMapDataLocked(file.node, offset, length)
	if errno != 0 {
		dstMMapRegistry.mu.Unlock()
		return nil, errno, true
	}
	key, errno := dstMMapKey(data)
	if errno != 0 {
		dstMMapRegistry.mu.Unlock()
		return nil, errno, true
	}
	dstMMapRegistry.seq++
	dstMMapRegistry.maps[key] = append(dstMMapRegistry.maps[key], &dstMMapEntry{
		data:     data,
		base:     base,
		node:     file.node,
		epoch:    dstFSEpoch(),
		seq:      dstMMapRegistry.seq,
		host:     host,
		proc:     proc,
		off:      offset,
		writable: writable,
	})
	dstMMapRegistry.mu.Unlock()
	return data, 0, true
}

func dstMMapDataLocked(node *dstFSNode, offset int64, length int) ([]byte, []byte, syscall.Errno) {
	end := offset + int64(length)
	start := int(offset)
	stop := int(end)
	var overlapCandidate, spareCandidate []byte
	var spareSeq uint64
	for _, bucket := range dstMMapRegistry.maps {
		for _, entry := range bucket {
			if entry.node != node || entry.epoch != dstFSEpoch() {
				continue
			}
			mapStart := entry.off
			mapEnd := entry.off + int64(len(entry.data))
			overlaps := offset < mapEnd && end > mapStart
			if overlaps {
				if int64(cap(entry.base)) < end {
					return nil, nil, syscall.EINVAL
				}
				if overlapCandidate == nil {
					overlapCandidate = entry.base
				} else if &overlapCandidate[0] != &entry.base[0] {
					return nil, nil, syscall.EINVAL
				}
			} else if int64(cap(entry.base)) >= end {
				// Spare-base choice: largest capacity, ties by earliest
				// registration (seq) — order-independent over the registry map's
				// nondeterministic iteration, and never by heap address.
				if spareCandidate == nil || cap(entry.base) > cap(spareCandidate) ||
					(cap(entry.base) == cap(spareCandidate) && entry.seq < spareSeq) {
					spareCandidate = entry.base
					spareSeq = entry.seq
				}
			}
		}
	}
	candidate := overlapCandidate
	if candidate == nil {
		candidate = spareCandidate
	}
	if candidate != nil {
		copy(candidate, node.data)
		return candidate[start:stop:stop], candidate, 0
	}
	base := dstMMapNewBase(node, end)
	return base[start:stop:stop], base, 0
}

func dstMMapNewBase(node *dstFSNode, size int64) []byte {
	reserve := int(size)
	if reserve < len(node.data) {
		reserve = len(node.data)
	}
	if reserve < dstMMapPageSize {
		reserve = dstMMapPageSize
	} else if rem := reserve % dstMMapPageSize; rem != 0 {
		reserve += dstMMapPageSize - rem
	}
	base := make([]byte, reserve)
	copy(base, node.data)
	return base
}

func dstMMapLookupRange(data []byte) (*dstMMapEntry, syscall.Errno, bool) {
	start, end, errno := dstMMapRange(data)
	if errno != 0 {
		return nil, errno, true
	}
	host, proc := dstFSCurrentNode()
	dstMMapRegistry.mu.Lock()
	dstMMapRollLocked()
	var foundOther bool
	for _, bucket := range dstMMapRegistry.maps {
		for _, candidate := range bucket {
			mapStart, mapEnd, errno := dstMMapRange(candidate.data)
			if errno != 0 {
				continue
			}
			if start < mapStart || end > mapEnd {
				continue
			}
			if candidate.epoch == dstFSEpoch() && candidate.host == host && candidate.proc == proc {
				dstMMapRegistry.mu.Unlock()
				return candidate, 0, true
			}
			if candidate.epoch == dstFSEpoch() {
				foundOther = true
			}
		}
	}
	dstMMapRegistry.mu.Unlock()
	if foundOther {
		return nil, syscall.EINVAL, true
	}
	return nil, 0, false
}

func dstMunmap(data []byte) (syscall.Errno, bool) {
	key, errno := dstMMapKey(data)
	if errno != 0 {
		return errno, true
	}
	start, end, errno := dstMMapRange(data)
	if errno != 0 {
		return errno, true
	}
	host, proc := dstFSCurrentNode()
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstMMapRegistry.mu.Lock()
	defer dstMMapRegistry.mu.Unlock()
	dstMMapRollLocked()
	bucket := dstMMapRegistry.maps[key]
	for i := len(bucket) - 1; i >= 0; i-- {
		entry := bucket[i]
		if entry.epoch == dstFSEpoch() && entry.host == host && entry.proc == proc && len(entry.data) == len(data) && len(entry.data) != 0 && &entry.data[0] == &data[0] {
			dstMMapSyncEntryLocked(entry)
			bucket = append(bucket[:i], bucket[i+1:]...)
			if len(bucket) == 0 {
				delete(dstMMapRegistry.maps, key)
			} else {
				dstMMapRegistry.maps[key] = bucket
			}
			return 0, true
		}
	}
	for _, entry := range bucket {
		if entry.epoch == dstFSEpoch() && len(entry.data) == len(data) && len(entry.data) != 0 && &entry.data[0] == &data[0] {
			return syscall.EINVAL, true
		}
	}
	for _, bucket := range dstMMapRegistry.maps {
		for _, entry := range bucket {
			mapStart, mapEnd, errno := dstMMapRange(entry.data)
			if errno != 0 {
				continue
			}
			if start >= mapStart && end <= mapEnd && entry.epoch == dstFSEpoch() {
				return syscall.EINVAL, true
			}
		}
	}
	return 0, false
}

func dstMMapReleaseProc(proc uint32) {
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstMMapRegistry.mu.Lock()
	defer dstMMapRegistry.mu.Unlock()
	dstMMapRollLocked()
	for key, bucket := range dstMMapRegistry.maps {
		out := bucket[:0]
		for _, entry := range bucket {
			if entry.epoch == dstFSEpoch() && entry.proc == proc {
				dstMMapSyncEntryLocked(entry)
				continue
			}
			out = append(out, entry)
		}
		if len(out) == 0 {
			delete(dstMMapRegistry.maps, key)
		} else {
			dstMMapRegistry.maps[key] = out
		}
	}
}

func dstMprotect(data []byte, prot int) (syscall.Errno, bool) {
	start, end, errno := dstMMapRange(data)
	if errno != 0 {
		return errno, true
	}
	host, proc := dstFSCurrentNode()
	dstMMapRegistry.mu.Lock()
	dstMMapRollLocked()
	var foundCurrent, foundOther, foundWritable, misaligned bool
	for _, bucket := range dstMMapRegistry.maps {
		for _, entry := range bucket {
			mapStart, mapEnd, errno := dstMMapRange(entry.data)
			if errno != 0 || start < mapStart || end > mapEnd || entry.epoch != dstFSEpoch() {
				continue
			}
			// mprotect(2) requires a page-aligned start address. The model's
			// deterministic analog is the FILE offset the subrange starts at
			// (base[0] is file byte 0, so it is the same for every containing
			// entry) — heap addresses would vary run to run.
			if (entry.off+(int64(start)-int64(mapStart)))%dstMMapPageSize != 0 {
				misaligned = true
			}
			if entry.host == host && entry.proc == proc {
				foundCurrent = true
				if entry.writable {
					foundWritable = true
				}
			} else {
				foundOther = true
			}
		}
	}
	dstMMapRegistry.mu.Unlock()
	if (foundCurrent || foundOther) && misaligned {
		return syscall.EINVAL, true
	}
	if foundCurrent {
		if prot == syscall.PROT_READ {
			return 0, true
		}
		if prot == syscall.PROT_READ|syscall.PROT_WRITE && foundWritable {
			return 0, true
		}
		return syscall.EACCES, true
	}
	if foundOther {
		return syscall.EINVAL, true
	}
	return 0, false
}

func dstMadvise(data []byte, advice int) (syscall.Errno, bool) {
	entry, errno, handled := dstMMapLookupRange(data)
	if !handled || errno != 0 {
		return errno, handled
	}
	// madvise(2) requires a page-aligned start address; the deterministic
	// analog is the subrange's file offset (see dstMprotect).
	start, _, errno := dstMMapRange(data)
	if errno != 0 {
		return errno, true
	}
	entryStart, _, errno := dstMMapRange(entry.data)
	if errno != 0 {
		return errno, true
	}
	if (entry.off+(int64(start)-int64(entryStart)))%dstMMapPageSize != 0 {
		return syscall.EINVAL, true
	}
	switch advice {
	case dstMadvPopulateRead, syscall.MADV_HUGEPAGE, dstMadvCold:
		return 0, true
	}
	return syscall.EINVAL, true
}

func dstMMapWriteLocked(node *dstFSNode, off int64, p []byte) {
	if len(p) == 0 {
		return
	}
	dstMMapRegistry.mu.Lock()
	dstMMapRollLocked()
	for _, bucket := range dstMMapRegistry.maps {
		for _, entry := range bucket {
			if entry.node != node {
				continue
			}
			start := off
			end := off + int64(len(p))
			mapStart := entry.off
			mapEnd := entry.off + int64(len(entry.data))
			if end <= mapStart || start >= mapEnd {
				continue
			}
			if start < mapStart {
				start = mapStart
			}
			if end > mapEnd {
				end = mapEnd
			}
			copy(entry.data[start-mapStart:end-mapStart], p[start-off:end-off])
		}
	}
	dstMMapRegistry.mu.Unlock()
}

func dstMMapSyncLocked(node *dstFSNode) {
	dstMMapRegistry.mu.Lock()
	dstMMapRollLocked()
	for _, bucket := range dstMMapRegistry.maps {
		for _, entry := range bucket {
			if entry.node != node {
				continue
			}
			dstMMapSyncEntryLocked(entry)
		}
	}
	dstMMapRegistry.mu.Unlock()
}

func dstMMapSyncEntryLocked(entry *dstMMapEntry) {
	if !entry.writable {
		return
	}
	node := entry.node
	start := entry.off
	end := entry.off + int64(len(entry.data))
	if end <= 0 || start >= int64(len(node.data)) {
		return
	}
	if start < 0 {
		start = 0
	}
	if end > int64(len(node.data)) {
		end = int64(len(node.data))
	}
	copy(node.data[start:end], entry.data[start-entry.off:end-entry.off])
}

// dstMMapShrinkFencedLocked reports whether truncating node to size would cut
// bytes out from under a live mapping. Real Linux keeps the pages mapped and
// delivers SIGBUS on access wholly past the new EOF — an outcome the model
// cannot produce (no VM) — and silently zero-filling instead would hand a DB
// page reader zeros where production dies, masking exactly the torn-file bug
// class DST hunts. So the shrink is fenced loudly (the unsupported shape);
// growth and shrinks clear of every live mapping stay allowed. Caller holds
// dstFS.mu.
func dstMMapShrinkFencedLocked(node *dstFSNode, size int64) bool {
	dstMMapRegistry.mu.Lock()
	defer dstMMapRegistry.mu.Unlock()
	dstMMapRollLocked()
	for _, bucket := range dstMMapRegistry.maps {
		for _, entry := range bucket {
			if entry.node == node && entry.off+int64(len(entry.data)) > size {
				return true
			}
		}
	}
	return false
}
