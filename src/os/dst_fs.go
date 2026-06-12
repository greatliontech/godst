// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import (
	"internal/poll"
	"io"
	"path"
	"runtime"
	"sync"
	"syscall"
	"time"
	_ "unsafe" // for go:linkname
)

// Under deterministic simulation testing (testing/simulation), the filesystem
// is virtualized to a per-bubble in-memory tree: the exported os surface stops
// touching the host and operates on simulated nodes, reset per run by the run
// epoch (the same mechanism as the net registry). The tree starts EMPTY — the
// host filesystem is never visible under a run; a host path is machine state
// and reading it would make runs machine-dependent. All operations execute
// synchronously on the calling goroutine, so determinism rides the schedule:
// no new scheduler choices, no new RNG. Authoritative model and the durability
// contract: docs/dst/design.md "In-memory deterministic filesystem".
//
// Exported operations not yet modeled are FENCED while a run is active — they
// fail with a deterministic "unsupported under deterministic simulation"
// *PathError/*LinkError instead of leaking to the host (the host-isolation
// invariant). Later increments replace fences with implementations.

//go:linkname dstFSActive runtime.dstActive
func dstFSActive() bool

// The generic per-run epoch; the symbol predates this feature (the net
// registry named it), and both registries key off the same counter.
//
//go:linkname dstFSEpoch runtime.dstNetEpoch
func dstFSEpoch() uint64

// dstFS is the process-global simulated filesystem: a per-run in-memory tree.
// Keyed by the run epoch so a new run starts with a fresh empty root, with no
// explicit teardown hook. All node state (tree shape and file content) is
// guarded by mu; per-handle state (offset, closed) is guarded by the handle's
// own mutex, acquired before mu where both are needed.
var dstFS struct {
	mu    sync.Mutex
	epoch uint64
	root  *dstFSNode
}

// dstFSRoll resets the tree when the run epoch advances. Caller holds dstFS.mu.
func dstFSRoll() {
	if e := dstFSEpoch(); e != dstFS.epoch || dstFS.root == nil {
		dstFS.epoch = e
		dstFS.root = &dstFSNode{
			isDir:   true,
			entries: make(map[string]*dstFSNode),
			mode:    ModeDir | 0o755,
			modTime: time.Now(),
		}
	}
}

// dstFSNode is one node of the simulated tree. Content lives on the node and
// names are references (directory entries), so an open handle keeps a removed
// file's content alive — the POSIX unlinked-but-open contract.
//
// The durability representation is carried from day one (the spec's
// durability contract): data is the current content every observer sees;
// synced is the durable image a future simulated crash restores. Mutations
// touch only data; Sync replaces synced with a copy of data. In the base
// (no-fault) model the distinction is invisible — crash never fires — but the
// write path maintaining it is what lets the fault feature add policies
// without new representation. syncedEntries is the directory-side durable
// image (entry durability is the parent directory's property, POSIX-style);
// the durability increment wires its commit points.
type dstFSNode struct {
	isDir bool

	// Regular file state.
	data   []byte
	synced []byte

	// Directory state.
	entries       map[string]*dstFSNode
	syncedEntries map[string]*dstFSNode

	mode    FileMode
	modTime time.Time

	// Durable metadata image, advanced only by sync (with the content
	// image); a future simulated crash restores these alongside synced.
	syncedMode    FileMode
	syncedModTime time.Time
}

// dstErrUnsupportedFS is the inner error of every fenced operation; the same
// message shape as the net feature's gates.
var dstErrUnsupportedFS = newUnsupportedFSError()

func newUnsupportedFSError() error { return &dstUnsupportedFSError{} }

type dstUnsupportedFSError struct{}

func (*dstUnsupportedFSError) Error() string {
	return "filesystem operation unsupported under deterministic simulation"
}

// dstFSFenced reports whether op on name must be fenced (a run is active and
// the operation is not yet modeled). The bool follows the dstSim* accessor
// convention: false means fall through to the real implementation.
func dstFSFenced(op, name string) (error, bool) {
	if !dstFSActive() {
		return nil, false
	}
	return &PathError{Op: op, Path: name, Err: dstErrUnsupportedFS}, true
}

// dstFSFencedLink is dstFSFenced for two-name operations (*LinkError shape).
func dstFSFencedLink(op, oldname, newname string) (error, bool) {
	if !dstFSActive() {
		return nil, false
	}
	return &LinkError{Op: op, Old: oldname, New: newname, Err: dstErrUnsupportedFS}, true
}

