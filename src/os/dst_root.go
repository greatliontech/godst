// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && (unix || wasip1)

package os

import (
	"cmp"
	"errors"
	"path"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"time"
)

type dstRoot struct {
	node *dstFSNode
	disk *dstFSDisk
	// epoch is the run this Root belongs to, stamped at creation.
	// dstRootEnter refuses a Root from a dead run exactly as dstFile.enter
	// refuses a dead handle: the run's nodes were released with the run
	// (their page caches are gone — node.pc is nil), and before this gate a
	// leaked *Root either nil-dereferenced them (op ordered after the next
	// run's first fs op) or silently read the prior run's un-released tree
	// (ordered before it) — run-2 observable state depending on op ordering,
	// a determinism leak. Files opened through a Root are epoch-gated by the
	// same check before dstNewFile can stamp them with the current epoch.
	epoch uint64
}

var dstOpenRoots struct {
	mu    sync.Mutex
	epoch uint64
	roots map[*root]dstOpenFileEntry
	// seq stamps each registration in open order, exactly as
	// dstOpenFiles.seq does for files: close ORDER at teardown is
	// observable in principle, and iterating the pointer-keyed map would
	// order victims by run-varying heap addresses — a silent schedule fork
	// the moment Root.Close gains any observable side effect. Reset with
	// the per-run roll.
	seq uint64
}

func dstOpenRootsRollLocked() {
	if e := dstFSEpoch(); e != dstOpenRoots.epoch || dstOpenRoots.roots == nil {
		dstOpenRoots.epoch = e
		dstOpenRoots.roots = make(map[*root]dstOpenFileEntry)
		dstOpenRoots.seq = 0
	}
}

func dstRegisterRoot(r *root) {
	host, proc := dstFSCurrentNode()
	dstOpenRoots.mu.Lock()
	dstOpenRootsRollLocked()
	dstOpenRoots.seq++
	dstOpenRoots.roots[r] = dstOpenFileEntry{host: host, proc: proc, seq: dstOpenRoots.seq}
	dstOpenRoots.mu.Unlock()
}

func dstUnregisterRoot(r *root) {
	dstOpenRoots.mu.Lock()
	dstOpenRootsRollLocked()
	delete(dstOpenRoots.roots, r)
	dstOpenRoots.mu.Unlock()
}

func dstCloseRoots(match func(dstOpenFileEntry) bool) {
	dstOpenRoots.mu.Lock()
	dstOpenRootsRollLocked()
	type victim struct {
		r   *root
		seq uint64
	}
	var victims []victim
	for r, entry := range dstOpenRoots.roots {
		if match(entry) {
			victims = append(victims, victim{r: r, seq: entry.seq})
			delete(dstOpenRoots.roots, r)
		}
	}
	dstOpenRoots.mu.Unlock()
	// Close in registration order (the same rule as dstCloseOpenFiles): the
	// map yields victims in pointer-hash order, which hashes run-varying
	// heap addresses.
	slices.SortFunc(victims, func(a, b victim) int { return cmp.Compare(a.seq, b.seq) })
	for _, v := range victims {
		if dstRootCloseHook != nil {
			dstRootCloseHook(v.r)
		}
		_ = v.r.Close()
	}
}

// dstRootCloseHook, when non-nil, observes each teardown Close in order — the
// white-box pin for the registration-order teardown (Root.Close has no other
// observable side effect today, which is exactly why the order needs a direct
// observer). Set only by the os test export; nil in production.
var dstRootCloseHook func(*root)

func dstCloseProcRoots(proc uint32) {
	dstCloseRoots(func(e dstOpenFileEntry) bool { return e.proc == proc })
}

func dstCloseHostRoots(host uint32) {
	dstCloseRoots(func(e dstOpenFileEntry) bool { return e.host == host })
}

func dstRootActive(r *Root) bool {
	return r != nil && r.root != nil && r.root.dst != nil
}

func dstNewRoot(name string, node *dstFSNode, disk *dstFSDisk) *Root {
	r := &Root{&root{fd: -1, name: name, dst: &dstRoot{node: node, disk: disk, epoch: dstFSEpoch()}}}
	dstRegisterRoot(r.root)
	runtime.SetFinalizer(r.root, (*root).Close)
	return r
}

