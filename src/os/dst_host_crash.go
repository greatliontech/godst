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
	dstCoarseRecord(dstCoarseNamespaceForHost(host), true)
	disk := dstFS.disks[host]
	if disk == nil {
		return // the host never touched its filesystem: nothing to restore
	}
	disk.down = true
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
	// Reset link and durable-parent counts across this host's nodes before the
	// durable walk rebuilds them from surviving syncedEntries edges, then
	// reclaim any regular file on this disk that no longer survives on the
	// platter (closeHostFilesFor already dropped openFiles and liveMaps).
	seenDirs := make(map[*dstFSNode]bool)
	var zeroDirs func(d *dstFSNode)
	zeroDirs = func(d *dstFSNode) {
		if d == nil || !d.isDir || seenDirs[d] {
			return
		}
		seenDirs[d] = true
		d.durableRefs = 0
		d.dirLinks = 0
		d.dentries = nil
		for _, c := range d.entries {
			zeroDirs(c)
		}
		for _, c := range d.syncedEntries {
			zeroDirs(c)
		}
	}
	zeroDirs(disk.root)
	disk.root.durableRefs = 1
	disk.root.dirLinks = 1
	disk.rootDentry = &dstFSDentry{node: disk.root, name: "/"}
	for _, n := range dstFS.backed {
		if n.disk == disk {
			n.nlink = 0
			n.durableRefs = 0
		}
	}
	dstRestoreNodeLocked(disk.root, make(map[*dstFSNode]bool), make(map[*dstFSNode]bool))
	for i := len(dstFS.backed) - 1; i >= 0; i-- {
		if n := dstFS.backed[i]; n.disk == disk {
			dstNodeMaybeReleaseLocked(n)
		}
	}
	// The working directories of the host's processes are kernel state
	// (per-process cwd lives in the task struct); a reboot starts every
	// process at the root of the restored tree.
	for key := range dstFS.cwds {
		if key[0] == host {
			delete(dstFS.cwds, key)
		}
	}
}

// dstBootHostDiskFor clears host's powered-off disk flag when the machine boots
// so subsequent CorruptFile calls resume platter-only masking beneath the new
// boot's page cache. Reached from testing/simulation.Host by //go:linkname.
//
//go:linkname dstBootHostDiskFor
func dstBootHostDiskFor(host uint32) {
	if !dstFSActive() {
		return
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	// Only clear down on an already-live disk of the current run; rolling dstFS
	// on a filesystem-free Host declaration would reset the runtime page-cache
	// region beneath white-box runtime mapping tests that never touch os.
	if dstFS.disks == nil || dstFS.epoch != dstFSEpoch() {
		return
	}
	if disk := dstFS.disks[host]; disk != nil {
		disk.down = false
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
func dstRestoreNodeLocked(node *dstFSNode, restored, ancestors map[*dstFSNode]bool) {
	if restored[node] {
		if !node.isDir {
			// A second durable parent edge: one more surviving hard link.
			node.nlink++
		}
		return // reachable from two parents: restore its image exactly once
	}
	restored[node] = true
	if !node.isDir {
		// The reboot's link count is the number of durable dirents that
		// reach the node — exactly the edges this walk traverses. Unsynced
		// links died with the page cache; unsynced removals resurrect.
		node.nlink = 1
	}
	node.mode = node.syncedMode
	node.modTime = node.syncedModTime
	node.unlinked = false
	// Advisory locks need no explicit clearing here: every flock is owned by a
	// virtual descriptor, and dstCloseHostFilesFor released the host's entire
	// descriptor table before the disk was restored — the locks went with it.
	if node.isDevice() {
		// A character device stores nothing (data, synced, and its page cache
		// are nil for its whole life): the metadata restore above is the whole
		// of its reboot, and the regular-file branch below would dereference
		// the page cache it does not have.
		return
	}
	if !node.isDir {
		// Rebuild the memfd sparsely from the durable (or torn) image:
		// truncating to 0 discards all unsynced pages in one ftruncate, and
		// resizing to image.size + copying only image.pages keeps holes sparse.
		image := node.synced
		if dstCrashTear {
			image = dstTearFileLocked(node)
		} else if node.rot != nil {
			pages := dstCloneImageWithRot(node.synced, node.synced.size, node.rot)
			image = dstDurableImage{
				size:   node.synced.size,
				pages:  pages,
				blocks: dstBlocksClampImage(node.synced.blocks, node.synced.size, pages),
			}
		}
		dstNodeSetSizeLocked(node, 0)
		if image.size > 0 {
			dstNodeSetSizeLocked(node, image.size)
			for pg, p := range image.pages {
				off := pg * dstPageSize
				end := min(off+dstPageSize, image.size)
				copy(node.data[off:end], p[:end-off])
			}
		}
		node.blocks = dstBlocksClampImage(image.blocks, image.size, image.pages)
		node.everMappedWrite.Store(false)
		// A crash clears the fsyncgate mark: the page cache is rebuilt from
		// the platter, so nothing is clean-but-stale afterwards.
		node.clearWritebackDroppedLocked()
		// The reboot folds rot into content: the corrupted bytes ARE what the
		// platter holds now — what recovery reads, and what a checksum must
		// catch.
		node.rot = nil
		// Re-base synced to the restored platter image so a second crash with
		// no intervening writes cannot re-tear against pre-crash bytes.
		node.synced = dstDurableImage{
			size:   image.size,
			pages:  image.pages,
			blocks: slices.Clone(node.blocks),
		}
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
	// Directory side of the same commit: the restored entry set is the
	// platter's — landed-but-unsynced entries a tear kept must not re-flip at
	// the next crash.
	node.syncedEntries = make(map[string]*dstFSNode, len(node.entries))
	for name, child := range node.entries {
		node.syncedEntries[name] = child
		dstNodeAddDurableRefLocked(child)
	}
	// Recurse in sorted name order: the children's own draws must come off the
	// fault RNG in a fixed order, never the map's (DST-FAULT-REPLAY).
	names := make([]string, 0, len(node.entries))
	for name := range node.entries {
		names = append(names, name)
	}
	slices.Sort(names)
	ancestors[node] = true
	defer delete(ancestors, node)
	for _, name := range names {
		child := node.entries[name]
		if child.isDir {
			if ancestors[child] {
				delete(node.entries, name)
				delete(node.syncedEntries, name)
				dstNodeDropDurableRefLocked(child)
				continue
			}
			child.dirLinks++
		}
		dstRestoreNodeLocked(child, restored, ancestors)
	}
}

// dstCloseHostFilesFor closes every simulated file opened on host's disk and
// releases the virtual descriptors naming them — the open file descriptions the
// dead kernel owned. Mappings are released separately (without write-back: the
// mapped bytes are page cache). Reached from testing/simulation by //go:linkname.
//
//go:linkname dstCloseHostFilesFor
func dstCloseHostFilesFor(host uint32) {
	dstFutexTeardownHost(host)
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