// dstFSResolve walks cleaned absolute path name (resolved against the
// per-bubble root; the base-model working directory is "/") and returns the
// parent directory node, the base name, and the target node (nil if absent).
// Caller holds dstFS.mu. Errors are bare errnos; callers wrap.
func dstFSResolve(name string) (parent *dstFSNode, base string, node *dstFSNode, errno error) {
	clean := path.Clean("/" + name)
	if clean == "/" {
		return nil, "/", dstFS.root, nil
	}
	dir := dstFS.root
	rest := clean[1:]
	for {
		i := 0
		for i < len(rest) && rest[i] != '/' {
			i++
		}
		elem := rest[:i]
		if i == len(rest) {
			return dir, elem, dir.entries[elem], nil
		}
		next := dir.entries[elem]
		if next == nil {
			return nil, "", nil, syscall.ENOENT
		}
		if !next.isDir {
			return nil, "", nil, syscall.ENOTDIR
		}
		dir = next
		rest = rest[i+1:]
	}
}

// dstFile is the open-handle state for a simulated file: the node reference,
// the seek offset, and the access mode. It is the tree-file backend behind
// the os.File dst seam; dst-io's pipe plugs in as a sibling backend later
// (the non-foreclosure shape recorded in the spec).
type dstFile struct {
	mu     sync.Mutex
	node   *dstFSNode
	path   string
	off    int64
	rd, wr bool
	app    bool
	closed bool
}

// dstOpenFile implements OpenFile against the simulated tree while a run is
// active. handled=false outside a run (fall through to the host).
func dstOpenFile(name string, flag int, perm FileMode) (f *File, handled bool, err error) {
	if !dstFSActive() {
		return nil, false, nil
	}
	wrap := func(e error) (*File, bool, error) {
		return nil, true, &PathError{Op: "open", Path: name, Err: e}
	}
	if name == "" {
		return wrap(syscall.ENOENT)
	}

	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()

	parent, base, node, errno := dstFSResolve(name)
	if errno != nil {
		return wrap(errno)
	}

	accWrite := flag&(O_WRONLY|O_RDWR) != 0
	if node != nil && node.isDir {
		if accWrite {
			return wrap(syscall.EISDIR)
		}
		// Directory handles (ReadDir, directory Sync) are the namespace
		// increment; until then a directory open is fenced, not half-modeled.
		return wrap(dstErrUnsupportedFS)
	}

	switch {
	case node == nil && flag&O_CREATE == 0:
		return wrap(syscall.ENOENT)
	case node != nil && flag&(O_CREATE|O_EXCL) == O_CREATE|O_EXCL:
		return wrap(syscall.EEXIST)
	case node == nil:
		node = &dstFSNode{
			mode:    perm & ModePerm,
			modTime: time.Now(),
		}
		parent.entries[base] = node
		parent.modTime = time.Now()
	}
	if flag&O_TRUNC != 0 {
		// Linux truncates even for O_RDONLY|O_TRUNC; match the host's shape.
		// Truncation mutates current content only; the durable image moves
		// on Sync, never on a mutation (the durability contract).
		node.data = nil
		node.modTime = time.Now()
	}

	d := &dstFile{
		node: node,
		path: path.Clean("/" + name),
		rd:   flag&O_WRONLY == 0,
		wr:   accWrite,
		app:  flag&O_APPEND != 0,
	}
	return dstNewFile(d, name), true, nil
}

// dstNewFile builds an *os.File backed by a simulated file. The pfd is left
// with an invalid Sysfd so any not-yet-gated path fails with EBADF
// deterministically instead of touching a real descriptor.
func dstNewFile(d *dstFile, name string) *File {
	f := &File{&file{name: name, dstf: d}}
	f.pfd.Sysfd = -1
	runtime.SetFinalizer(f.file, (*file).close)
	return f
}

// enter marks the start of a handle operation: it validates the handle and
// acquires the tree lock (handle lock first, then tree lock — the fixed
// order). Callers must call leave (via defer) when it returns nil.
func (d *dstFile) enter() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return poll.ErrFileClosing
	}
	dstFS.mu.Lock()
	return nil
}

func (d *dstFile) leave() {
	dstFS.mu.Unlock()
	d.mu.Unlock()
}

func (d *dstFile) read(b []byte) (int, error) {
	if err := d.enter(); err != nil {
		return 0, err
	}
	defer d.leave()
	if len(b) == 0 {
		// Upstream poll.FD.Read returns (0, nil) for empty buffers before
		// the access-mode and EOF checks; mirror that order exactly.
		return 0, nil
	}
	if !d.rd {
		return 0, syscall.EBADF
	}
	if d.off >= int64(len(d.node.data)) {
		return 0, io.EOF
	}
	n := copy(b, d.node.data[d.off:])
	d.off += int64(n)
	return n, nil
}

func (d *dstFile) pread(b []byte, off int64) (int, error) {
	if err := d.enter(); err != nil {
		return 0, err
	}
	defer d.leave()
	if !d.rd {
		return 0, syscall.EBADF
	}
	if off >= int64(len(d.node.data)) {
		return 0, io.EOF
	}
	return copy(b, d.node.data[off:]), nil
}