func dstOpenRoot(name string) (*Root, bool, error) {
	if !dstFSActive() {
		return nil, false, nil
	}
	if dstProcReserved(name) {
		return nil, true, &PathError{Op: "open", Path: name, Err: dstErrUnsupportedFS}
	}
	dstDiskDelayHere()
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	wrap := func(e error) (*Root, bool, error) { return nil, true, &PathError{Op: "open", Path: name, Err: e} }
	if name == "" {
		return wrap(syscall.ENOENT)
	}
	_, _, node, errno := dstFSResolve(name)
	if errno != nil {
		return wrap(errno)
	}
	if node == nil {
		return wrap(syscall.ENOENT)
	}
	if !node.isDir {
		return wrap(errors.New("not a directory"))
	}
	return dstNewRoot(name, node, dstFSDiskHere()), true, nil
}

func dstRootDelay(r *dstRoot) {
	if !dstDiskSlow.Load() {
		return
	}
	if lat := r.disk.latency.Load(); lat > 0 {
		time.Sleep(time.Duration(lat))
	}
}

func dstRootEnter(r *Root) (*dstRoot, error) {
	if err := r.root.incref(); err != nil {
		return nil, err
	}
	if r.root.dst.epoch != dstFSEpoch() {
		// A Root from a dead run: refused like a closed handle, the identity
		// a closed Root and a leaked *File both surface (the File's
		// ErrFileClosing is converted by wrapErr; the Root path has no such
		// converter, so ErrClosed is returned directly). See dstRoot.epoch.
		r.root.decref()
		return nil, ErrClosed
	}
	return r.root.dst, nil
}

func dstRootResolveLocked(r *dstRoot, name string) (parent *dstFSNode, base string, node *dstFSNode, trailingSlash bool, err error) {
	parts, endsInSlash, err := splitPathInRoot(name, nil, nil)
	if err != nil {
		return nil, "", nil, false, err
	}
	cur := r.node
	stack := []*dstFSNode{cur}
	names := []string{"."}
	terminalDir := func() (*dstFSNode, string, *dstFSNode, bool, error) {
		cur := stack[len(stack)-1]
		if len(stack) == 1 {
			return nil, ".", cur, endsInSlash, nil
		}
		return stack[len(stack)-2], names[len(names)-1], cur, endsInSlash, nil
	}
	for i, part := range parts {
		last := i == len(parts)-1
		switch part {
		case ".":
			if last {
				return terminalDir()
			}
			continue
		case "..":
			if len(stack) == 1 {
				return nil, "", nil, false, errPathEscapes
			}
			stack = stack[:len(stack)-1]
			names = names[:len(names)-1]
			cur = stack[len(stack)-1]
			if last {
				return terminalDir()
			}
			continue
		}
		if !cur.isDir {
			return nil, "", nil, false, syscall.ENOTDIR
		}
		child := cur.entries[part]
		if last {
			if endsInSlash && child != nil && !child.isDir {
				return nil, "", nil, false, syscall.ENOTDIR
			}
			return cur, part, child, endsInSlash, nil
		}
		if child == nil {
			return nil, "", nil, false, syscall.ENOENT
		}
		if !child.isDir {
			return nil, "", nil, false, syscall.ENOTDIR
		}
		cur = child
		stack = append(stack, cur)
		names = append(names, part)
	}
	return terminalDir()
}

