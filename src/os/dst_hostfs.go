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
// Content and directory listings are snapshotted under dstFS.mu at Open, so the
// returned fs.File reads consistently and without holding the lock.
type dstHostFS struct{ host uint32 }

//go:linkname dstHostFSFor
func dstHostFSFor(host uint32) fs.FS { return dstHostFS{host: host} }

func dstFileInfoForNode(name string, node *dstFSNode) *dstFileInfo {
	return &dstFileInfo{
		name:    name,
		size:    int64(len(node.data)),
		mode:    node.mode,
		modTime: node.modTime,
		isDir:   node.isDir,
		ident:   node,
	}
}

func (h dstHostFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	d := dstFS.disks[h.host]
	if d == nil {
		// A host with no FS activity reports its baseline (empty root + /tmp).
		// Use a throwaway disk rather than storing one: inspection must not mutate
		// simulation state (the host materializes its own identical baseline if it
		// ever touches the filesystem).
		d = newDstFSDisk()
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
				return nil, &PathError{Op: "open", Path: name, Err: syscall.ENOTDIR}
			}
			next := node.entries[elem]
			if next == nil {
				return nil, &PathError{Op: "open", Path: name, Err: syscall.ENOENT}
			}
			node, base = next, elem
			if i == len(rest) {
				break
			}
			rest = rest[i+1:]
		}
	}
	f := &dstHostFile{info: dstFileInfoForNode(base, node)}
	if node.isDir {
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
		f.entries = make([]fs.DirEntry, len(names))
		for i, n := range names {
			f.entries[i] = fs.FileInfoToDirEntry(dstFileInfoForNode(n, node.entries[n]))
		}
	} else {
		f.data = append([]byte(nil), node.data...)
	}
	return f, nil
}

// dstHostFile is a read-only fs.File: a snapshot of one tree node taken at Open.
type dstHostFile struct {
	info    *dstFileInfo
	data    []byte
	off     int
	entries []fs.DirEntry
	dirpos  int
}

func (f *dstHostFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *dstHostFile) Close() error               { return nil }

func (f *dstHostFile) Read(b []byte) (int, error) {
	if f.info.isDir {
		return 0, &PathError{Op: "read", Path: f.info.name, Err: syscall.EISDIR}
	}
	if f.off >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(b, f.data[f.off:])
	f.off += n
	return n, nil
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
