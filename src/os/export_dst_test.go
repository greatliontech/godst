// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import "unsafe"

// DSTFSNodeState is the white-box inspector for the durability-monotonicity
// invariant: it reports a simulated node's current vs durable state so tests
// can assert that mutations never advance the durable image and that sync
// alone does. Test-only; the fault feature's crash restoration is the
// eventual production reader of the durable image.
func DSTFSNodeState(name string) (cur, synced string, curEntries, syncedEntries []string, mode, syncedMode FileMode, ok bool) {
	cur, synced, curEntries, syncedEntries, mode, syncedMode, _, _, ok = dstFSNodeStateFull(name)
	return
}

// DSTFSNodeTimes exposes the modTime pair for the metadata-monotonicity pins.
func DSTFSNodeTimes(name string) (modTime, syncedModTime int64, ok bool) {
	_, _, _, _, _, _, mt, smt, ok := dstFSNodeStateFull(name)
	return mt, smt, ok
}

func dstFSNodeStateFull(name string) (cur, synced string, curEntries, syncedEntries []string, mode, syncedMode FileMode, modTime, syncedModTime int64, ok bool) {
	if !dstFSActive() {
		return "", "", nil, nil, 0, 0, 0, 0, false
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	_, _, node, errno := dstFSResolve(name)
	if errno != nil || node == nil {
		return "", "", nil, nil, 0, 0, 0, 0, false
	}
	sorted := func(m map[string]*dstFSNode) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		for i := 1; i < len(out); i++ {
			for j := i; j > 0 && out[j] < out[j-1]; j-- {
				out[j], out[j-1] = out[j-1], out[j]
			}
		}
		return out
	}
	return string(node.data), string(node.synced.bytes()),
		sorted(node.entries), sorted(node.syncedEntries),
		node.mode, node.syncedMode,
		node.modTime.UnixNano(), node.syncedModTime.UnixNano(), true
}

// DSTFSNodeSparseStats reports a regular file's logical and durable sizes,
// allocated 4 KB block count, and durable non-zero 4 KB page count without
// densifying sparse holes.
func DSTFSNodeSparseStats(name string) (size, syncedSize, allocatedPages, syncedNonZeroPages int64, ok bool) {
	if !dstFSActive() {
		return 0, 0, 0, 0, false
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	_, _, node, errno := dstFSResolve(name)
	if errno != nil || node == nil || node.isDir || node.isDevice() {
		return 0, 0, 0, 0, false
	}
	return int64(len(node.data)), node.synced.size, node.allocatedPagesLocked(), int64(len(node.synced.pages)), true
}

// DSTFSNodeDurablePagePtr returns the identity of pg's immutable durable page
// buffer (0 if pg is a hole) so white-box tests can pin COW page sharing.
func DSTFSNodeDurablePagePtr(name string, pg int64) (uintptr, bool) {
	if !dstFSActive() {
		return 0, false
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	_, _, node, errno := dstFSResolve(name)
	if errno != nil || node == nil || node.isDir || node.isDevice() {
		return 0, false
	}
	p := node.synced.pages[pg]
	if p == nil {
		return 0, true
	}
	return uintptr(unsafe.Pointer(p)), true
}

// DSTBackedNodeCount reports the number of regular file nodes currently
// holding a live memfd and internal page-cache view in dstFS.backed.
func DSTBackedNodeCount() int {
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	return len(dstFS.backed)
}

// DSTRawRename drives the internal renameat(2) ladder (dstRename) directly —
// the kernel semantics beneath both portable preambles. The os.Rename and
// os.Root.Rename surfaces refuse every existing-directory newname (EEXIST)
// before reaching it, so the ladder's dir-over-empty-dir replacement arm is
// surface-unreachable; tests use this hook to pin the internal arm (a
// replaced directory node leaves the namespace like a Remove).
func DSTRawRename(oldname, newname string) error {
	handled, err := dstRename(oldname, newname)
	if !handled {
		return ErrInvalid
	}
	return err
}

// DSTSetRootCloseObserver installs (or clears, with nil) the teardown-order
// observer for dstCloseRoots: fn receives each victim Root's captured name in
// close order. White-box pin support for the registration-order teardown.
func DSTSetRootCloseObserver(fn func(name string)) {
	if fn == nil {
		dstRootCloseHook = nil
		return
	}
	dstRootCloseHook = func(r *root) { fn(r.name) }
}

// DSTSMaxBytes exposes the modeled s_maxbytes for the white-box tests.
func DSTSMaxBytes() int64 { return dstSMaxBytes }
