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
	// data is the window the system under test holds: a slice of the real
	// mapping, starting at file offset off. mapBase/mapLen are the mapping
	// itself, which always starts at file offset 0 (see dstPageCacheMap), so
	// data's first byte is mapBase+off.
	data     []byte
	mapBase  uintptr
	mapLen   uintptr
	node    *dstFSNode
	epoch   uint64
	host    uint32
	proc    uint32
	off     int64
	// writable is the map-time prot's write bit; fdWritable is the backing
	// descriptor's access mode at map time — the model's VM_MAYWRITE, which
	// caps what mprotect may grant later (an O_RDWR-backed read-only mapping
	// may upgrade to PROT_READ|PROT_WRITE; an O_RDONLY-backed one may not).
	writable   bool
	fdWritable bool
}

var dstMMapRegistry struct {
	mu    sync.Mutex
	epoch uint64
	maps  map[*byte][]*dstMMapEntry
}

func dstMMapRollLocked() {
	if e := dstFSEpoch(); e != dstMMapRegistry.epoch || dstMMapRegistry.maps == nil {
		dstMMapRegistry.epoch = e
		dstMMapRegistry.maps = make(map[*byte][]*dstMMapEntry)
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
	host, proc := dstFSCurrentNode()
	// enter() above holds dstFS.mu until leave(): node reads here are under the
	// tree lock, and the registry lock nests inside it — the same order
	// dstMunmap and the release paths use.
	//
	// The window may extend past the file's end — a RESERVATION, the shape a
	// database maps its data file with: pages wholly past EOF trap on access
	// (the process dies, as under production SIGBUS) and become readable when
	// the file grows over them, with no remapping. No bound is checked here:
	// offset and length are each int64-bounded, so their uintptr sum cannot
	// wrap on the 64-bit hosts mappings require, and a window the region
	// cannot hold is the carve's deterministic ENOMEM below.

	dstMMapRegistry.mu.Lock()
	mprot := syscall.PROT_READ
	if writable {
		mprot |= syscall.PROT_WRITE
	}
	// The mapping starts at file offset 0 and runs to the end of the window the
	// caller asked for; the caller's window is the tail of it. Pages past the
	// file's end are mapped but not backed: they trap on access, exactly as a
	// reservation over a short file does in production.
	mapLen := uintptr(offset) + uintptr(length)
	mapBase := dstPageCacheMap(file.node.pc.fd, mapLen, int32(mprot))
	if mapBase == 0 {
		// The region is exhausted — deterministically: the bump pointer is a
		// pure function of the run's mapping sequence. Production's mmap says
		// ENOMEM when the address space cannot hold the mapping.
		dstMMapRegistry.mu.Unlock()
		return nil, syscall.ENOMEM, true
	}
	data := unsafe.Slice((*byte)(unsafe.Pointer(mapBase+uintptr(offset))), length)

	dstMMapRollLocked()
	key, errno := dstMMapKey(data)
	if errno != 0 {
		dstPageCacheUnmap(mapBase, mapLen, dstSpanRetired)
		dstMMapRegistry.mu.Unlock()
		return nil, errno, true
	}
	dstMMapRegistry.maps[key] = append(dstMMapRegistry.maps[key], &dstMMapEntry{
		data:       data,
		mapBase:    mapBase,
		mapLen:     mapLen,
		node:       file.node,
		epoch:      dstFSEpoch(),
		host:       host,
		proc:       proc,
		off:        offset,
		writable:   writable,
		fdWritable: file.wr,
	})
	dstMMapRegistry.mu.Unlock()
	return data, 0, true
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
			dstPageCacheUnmap(entry.mapBase, entry.mapLen, dstSpanUnmapped)
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

// dstMMapReleaseHost unmaps every mapping made on host. The host/process crash
// split no longer lives here: a mapping's bytes ARE the page cache's, so a
// dying process leaves its dirty MAP_SHARED pages exactly where the kernel
// leaves them, visible to survivors and to a restart. A dying HOST loses them
// instead, and dstRestoreHostDiskFor is what rewinds the page cache to the
// durable image — this function only gives back the address space.
func dstMMapReleaseHost(host uint32) {
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstMMapRegistry.mu.Lock()
	defer dstMMapRegistry.mu.Unlock()
	dstMMapRollLocked()
	for key, bucket := range dstMMapRegistry.maps {
		out := bucket[:0]
		for _, entry := range bucket {
			if entry.epoch == dstFSEpoch() && entry.host == host {
				dstPageCacheUnmap(entry.mapBase, entry.mapLen, dstSpanCrashed)
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
				dstPageCacheUnmap(entry.mapBase, entry.mapLen, dstSpanCrashed)
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
	var foundCurrent, foundOther, misaligned bool
	fdWritable := true // all matched current mappings must be fd-writable for a write upgrade
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
				fdWritable = fdWritable && entry.fdWritable
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
		// A mapping may not gain a permission its file DESCRIPTOR never had:
		// VM_MAYWRITE follows the fd's access mode at map time, not the
		// map-time prot — so an O_RDWR-backed read-only mapping may upgrade
		// to PROT_READ|PROT_WRITE, and PROT_NONE is always permitted, as
		// Linux gives. Only the R/W bits are modeled (as at map time); the
		// page cache itself is opened read-write, so only the model knows
		// the descriptor's mode.
		if prot&^(syscall.PROT_READ|syscall.PROT_WRITE) != 0 {
			return syscall.EACCES, true
		}
		if prot&syscall.PROT_WRITE != 0 && !fdWritable {
			return syscall.EACCES, true
		}
		if !dstPageCacheProtect(start, end-start, int32(prot)) {
			return syscall.EINVAL, true
		}
		return 0, true
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
