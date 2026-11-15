// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import (
	"io"
	"io/fs"
	"syscall"
	_ "unsafe" // for go:linkname
)

// dstHostFS is a read-only io/fs.FS over one host's simulated tree, returned by
// testing/simulation.HostFS for white-box inspection of a node's disk from the
// harness (idiom 2) — without running as that node. A process reading its OWN disk
// uses ordinary os calls inside its Host/Process body (idiom 1). Paths are
// fs.FS-style (slash-separated, relative to the host root, no leading slash).
// Content and directory listings are snapshotted sparsely under dstFS.mu at
// Open, so the returned fs.File reads consistently without holding the lock or
// allocating dense slices for sparse holes.
//
// Each handle is stamped with its run epoch and requires an active bubble caller
// of that same run: post-run, cross-run, and foreign-goroutine calls fail with
// ErrClosed before touching simulation state.
type dstHostFS struct {
	host  uint32
	epoch uint64
}

var (
	_ fs.FS        = dstHostFS{}
	_ fs.StatFS    = dstHostFS{}
	_ fs.ReadDirFS = dstHostFS{}
)

//go:linkname dstHostFSFor
func dstHostFSFor(host uint32) fs.FS {
	if !dstFenceActive() {
		return dstHostFS{host: host}
	}
	return dstHostFS{host: host, epoch: dstFSEpoch()}
}

func (h dstHostFS) resolveLocked(op, name string) (*dstFSNode, string, error) {
	if !fs.ValidPath(name) {
		return nil, "", &PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	if !dstFenceActive() || h.epoch == 0 || h.epoch != dstFSEpoch() {
		return nil, "", &PathError{Op: op, Path: name, Err: ErrClosed}
	}
	dstFS.mu.Lock()
	dstFSRoll()
	d := dstFS.disks[h.host]
	if d == nil {
		// A host with no FS activity reports its baseline (empty root + /tmp).
		// Use a throwaway disk rather than storing one: inspection must not mutate
		// simulation state (the host materializes its own identical baseline if it
		// ever touches the filesystem). newDstFSDisk draws root+/tmp inodes from
		// the shared per-run counter (dstFS.nextIno); restore it so the phantom
		// draw is invisible — every subsequently created file keeps the st_ino it
		// would have had without the inspection (the counter is the only
		// simulation state the throwaway construction mutates; its nodes, page
		// caches, and durable images are discarded with it). The host's real
		// baseline, materialized later, draws these same inodes fresh.
		savedIno := dstFS.nextIno
		d = newDstFSDisk()
		dstFS.nextIno = savedIno
	}
	node := d.root
	base := "."
	// Walk the cleaned fs path within this host's tree. fs.ValidPath guarantees no
	// leading/trailing slash, no empty/"."/".." elements, so the split is safe.
	if name != "." {
		rest := name
		for {
			i := 0
			for i < len(rest) && rest[i] != '/' {
				i++
			}
			elem := rest[:i]
			if !node.isDir {
				dstFS.mu.Unlock()
				return nil, "", &PathError{Op: op, Path: name, Err: syscall.ENOTDIR}
			}
			next := node.entries[elem]
			if next == nil {
				dstFS.mu.Unlock()
				return nil, "", &PathError{Op: op, Path: name, Err: syscall.ENOENT}
			}
			node, base = next, elem
			if i == len(rest) {
				break
			}
			rest = rest[i+1:]
		}
	}
	return node, base, nil
}

func dstHostSortedDirEntriesLocked(node *dstFSNode) []fs.DirEntry {
	names := make([]string, 0, len(node.entries))
	for n := range node.entries {
		names = append(names, n)
	}
	// Insertion sort (sorted listing, matching os.ReadDir; no new os imports).
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	entries := make([]fs.DirEntry, len(names))
	for i, n := range names {
		entries[i] = fs.FileInfoToDirEntry(dstFileInfoForNode(n, node.entries[n]))
	}
	return entries
}

func (h dstHostFS) Stat(name string) (fs.FileInfo, error) {
	node, base, err := h.resolveLocked("stat", name)
	if err != nil {
		return nil, err
	}
	defer dstFS.mu.Unlock()
	return dstFileInfoForNode(base, node), nil
}

func (h dstHostFS) ReadDir(name string) ([]fs.DirEntry, error) {
	node, _, err := h.resolveLocked("open", name)
	if err != nil {
		return nil, err
	}
	defer dstFS.mu.Unlock()
	if !node.isDir {
		return nil, &PathError{Op: "readdir", Path: name, Err: syscall.ENOTDIR}
	}
	return dstHostSortedDirEntriesLocked(node), nil
}

func (h dstHostFS) Open(name string) (fs.File, error) {
	node, base, err := h.resolveLocked("open", name)
	if err != nil {
		return nil, err
	}
	defer dstFS.mu.Unlock()
	f := &dstHostFile{info: dstFileInfoForNode(base, node)}
	if node.isDir {
		f.entries = dstHostSortedDirEntriesLocked(node)
	} else if !node.isDevice() {
		f.img = node.snapshotDataLocked()
	}
	return f, nil
}

// dstHostFile is a read-only fs.File: a sparse snapshot of one tree node taken
// at Open.
type dstHostFile struct {
	info    *dstFileInfo
	img     dstDurableImage
	off     int64
	entries []fs.DirEntry
	dirpos  int
}

func (f *dstHostFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *dstHostFile) Close() error               { return nil }

func (f *dstHostFile) Read(b []byte) (int, error) {
	if f.info.isDir {
		return 0, &PathError{Op: "read", Path: f.info.name, Err: syscall.EISDIR}
	}
	if f.off >= f.img.size {
		return 0, io.EOF
	}
	if len(b) == 0 {
		return 0, nil
	}
	n := f.img.copyAt(b, f.off)
	f.off += int64(n)
	return n, nil
}

func (f *dstHostFile) ReadAt(b []byte, off int64) (int, error) {
	if f.info.isDir {
		return 0, &PathError{Op: "read", Path: f.info.name, Err: syscall.EISDIR}
	}
	if off < 0 {
		return 0, &PathError{Op: "readat", Path: f.info.name, Err: syscall.EINVAL}
	}
	if len(b) == 0 {
		if off >= f.img.size {
			return 0, io.EOF
		}
		return 0, nil
	}
	if off >= f.img.size {
		return 0, io.EOF
	}
	n := f.img.copyAt(b, off)
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

func (f *dstHostFile) Seek(offset int64, whence int) (int64, error) {
	if f.info.isDir {
		return 0, &PathError{Op: "seek", Path: f.info.name, Err: syscall.EISDIR}
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = f.off
	case io.SeekEnd:
		base = f.img.size
	default:
		return 0, &PathError{Op: "seek", Path: f.info.name, Err: syscall.EINVAL}
	}
	pos := base + offset
	if pos < 0 {
		return 0, &PathError{Op: "seek", Path: f.info.name, Err: syscall.EINVAL}
	}
	f.off = pos
	return pos, nil
}

func (f *dstHostFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if !f.info.isDir {
		return nil, &PathError{Op: "readdir", Path: f.info.name, Err: syscall.ENOTDIR}
	}
	rem := f.entries[f.dirpos:]
	if n <= 0 {
		f.dirpos = len(f.entries)
		return rem, nil
	}
	if len(rem) == 0 {
		return nil, io.EOF
	}
	if n > len(rem) {
		n = len(rem)
	}
	f.dirpos += n
	return rem[:n], nil
}
