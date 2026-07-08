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
	data  []byte
	node  *dstFSNode
	epoch uint64
	host  uint32
	proc  uint32
	off   int64
}

var dstMMapRegistry struct {
	mu    sync.Mutex
	epoch uint64
	maps  map[*byte]*dstMMapEntry
}

func dstMMapRollLocked() {
	if e := dstFSEpoch(); e != dstMMapRegistry.epoch || dstMMapRegistry.maps == nil {
		dstMMapRegistry.epoch = e
		dstMMapRegistry.maps = make(map[*byte]*dstMMapEntry)
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
	if offset%int64(syscall.Getpagesize()) != 0 {
		return nil, syscall.EINVAL, true
	}
	if prot != syscall.PROT_READ {
		return nil, syscall.EACCES, true
	}
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
	if !file.rd {
		return nil, syscall.EACCES, true
	}
	if err := file.diskEIO(); err != nil {
		return nil, dstFDErr(err), true
	}
	if offset > int64(len(file.node.data)) || int64(length) > int64(len(file.node.data))-offset {
		return nil, syscall.EINVAL, true
	}

	data := make([]byte, length)
	copy(data, file.node.data[offset:offset+int64(length)])
	key, errno := dstMMapKey(data)
	if errno != 0 {
		return nil, errno, true
	}
	host, proc := dstFSCurrentNode()
	dstMMapRegistry.mu.Lock()
	dstMMapRollLocked()
	dstMMapRegistry.maps[key] = &dstMMapEntry{
		data:  data,
		node:  file.node,
		epoch: dstFSEpoch(),
		host:  host,
		proc:  proc,
		off:   offset,
	}
	dstMMapRegistry.mu.Unlock()
	return data, 0, true
}

func dstMMapLookupExact(data []byte) (*dstMMapEntry, syscall.Errno, bool) {
	key, errno := dstMMapKey(data)
	if errno != 0 {
		return nil, errno, true
	}
	host, proc := dstFSCurrentNode()
	dstMMapRegistry.mu.Lock()
	dstMMapRollLocked()
	entry := dstMMapRegistry.maps[key]
	dstMMapRegistry.mu.Unlock()
	if entry == nil {
		return nil, 0, false
	}
	if entry.epoch != dstFSEpoch() || entry.host != host || entry.proc != proc || len(entry.data) == 0 || &entry.data[0] != &data[0] {
		return nil, syscall.EINVAL, true
	}
	return entry, 0, true
}

func dstMMapLookupRange(data []byte) (*dstMMapEntry, syscall.Errno, bool) {
	start, end, errno := dstMMapRange(data)
	if errno != 0 {
		return nil, errno, true
	}
	host, proc := dstFSCurrentNode()
	dstMMapRegistry.mu.Lock()
	dstMMapRollLocked()
	var entry *dstMMapEntry
	for _, candidate := range dstMMapRegistry.maps {
		mapStart, mapEnd, errno := dstMMapRange(candidate.data)
		if errno != 0 {
			continue
		}
		if start >= mapStart && end <= mapEnd {
			entry = candidate
			break
		}
	}
	dstMMapRegistry.mu.Unlock()
	if entry == nil {
		return nil, 0, false
	}
	if entry.epoch != dstFSEpoch() || entry.host != host || entry.proc != proc {
		return nil, syscall.EINVAL, true
	}
	return entry, 0, true
}

func dstMunmap(data []byte) (syscall.Errno, bool) {
	entry, errno, handled := dstMMapLookupExact(data)
	if !handled || errno != 0 {
		return errno, handled
	}
	key := &entry.data[cap(entry.data)-1]
	dstMMapRegistry.mu.Lock()
	delete(dstMMapRegistry.maps, key)
	dstMMapRegistry.mu.Unlock()
	return 0, true
}

func dstMprotect(data []byte, prot int) (syscall.Errno, bool) {
	_, errno, handled := dstMMapLookupRange(data)
	if !handled || errno != 0 {
		return errno, handled
	}
	if prot != syscall.PROT_READ {
		return syscall.EACCES, true
	}
	return 0, true
}

func dstMadvise(data []byte, advice int) (syscall.Errno, bool) {
	_, errno, handled := dstMMapLookupRange(data)
	if !handled || errno != 0 {
		return errno, handled
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
	for _, entry := range dstMMapRegistry.maps {
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
	dstMMapRegistry.mu.Unlock()
}

func dstMMapTruncateLocked(node *dstFSNode, size int64) {
	dstMMapRegistry.mu.Lock()
	dstMMapRollLocked()
	for _, entry := range dstMMapRegistry.maps {
		if entry.node != node {
			continue
		}
		mapEnd := entry.off + int64(len(entry.data))
		if size >= mapEnd {
			continue
		}
		clearStart := size - entry.off
		if clearStart < 0 {
			clearStart = 0
		}
		clear(entry.data[clearStart:])
	}
	dstMMapRegistry.mu.Unlock()
}