func (d *dstFile) write(b []byte) (int, error) {
	if err := d.enter(); err != nil {
		return 0, err
	}
	defer d.leave()
	if !d.wr {
		return 0, syscall.EBADF
	}
	if d.app {
		d.off = int64(len(d.node.data))
	}
	n := d.writeAtLocked(b, d.off)
	d.off += int64(n)
	return n, nil
}

func (d *dstFile) pwrite(b []byte, off int64) (int, error) {
	if err := d.enter(); err != nil {
		return 0, err
	}
	defer d.leave()
	if !d.wr {
		return 0, syscall.EBADF
	}
	return d.writeAtLocked(b, off), nil
}

// writeAtLocked extends-with-zeros as needed and copies b at off. Caller
// holds both locks. Mutates current content only (never the durable image).
func (d *dstFile) writeAtLocked(b []byte, off int64) int {
	node := d.node
	if need := off + int64(len(b)); need > int64(len(node.data)) {
		if need > int64(cap(node.data)) {
			// append for amortized growth (an exact-size make would copy
			// O(n^2) over append-heavy workloads).
			node.data = append(node.data, make([]byte, need-int64(len(node.data)))...)
		} else {
			node.data = node.data[:need]
		}
	}
	n := copy(node.data[off:], b)
	node.modTime = time.Now()
	return n
}

func (d *dstFile) seek(offset int64, whence int) (int64, error) {
	if err := d.enter(); err != nil {
		return 0, err
	}
	defer d.leave()
	var base int64
	switch whence {
	case io.SeekStart:
		base = 0
	case io.SeekCurrent:
		base = d.off
	case io.SeekEnd:
		base = int64(len(d.node.data))
	default:
		return 0, syscall.EINVAL
	}
	pos := base + offset
	if pos < 0 {
		return 0, syscall.EINVAL
	}
	d.off = pos
	return pos, nil
}

func (d *dstFile) truncate(size int64) error {
	if err := d.enter(); err != nil {
		return err
	}
	defer d.leave()
	if !d.wr {
		return syscall.EINVAL
	}
	if size < 0 {
		return syscall.EINVAL
	}
	node := d.node
	switch {
	case size <= int64(len(node.data)):
		node.data = node.data[:size]
	default:
		grown := make([]byte, size)
		copy(grown, node.data)
		node.data = grown
	}
	node.modTime = time.Now()
	return nil
}

// sync commits the file's current content as the durable image (the
// durability contract's file-data commit point; entry durability is the
// parent directory's, wired by the durability increment).
func (d *dstFile) sync() error {
	if err := d.enter(); err != nil {
		return err
	}
	defer d.leave()
	d.node.synced = append([]byte(nil), d.node.data...)
	d.node.syncedMode = d.node.mode
	d.node.syncedModTime = d.node.modTime
	return nil
}

func (d *dstFile) stat() (FileInfo, error) {
	if err := d.enter(); err != nil {
		return nil, err
	}
	defer d.leave()
	return &dstFileInfo{
		name:    path.Base(d.path),
		size:    int64(len(d.node.data)),
		mode:    d.node.mode,
		modTime: d.node.modTime,
		isDir:   d.node.isDir,
		node:    d.node,
	}, nil
}

// closeFile closes the handle. The node stays alive while other handles or
// directory entries reference it.
func (d *dstFile) closeFile() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return poll.ErrFileClosing
	}
	d.closed = true
	d.node = nil
	return nil
}

// dstFileInfo is the FileInfo for simulated nodes. Sys() is nil: there is no
// host stat to report, and synthesizing one is a fidelity decision for a
// later increment.
type dstFileInfo struct {
	name    string
	size    int64
	mode    FileMode
	modTime time.Time
	isDir   bool
	node    *dstFSNode // identity for SameFile
}

// dstSameFile reports SameFile for simulated FileInfos. handled=false when
// neither side is simulated; a simulated/host mix is decidedly not the same
// file.
func dstSameFile(fi1, fi2 FileInfo) (same, handled bool) {
	d1, ok1 := fi1.(*dstFileInfo)
	d2, ok2 := fi2.(*dstFileInfo)
	if !ok1 && !ok2 {
		return false, false
	}
	return ok1 && ok2 && d1.node == d2.node && d1.node != nil, true
}

func (fi *dstFileInfo) Name() string       { return fi.name }
func (fi *dstFileInfo) Size() int64        { return fi.size }
func (fi *dstFileInfo) Mode() FileMode     { return fi.mode }
func (fi *dstFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *dstFileInfo) IsDir() bool        { return fi.isDir }
func (fi *dstFileInfo) Sys() any           { return nil }
