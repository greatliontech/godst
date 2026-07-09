// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && (unix || wasip1)

package os

import (
	"errors"
	"path"
	"runtime"
	"syscall"
	"time"
)

type dstRoot struct {
	node *dstFSNode
	disk *dstFSDisk
}

func dstRootActive(r *Root) bool {
	return r != nil && r.root != nil && r.root.dst != nil
}

func dstNewRoot(name string, node *dstFSNode, disk *dstFSDisk) *Root {
	r := &Root{&root{fd: -1, name: name, dst: &dstRoot{node: node, disk: disk}}}
	runtime.SetFinalizer(r.root, (*root).Close)
	return r
}

func dstOpenRoot(name string) (*Root, bool, error) {
	if !dstFSActive() {
		return nil, false, nil
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
	parent, base, node, trailingSlash, errno := dstRootResolveLocked(r, name)
	if errno != nil {
		return wrap(errno)
	}
	accWrite := flag&(O_WRONLY|O_RDWR) != 0
	if node != nil && node.isDir && (accWrite || flag&O_TRUNC != 0) {
		return wrap(syscall.EISDIR)
	}
	switch {
	case node == nil && flag&O_CREATE == 0:
		return wrap(syscall.ENOENT)
	case node != nil && flag&(O_CREATE|O_EXCL) == O_CREATE|O_EXCL:
		return wrap(syscall.EEXIST)
	case node == nil:
		if trailingSlash {
			return wrap(syscall.EISDIR)
		}
		if parent == nil {
			return wrap(syscall.EISDIR)
		}
		if r.disk.diskFullForCreate() {
			return wrap(syscall.ENOSPC)
		}
		node = &dstFSNode{mode: perm & ModePerm, modTime: time.Now()}
		parent.entries[base] = node
		parent.modTime = time.Now()
	}
	if flag&O_TRUNC != 0 {
		node.truncateLocked(0)
	}
	d := &dstFile{
		node:  node,
		disk:  r.disk,
		path:  joinPath(root.Name(), name),
		rd:    flag&O_WRONLY == 0,
		wr:    accWrite,
		app:   flag&O_APPEND != 0,
		osync: flag&O_SYNC != 0,
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

func dstRootChmod(root *Root, name string, mode FileMode) error {
	r, err := dstRootEnter(root)
	if err != nil {
		return &PathError{Op: "chmodat", Path: name, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
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
	parent, base, node, _, errno := dstRootResolveLocked(r, name)
	if errno != nil {
		return &PathError{Op: "mkdirat", Path: name, Err: errno}
	}
	if node != nil || parent == nil {
		return &PathError{Op: "mkdirat", Path: name, Err: syscall.EEXIST}
	}
	if r.disk.diskFullForCreate() {
		return &PathError{Op: "mkdirat", Path: name, Err: syscall.ENOSPC}
	}
	parent.entries[base] = &dstFSNode{isDir: true, entries: make(map[string]*dstFSNode), mode: ModeDir | perm&ModePerm, modTime: time.Now()}
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
			if r.disk.diskFullForCreate() {
				return &PathError{Op: "mkdirat", Path: name, Err: syscall.ENOSPC}
			}
			child = &dstFSNode{isDir: true, entries: make(map[string]*dstFSNode), mode: ModeDir | perm&ModePerm, modTime: time.Now()}
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
	delete(parent.entries, base)
	parent.modTime = time.Now()
	return nil
}

func dstRootRename(root *Root, oldname, newname string) error {
	r, err := dstRootEnter(root)
	if err != nil {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: err}
	}
	defer root.root.decref()
	dstRootDelay(r)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	oldParent, oldBase, oldNode, _, errno := dstRootResolveLocked(r, oldname)
	if errno != nil {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: errno}
	}
	if oldBase == "." {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.EBUSY}
	}
	if oldNode == nil {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.ENOENT}
	}
	newParent, newBase, newNode, newTrailingSlash, errno := dstRootResolveLocked(r, newname)
	if errno != nil {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: errno}
	}
	if newBase == "." || newParent == nil {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.EBUSY}
	}
	if newTrailingSlash && newNode == nil {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.ENOTDIR}
	}
	if newNode == oldNode {
		return nil
	}
	if oldNode.isDir && dstNodeContains(oldNode, newParent) {
		return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.EINVAL}
	}
	if newNode != nil {
		switch {
		case newNode.isDir && !oldNode.isDir:
			return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.EISDIR}
		case !newNode.isDir && oldNode.isDir:
			return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.ENOTDIR}
		case newNode.isDir && len(newNode.entries) > 0:
			return &LinkError{Op: "renameat", Old: oldname, New: newname, Err: syscall.ENOTEMPTY}
		}
	}
	delete(oldParent.entries, oldBase)
	newParent.entries[newBase] = oldNode
	now := time.Now()
	oldParent.modTime = now
	newParent.modTime = now
	return nil
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
