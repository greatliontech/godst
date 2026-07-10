// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import (
	"slices"
	_ "unsafe" // for go:linkname
)

// Host-crash teardown: power loss / kernel panic. Everything the KERNEL owned
// dies — the page cache (so unsynced writes and unsynced directory entries are
// gone), open file descriptions, and the advisory lock table — while the DISK
// keeps exactly what was committed to it. That split is why the durability
// representation carries a per-node durable image (synced / syncedEntries /
// syncedMode / syncedModTime) from day one: a host crash is the policy that
// restores it, and a process crash (whose kernel survives) is the policy that
// does not.
//
// Everything here is keyed by HOST, never by process: a Host body with no
// Process declaration runs as the root process (proc 0), which is shared across
// hosts, so proc-keyed teardown would reach into a sibling host's state. The
// process-keyed half (goroutine death, pid liveness, conns) is the caller's.

// dstRestoreHostDiskFor tears host's filesystem back to its durable image, in
// place, so handles the crash is about to close cannot observe an intermediate
// tree. Reached from testing/simulation.CrashHost by //go:linkname.
//
//go:linkname dstRestoreHostDiskFor
func dstRestoreHostDiskFor(host uint32) {
	if !dstFSActive() {
		return
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	disk := dstFS.disks[host]
	if disk == nil {
		return // the host never touched its filesystem: nothing to restore
	}
	// One inode can be reachable from two parents after a rename whose parent
	// directories were never fsynced (each link independently lands), so the walk
	// tracks what it has restored: the restore mutates nodes in place, and a
	// second visit would read already-restored bytes as if they were the page
	// cache's and tear them again — a second draw off the fault RNG for one
	// inode. The doubly-torn image is still made of durable and written bytes
	// (both links read the same node, so it stays self-consistent), which is why
	// no test can distinguish it: the guard buys one draw per inode, not
	// soundness. TestDSTCrashTearRenameDoubleLinkRestoredOnce pins that the
	// two links agree and that every byte is durable-or-written.
	dstRestoreNodeLocked(disk.root, make(map[*dstFSNode]bool))
	// The working directories of the host's processes are kernel state
	// (per-process cwd lives in the task struct); a reboot starts every
	// process at the root of the restored tree.
	for key := range dstFS.cwds {
		if key[0] == host {
			delete(dstFS.cwds, key)
		}
	}
}

// dstRestoreNodeLocked restores one node and, for a directory, exactly the
// subtree its durable entry set reaches. Caller holds dstFS.mu.
//
// A node that its parent never durably linked is simply not reached — it
// vanishes with the page cache. A node the parent still durably links but that
// was UNLINKED in the live tree comes back: the removal itself was never made
// durable (no parent fsync), so after a power loss the directory entry is still
// on the disk. Its unlinked mark and its emptied entry map must both be undone,
// or the resurrected node would be a directory that exists but can never gain
// an entry (the mark makes creation ENOENT) — a state no kernel produces.
func dstRestoreNodeLocked(node *dstFSNode, restored map[*dstFSNode]bool) {
	if restored[node] {
		return // reachable from two parents: restore its image exactly once
	}
	restored[node] = true
	node.mode = node.syncedMode
	node.modTime = node.syncedModTime
	node.unlinked = false
	// Advisory locks need no explicit clearing here: every flock is owned by a
	// virtual descriptor, and dstCloseHostFilesFor released the host's entire
	// descriptor table before the disk was restored — the locks went with it.
	if !node.isDir {
		// In place, never by reassignment: node.data aliases the node's page
		// cache, and the rewind must land where any still-registered mapping
		// of a dead process's would have looked (they are unmapped by then,
		// but the bytes' home does not move).
		image := node.synced
		if dstCrashTear {
			image = dstTearFileLocked(node.synced, node.data)
		}
		dstNodeSetSizeLocked(node, int64(len(image)))
		copy(node.data, image)
		return
	}
	if dstCrashTear {
		node.entries = dstTearEntriesLocked(node)
	} else {
		node.entries = make(map[string]*dstFSNode, len(node.syncedEntries))
		for name, child := range node.syncedEntries {
			node.entries[name] = child
		}
	}
	// Recurse in sorted name order: the children's own draws must come off the
	// fault RNG in a fixed order, never the map's (DST-FAULT-REPLAY).
	names := make([]string, 0, len(node.entries))
	for name := range node.entries {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		dstRestoreNodeLocked(node.entries[name], restored)
	}
}

// dstCloseHostFilesFor closes every simulated file opened on host's disk and
// releases the virtual descriptors naming them — the open file descriptions the
// dead kernel owned. Mappings are released separately (without write-back: the
// mapped bytes are page cache). Reached from testing/simulation by //go:linkname.
//
//go:linkname dstCloseHostFilesFor
func dstCloseHostFilesFor(host uint32) {
	if !dstFSActive() {
		return
	}
	// Mappings first: they must be dropped, never written back (see
	// dstMMapReleaseHost), before anything else can observe their bytes.
	dstMMapReleaseHost(host)
	// Closing the host's files releases the virtual descriptors naming them and
	// the flocks those descriptors own (dstReleaseFD walks every fd of the
	// file, across processes), so no separate descriptor-table sweep is needed:
	// a descriptor on this host always names a file on this host.
	dstCloseHostFiles(host)
}