func dstRootOpenRoot(root *Root, name string) (*Root, error) {
	r, err := dstRootEnter(root)
	if err != nil {
		return nil, &PathError{Op: "openat", Path: name, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	if dstRootProcAbsLocked(r, name) != "" {
		return nil, &PathError{Op: "openat", Path: name, Err: dstErrUnsupportedFS}
	}
	_, _, node, _, errno := dstRootResolveLocked(r, name)
	if errno != nil {
		return nil, &PathError{Op: "openat", Path: name, Err: errno}
	}
	if node == nil {
		return nil, &PathError{Op: "openat", Path: name, Err: syscall.ENOENT}
	}
	if !node.isDir {
		return nil, &PathError{Op: "openat", Path: name, Err: errors.New("not a directory")}
	}
	return dstNewRoot(joinPath(root.Name(), name), node, r.disk), nil
}

func dstRootOpenFile(root *Root, name string, flag int, perm FileMode) (*File, error) {
	r, err := dstRootEnter(root)
	if err != nil {
		return nil, &PathError{Op: "openat", Path: name, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	wrap := func(e error) (*File, error) { return nil, &PathError{Op: "openat", Path: name, Err: e} }
	if abs := dstRootProcAbsLocked(r, name); abs != "" {
		data, ident, base, _, errno := dstProcStatDataAbs(abs)
		if errno != nil {
			return wrap(errno)
		}
		if flag&(O_WRONLY|O_RDWR|O_CREATE|O_TRUNC) != 0 {
			return wrap(syscall.EACCES)
		}
		f := dstNewFile(&dstProcFile{data: data, name: joinPath(root.Name(), name), base: base, ident: ident}, joinPath(root.Name(), name))
		f.inRoot = true
		return f, nil
	}
	parent, base, node, trailingSlash, errno := dstRootResolveLocked(r, name)
	if errno != nil {
		return wrap(errno)
	}
	accWrite := flag&(O_WRONLY|O_RDWR) != 0
	if node != nil && node.isDir {
		// do_open's ordering — see dstOpenFile: O_CREAT|O_EXCL's EEXIST
		// precedes EISDIR and the write/O_TRUNC checks on an existing dir.
		if flag&(O_CREATE|O_EXCL) == O_CREATE|O_EXCL {
			return wrap(syscall.EEXIST)
		}
		if accWrite || flag&(O_TRUNC|O_CREATE) != 0 {
			return wrap(syscall.EISDIR)
		}
	}
	switch {
	case node == nil && flag&O_CREATE == 0:
		return wrap(syscall.ENOENT)
	case node != nil && flag&(O_CREATE|O_EXCL) == O_CREATE|O_EXCL:
		return wrap(syscall.EEXIST)
	case node == nil:
		if trailingSlash {
			// The ROOTED create through a trailing slash is ENOENT, not the
			// plain path's EISDIR: openat2(2) (os.Root's resolver) rejects
			// the slash-asserted missing final component before open(2)'s
			// EISDIR arm is reached (host-probed: Root.OpenFile("missing/",
			// O_CREATE|O_WRONLY) is "no such file or directory").
			return wrap(syscall.ENOENT)
		}
		if parent.unlinked {
			// Creation in an rmdir'd directory (addressed through the captured
			// Root node) is ENOENT, as openat(2) answers on the host.
			return wrap(syscall.ENOENT)
		}
		if r.disk.diskFullForCreate() {
			return wrap(syscall.ENOSPC)
		}
		node = dstFSNewNode(false, perm&dstFSModeMask)
		parent.entries[base] = node
		parent.modTime = time.Now()
	}
	if flag&O_TRUNC != 0 && !node.isDevice() {
		// Same rule as the plain-path open: Linux truncates even for
		// O_RDONLY|O_TRUNC, but on a character device the kernel ignores
		// O_TRUNC (handle_truncate runs for regular files only,
		// host-verified error-free) — and the device node has no pagecache
		// to truncate.
		if err := node.truncateLocked(0); err != nil {
			return wrap(err)
		}
	}
	osync, odsync := dstSyncModes(flag)
	d := &dstFile{
		node:   node,
		disk:   r.disk,
		path:   joinPath(root.Name(), name),
		rd:     flag&O_WRONLY == 0,
		wr:     accWrite,
		app:    flag&O_APPEND != 0,
		osync:  osync,
		odsync: odsync,
	}
	f := dstNewFile(d, d.path)
	f.inRoot = true
	return f, nil
}

func dstRootStat(root *Root, name string, lstat bool) (FileInfo, error) {
	r, err := dstRootEnter(root)
	if err != nil {
		return nil, &PathError{Op: "statat", Path: name, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	if abs := dstRootProcAbsLocked(r, name); abs != "" {
		_, ident, base, _, errno := dstProcStatDataAbs(abs)
		if errno != nil {
			return nil, &PathError{Op: "statat", Path: name, Err: errno}
		}
		return &dstFileInfo{name: base, mode: 0o444, ident: ident}, nil
	}
	_, base, node, _, errno := dstRootResolveLocked(r, name)
	if errno != nil {
		return nil, &PathError{Op: "statat", Path: name, Err: errno}
	}
	if node == nil {
		return nil, &PathError{Op: "statat", Path: name, Err: syscall.ENOENT}
	}
	if base == "." {
		base = path.Base(root.Name())
	}
	return &dstFileInfo{name: base, size: int64(len(node.data)), mode: node.mode, modTime: node.modTime, isDir: node.isDir, ident: node}, nil
}

func dstRootReadlink(root *Root, name string) (string, error) {
	r, err := dstRootEnter(root)
	if err != nil {
		return "", &PathError{Op: "readlinkat", Path: name, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	abs := dstRootProcAbsLocked(r, name)
	if abs == "" {
		return "", &PathError{Op: "readlinkat", Path: name, Err: dstErrUnsupportedFS}
	}
	target, errno := dstProcReadlinkAbs(abs)
	if errno != nil {
		return "", &PathError{Op: "readlinkat", Path: name, Err: errno}
	}
	return target, nil
}

func dstRootChmod(root *Root, name string, mode FileMode) error {
	r, err := dstRootEnter(root)
	if err != nil {
		return &PathError{Op: "chmodat", Path: name, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	if dstRootProcAbsLocked(r, name) != "" {
		return &PathError{Op: "chmodat", Path: name, Err: dstErrUnsupportedFS}
	}
	_, _, node, _, errno := dstRootResolveLocked(r, name)
	if errno != nil {
		return &PathError{Op: "chmodat", Path: name, Err: errno}
	}
	if node == nil {
		return &PathError{Op: "chmodat", Path: name, Err: syscall.ENOENT}
	}
	node.chmodLocked(mode)
	return nil
}

func dstRootChtimes(root *Root, name string, atime, mtime time.Time) error {
	r, err := dstRootEnter(root)
	if err != nil {
		return &PathError{Op: "chtimesat", Path: name, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	if dstRootProcAbsLocked(r, name) != "" {
		return &PathError{Op: "chtimesat", Path: name, Err: dstErrUnsupportedFS}
	}
	_, _, node, _, errno := dstRootResolveLocked(r, name)
	if errno != nil {
		return &PathError{Op: "chtimesat", Path: name, Err: errno}
	}
	if node == nil {
		return &PathError{Op: "chtimesat", Path: name, Err: syscall.ENOENT}
	}
	if !mtime.IsZero() {
		node.modTime = mtime
	}
	_ = atime
	return nil
}

func dstRootMkdir(root *Root, name string, perm FileMode) error {
	r, err := dstRootEnter(root)
	if err != nil {
		return &PathError{Op: "mkdirat", Path: name, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	if dstRootProcAbsLocked(r, name) != "" {
		return &PathError{Op: "mkdirat", Path: name, Err: dstErrUnsupportedFS}
	}
	// mkdirat(2) reports a positive final dentry EEXIST before the trailing
	// slash's directory assertion (filename_create looks the dentry up first;
	// host-probed: Root.Mkdir("file/") is EEXIST, not ENOTDIR), so mkdir
	// resolves with trailing slashes stripped and lets the existing-node
	// check answer.
	trimmed := name
	for len(trimmed) > 0 && trimmed[len(trimmed)-1] == '/' {
		trimmed = trimmed[:len(trimmed)-1]
	}
	if trimmed == "" {
		trimmed = name // all-slash names keep their original resolution
	}
	parent, base, node, _, errno := dstRootResolveLocked(r, trimmed)
	if errno != nil {
		return &PathError{Op: "mkdirat", Path: name, Err: errno}
	}
	if node != nil || parent == nil {
		return &PathError{Op: "mkdirat", Path: name, Err: syscall.EEXIST}
	}
	if parent.unlinked {
		return &PathError{Op: "mkdirat", Path: name, Err: syscall.ENOENT}
	}
	if r.disk.diskFullForCreate() {
		return &PathError{Op: "mkdirat", Path: name, Err: syscall.ENOSPC}
	}
	parent.entries[base] = dstFSNewNode(true, ModeDir|perm&dstFSModeMask)
	parent.modTime = time.Now()
	return nil
}

func dstRootMkdirAll(root *Root, name string, perm FileMode) error {
	r, err := dstRootEnter(root)
	if err != nil {
		return &PathError{Op: "mkdirat", Path: name, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	if dstRootProcAbsLocked(r, name) != "" || dstRootMkdirAllProcReservedLocked(r, name) {
		return &PathError{Op: "mkdirat", Path: name, Err: dstErrUnsupportedFS}
	}
	parts, _, err := splitPathInRoot(name, nil, nil)
	if err != nil {
		return &PathError{Op: "mkdirat", Path: name, Err: err}
	}
	cur := r.node
	stack := []*dstFSNode{cur}
	for _, part := range parts {
		switch part {
		case ".":
			continue
		case "..":
			if len(stack) == 1 {
				return &PathError{Op: "mkdirat", Path: name, Err: errPathEscapes}
			}
			stack = stack[:len(stack)-1]
			cur = stack[len(stack)-1]
			continue
		}
		if !cur.isDir {
			return &PathError{Op: "mkdirat", Path: name, Err: syscall.ENOTDIR}
		}
		child := cur.entries[part]
		if child == nil {
			if cur.unlinked {
				return &PathError{Op: "mkdirat", Path: name, Err: syscall.ENOENT}
			}
			if r.disk.diskFullForCreate() {
				return &PathError{Op: "mkdirat", Path: name, Err: syscall.ENOSPC}
			}
			child = dstFSNewNode(true, ModeDir|perm&dstFSModeMask)
			cur.entries[part] = child
			cur.modTime = time.Now()
		} else if !child.isDir {
			return &PathError{Op: "mkdirat", Path: name, Err: syscall.ENOTDIR}
		}
		cur = child
		stack = append(stack, cur)
	}
	return nil
}

func dstRootRemove(root *Root, name string) error {
	r, err := dstRootEnter(root)
	if err != nil {
		return &PathError{Op: "removeat", Path: name, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	if dstRootProcAbsLocked(r, name) != "" {
		return &PathError{Op: "removeat", Path: name, Err: dstErrUnsupportedFS}
	}
	parent, base, node, _, errno := dstRootResolveLocked(r, name)
	if errno != nil {
		return &PathError{Op: "removeat", Path: name, Err: errno}
	}
	if base == "." {
		return &PathError{Op: "removeat", Path: name, Err: syscall.EINVAL}
	}
	if node == nil {
		return &PathError{Op: "removeat", Path: name, Err: syscall.ENOENT}
	}
	if node.isDir && len(node.entries) > 0 {
		return &PathError{Op: "removeat", Path: name, Err: syscall.ENOTEMPTY}
	}
	dstFSDropLink(node)
	dstFSMarkUnlinked(node)
	delete(parent.entries, base)
	parent.modTime = time.Now()
	return nil
}

func dstRootRemoveAll(root *Root, name string) error {
	for len(name) > 0 && IsPathSeparator(name[len(name)-1]) {
		name = name[:len(name)-1]
	}
	if endsWithDot(name) {
		return &PathError{Op: "RemoveAll", Path: name, Err: syscall.EINVAL}
	}
	r, err := dstRootEnter(root)
	if err != nil {
		return &PathError{Op: "RemoveAll", Path: name, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	if dstRootProcAbsLocked(r, name) != "" {
		return &PathError{Op: "RemoveAll", Path: name, Err: dstErrUnsupportedFS}
	}
	parent, base, node, _, errno := dstRootResolveLocked(r, name)
	if errno != nil {
		if errno == syscall.ENOENT {
			return nil
		}
		return &PathError{Op: "RemoveAll", Path: name, Err: errno}
	}
	if node == nil {
		return nil
	}
	if parent == nil {
		return &PathError{Op: "RemoveAll", Path: name, Err: syscall.EBUSY}
	}
	dstFSDropLink(node)
	dstFSMarkUnlinked(node)
	delete(parent.entries, base)
	parent.modTime = time.Now()
	return nil
}

// dstRootRename mirrors the HOST os.Root.Rename surface (Go's own openat
// walk in rootRename, not raw renameat(2)) — host-probed on ext4 and tmpfs,
// which agree on every row. The ladder, in the host's order:
//
//  1. the OLD walk, including its terminal-slash assertion (a slash on a
//     missing old final is ENOENT, on a file ENOTDIR);
//  2. the NEW walk with rename's creating-directory slash rule (a slash on
//     a missing new final is legal, on a file ENOTDIR) — so a bad new walk
//     precedes even an old terminal "." EBUSY;
//  3. a slashed NEW final requires a directory SOURCE (missing old ENOENT,
//     file old ENOTDIR);
//  4. the portable preamble: an EXISTING-DIRECTORY newname is refused
//     EEXIST when the old name resolves (missing old reports ENOENT) —
//     so dir-over-dir replacement, dir-onto-nonempty-dir (raw rename(2)'s
//     ENOTEMPTY), dir-onto-self, and file-onto-dir (raw rename(2)'s
//     EISDIR) are all EEXIST through a Root;
//  5. renameat(2) itself: terminal "." EBUSY, missing old ENOENT,
//     same-node no-op, containment EINVAL, dir-over-file ENOTDIR.
func dstRootRename(root *Root, oldname, newname string) error {
	r, err := dstRootEnter(root)
	if err != nil {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	if dstRootProcAbsLocked(r, oldname) != "" || dstRootProcAbsLocked(r, newname) != "" {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: dstErrUnsupportedFS}
	}
	oldParent, oldBase, oldNode, oldTrailingSlash, errno := dstRootResolveLocked(r, oldname)
	if errno != nil {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: errno}
	}
	if oldTrailingSlash && oldNode == nil {
		// The old walk lstats a slashed final: a missing source surfaces
		// there, before the new path is looked at.
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.ENOENT}
	}
	newParent, newBase, newNode, newTrailingSlash, errno := dstRootResolveLocked(r, newname)
	if errno != nil {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: errno}
	}
	if newTrailingSlash {
		// A slashed new final admits a missing component (the creating-
		// directory rule) but requires a directory SOURCE.
		if oldNode == nil {
			return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.ENOENT}
		}
		if !oldNode.isDir {
			return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.ENOTDIR}
		}
	}
	if newNode != nil && newNode.isDir {
		// The preamble: an existing-directory newname is EEXIST whenever
		// the old name resolves — with the host's one escape: equal names
		// referring to the same file skip the preamble and renameat
		// answers. The host compares the two FINAL components (after its
		// ".." restart rewrite, before any "." collapse), so the same
		// directory node reached with a literal-dot final on exactly one
		// side has DIFFERING finals, escapes the preamble, and renameat
		// refuses the dot EBUSY (host-probed: Rename("de","de/.") and
		// Rename("de/.","de") are EBUSY while Rename("de","de") and
		// Rename("de/.","de/.") are EEXIST). Distinct directory nodes
		// never share a final-agnostic identity (no hardlinked dirs), so
		// the escape fires only on same-node-differing-final shapes.
		if oldNode == nil {
			return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.ENOENT}
		}
		if newNode != oldNode || dstRootRenameFinal(oldname, oldBase) == dstRootRenameFinal(newname, newBase) {
			return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.EEXIST}
		}
		// Same node, differing finals: fall through to renameat's ladder —
		// the terminal-dot arm below answers EBUSY (one side must carry
		// the dot for the finals to differ).
	}
	if oldBase == "." || dstRootFinalIsDot(oldname) || newBase == "." || dstRootFinalIsDot(newname) {
		// renameat(2)'s terminal-"." refusal. A new-side "." always names
		// an existing directory, so the preamble above answers it first;
		// the arm is kept for both sides as renameat itself has it.
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.EBUSY}
	}
	if oldNode == nil {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.ENOENT}
	}
	if newNode == oldNode {
		return nil
	}
	// newParent is never nil here: only a root-resolving newname yields a
	// nil parent, and the root is a directory the preamble above always
	// answers (EEXIST, ENOENT, or the same-node escape into the dot arm).
	if oldNode.isDir && dstNodeContains(oldNode, newParent) {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.EINVAL}
	}
	if newNode != nil && oldNode.isDir {
		// newNode is a file here (directory targets were EEXIST above).
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.ENOTDIR}
	}
	// (No unlinked check needed here: a rooted rename's SOURCE must resolve
	// inside this root, and an unlinked root is empty — the source lookup
	// already answered ENOENT.)
	if newNode != nil {
		dstFSDropLink(newNode)
		dstFSMarkUnlinked(newNode) // replaced target leaves the namespace (see dstRename)
	}
	delete(oldParent.entries, oldBase)
	newParent.entries[newBase] = oldNode
	now := time.Now()
	oldParent.modTime = now
	newParent.modTime = now
	return nil
}

// dstRootRenameFinal is the final path component as the host's rename
// preamble compares it: the resolver's base (which carries the host's ".."
// restart-rewrite collapse) except that a literal terminal "." — which the
// resolver collapses onto the named directory — stays ".".
func dstRootRenameFinal(name, base string) string {
	if dstRootFinalIsDot(name) {
		return "."
	}
	return base
}

// dstRootFinalIsDot reports whether the path's final component is a literal
// "." — renameat(2) refuses those EBUSY, and the resolver collapses them
// onto the named directory, so it alone cannot distinguish "de/." from "de".
// (A terminal ".." is NOT a dot final: the host's restart rewrite collapses
// it onto the parent name, exactly as the resolver does, and the rename
// legally targets that parent.) splitPathInRoot drops "." components except
// at the end, so checking the last part is exact.
func dstRootFinalIsDot(name string) bool {
	parts, _, err := splitPathInRoot(name, nil, nil)
	if err != nil || len(parts) == 0 {
		return false
	}
	return parts[len(parts)-1] == "."
}

func dstNodeContains(root, node *dstFSNode) bool {
	if root == node {
		return true
	}
	if !root.isDir {
		return false
	}
	for _, child := range root.entries {
		if dstNodeContains(child, node) {
			return true
		}
	}
	return false
}

// dstRootLink mirrors the HOST os.Root.Link surface (Go's openat walk
// into linkat(2)) — host-probed on tmpfs: the shared rows match plain
// link(2) — old walk-class errors first (missing old ENOENT even with
// an existing new, slashed file old ENOTDIR), a positive new EEXIST
// beating the dir-old EPERM, a slashed MISSING new ENOENT, EPERM last
// — with ONE rooted divergence: a slashed EXISTING regular-file new
// answers ENOTDIR (the rooted walk asserts the slash against the
// final's type), where plain link(2) answers EEXIST.
func dstRootLink(root *Root, oldname, newname string) error {
	r, err := dstRootEnter(root)
	if err != nil {
		return &LinkError{Op: "linkat", Old: oldname, New: newname, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	if dstRootProcAbsLocked(r, oldname) != "" || dstRootProcAbsLocked(r, newname) != "" {
		return &LinkError{Op: "linkat", Old: oldname, New: newname, Err: dstErrUnsupportedFS}
	}
	wrap := func(e error) error { return &LinkError{Op: "linkat", Old: oldname, New: newname, Err: e} }
	_, _, oldNode, _, errno := dstRootResolveLocked(r, oldname)
	if errno != nil {
		return wrap(errno)
	}
	if oldNode == nil {
		return wrap(syscall.ENOENT)
	}
	// Slashed-final type errors (a slashed regular-file old, the rooted
	// divergence where a slashed EXISTING file new answers ENOTDIR) are
	// the RESOLVER's: dstRootResolveLocked rejects a slashed non-dir
	// final before returning, so no arm here re-checks them — the ladder
	// rows in the Root-surface test pin them through that path.
	newParent, newBase, newNode, newTrailingSlash, errno := dstRootResolveLocked(r, newname)
	if errno != nil {
		return wrap(errno)
	}
	if newNode != nil {
		return wrap(syscall.EEXIST)
	}
	if newTrailingSlash {
		return wrap(syscall.ENOENT)
	}
	if oldNode.isDir {
		return wrap(syscall.EPERM)
	}
	newParent.entries[newBase] = oldNode
	oldNode.nlink++
	newParent.modTime = time.Now()
	return nil
}
