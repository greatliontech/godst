// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import (
	"cmp"
	"internal/filepathlite"
	"internal/poll"
	isync "internal/sync"
	"io"
	"path"
	"slices"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe" // go:linkname + coarse-dependency node identities
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

// dstFS is the process-global simulated filesystem. The tree is per HOST: each
// host (testing/simulation.Host) owns its own root, so co-located processes share a
// filesystem while different hosts are isolated — process A on host hA cannot
// observe or mutate host hB's files except over the simulated network
// (DST-NODE-ISOLATION). The working directory is per PROCESS: a path into the
// process's host tree, so one process's Chdir does not move another's even on the
// same host. Both maps are keyed by the run epoch (reset in dstFSRoll) so a new run
// starts fresh even without a teardown; the run's HOST residue (page-cache memfds
// and the mapping region) is additionally released eagerly at run end
// (dstFSRunTeardown). The default host/process (id 0 — a program
// that declares neither) is one host, one process, identical to a plain run. All
// node state is guarded by mu; per-handle state by the handle's own mutex, acquired
// before mu where both are needed.
var dstFS struct {
	mu      isync.KernelMutex
	epoch   uint64
	disks   map[uint32]*dstFSDisk      // host id -> the host's in-memory tree
	cwds    map[[2]uint32]*dstFSDentry // (host id, process id) -> that process's working directory dentry
	nextIno uint64                     // last synthetic inode number handed out (dstFSAllocIno)
	// backed lists every regular node of the run — each is born with a page
	// cache — so the run's end can close their descriptors and take back the
	// mapping region in one stroke (dstNodeReleaseRunLocked).
	backed []*dstFSNode
}

// dstFSModeMask is the modeled metadata mode-bit set: the permission bits
// plus the bits Chmod can set (setuid/setgid/sticky). Which bits a given
// CREATE surface accepts is upstream's contract, enforced at the public API
// before any backend dispatch (Root's create operations accept 0o777 only);
// this mask is what the tree stores and Chmod changes; dstFSNewNode
// applies it, so no creation path can differ.
const dstFSModeMask = ModePerm | ModeSetuid | ModeSetgid | ModeSticky

var dstOpenFiles struct {
	mu    isync.KernelMutex
	epoch uint64
	files map[*file]dstOpenFileEntry
	seq   uint64 // per-run registration counter (see dstOpenFileEntry.seq)
}

type dstOpenFileEntry struct {
	// host and proc attribute the open file description to its owning machine
	// and process. A process crash closes a PROCESS's files; a host crash
	// closes a HOST's — different sets, because a Host body with no Process
	// declaration runs as the root process (proc 0), which every host shares.
	// Keying host teardown by proc would close a sibling host's files
	// (DST-FAULT-VICTIM).
	host uint32
	proc uint32
	// seq stamps each registration in open order — a pure function of the
	// schedule — so teardown closes a victim's files in that order, never the
	// pointer-keyed map's iteration order (run-varying even under the fixed
	// -tags dst hash key: addresses aren't reproducible). Close order is
	// observable: each close's flock release can wake blocked waiters, so a
	// varying order would fork the schedule (DST-FAULT-REPLAY).
	seq uint64
}

func dstOpenFilesRollLocked() {
	if e := dstFSEpoch(); e != dstOpenFiles.epoch || dstOpenFiles.files == nil {
		dstOpenFiles.epoch = e
		dstOpenFiles.files = make(map[*file]dstOpenFileEntry)
		dstOpenFiles.seq = 0
	}
}

func dstRegisterOpenFile(f *file, host, proc uint32) {
	dstOpenFiles.mu.Lock()
	dstOpenFilesRollLocked()
	dstOpenFiles.seq++
	dstOpenFiles.files[f] = dstOpenFileEntry{host: host, proc: proc, seq: dstOpenFiles.seq}
	dstOpenFiles.mu.Unlock()
}

func dstUnregisterOpenFile(f *file) {
	dstOpenFiles.mu.Lock()
	dstOpenFilesRollLocked()
	delete(dstOpenFiles.files, f)
	dstOpenFiles.mu.Unlock()
}

func dstHasOpenProcFiles(proc uint32) bool {
	dstOpenFiles.mu.Lock()
	defer dstOpenFiles.mu.Unlock()
	dstOpenFilesRollLocked()
	for _, entry := range dstOpenFiles.files {
		if entry.proc == proc {
			return true
		}
	}
	return false
}

// dstCloseProcFiles closes the files a PROCESS opened (process crash, or exit).
func dstCloseProcFiles(proc uint32) {
	dstKernelSectionEnter()
	defer dstKernelSectionExit()
	dstCloseOpenFiles(func(e dstOpenFileEntry) bool { return e.proc == proc })
	dstCloseProcRoots(proc)
}

// dstCloseHostFiles closes every file opened on a HOST, whichever process
// opened it (host crash: the kernel that held the open file descriptions is
// gone). See dstOpenFileEntry for why this cannot key on proc.
func dstCloseHostFiles(host uint32) {
	dstKernelSectionEnter()
	defer dstKernelSectionExit()
	dstCloseOpenFiles(func(e dstOpenFileEntry) bool { return e.host == host })
	dstCloseHostRoots(host)
}

func dstCloseOpenFiles(match func(dstOpenFileEntry) bool) {
	dstOpenFiles.mu.Lock()
	dstOpenFilesRollLocked()
	type victim struct {
		f   *file
		seq uint64
	}
	var victims []victim
	for f, entry := range dstOpenFiles.files {
		if match(entry) {
			victims = append(victims, victim{f: f, seq: entry.seq})
			delete(dstOpenFiles.files, f)
		}
	}
	dstOpenFiles.mu.Unlock()
	// Registration (open) order, never the pointer-keyed map's iteration order
	// — see dstOpenFileEntry.seq.
	slices.SortFunc(victims, func(a, b victim) int { return cmp.Compare(a.seq, b.seq) })
	files := make([]*file, len(victims))
	for i, v := range victims {
		files[i] = v.f
	}
	for _, f := range files {
		dstf := dstBackendOf(f)
		if dstf == nil {
			continue
		}
		_ = dstf.closeFile()
		dstReleaseFD(f)
		dstDropClosedNode(dstf)
	}
}

// dstFSDisk is one host's tree (its filesystem). The durability state lives on the
// nodes; a host's disk is what a host (power-loss) crash later restores.
//
// Disk faults are policy on the disk itself (one source of truth, reset with the
// disk when the run epoch rolls — no separate teardown): eio fails the whole disk's
// I/O with EIO, and eioFiles fails just the listed nodes (a bad sector on one
// file); wbFail fails only writeback — every sync EIOs (data syncs dropping their
// dirty pages, the fsyncgate model) while reads and writes keep succeeding, since
// the page cache serves them without touching the failing medium (and this model
// never evicts). A node, not a path, is the per-file key, so a faulted file stays faulted
// across a rename and a removed-but-open handle keeps failing — the physical
// bad-block semantics. capped/capacity model a full disk: writes or fallocate
// calls whose newly allocated 4 KB blocks would exceed capacity/4096, and creates
// on an already-full disk, fail with ENOSPC. The space in use is summed on demand
// from the live tree (residentPagesLocked), not tracked incrementally, so a delete
// or truncate-down frees blocks for the next write with no accounting threaded
// through every mutation — and never a false ENOSPC a budget-that-ignores-frees
// would produce. latency models a slow disk: a per-op delay (nanoseconds) the
// calling goroutine sleeps before each disk-touching op. It
// is atomic, not under dstFS.mu, because the delay is read and slept on BEFORE the op
// takes the tree lock — sleeping while holding dstFS.mu would freeze every host's
// filesystem, not just the slow one. (eio/capacity are read inside the op under the
// lock; only latency needs the lock-free read.) Set through the runtime relay (see
// dst_disk_fault.go).
type dstFSDisk struct {
	root       *dstFSNode
	rootDentry *dstFSDentry
	down       bool                // powered-off state between CrashHost and the next Host boot
	eio        bool                // host-disk EIO: every read/write/sync on this disk fails EIO
	eioFiles   map[*dstFSNode]bool // per-file EIO: just these nodes fail (a bad sector)
	wbFail     bool                // writeback EIO: syncs fail (data syncs drop dirty pages); cache-served reads/writes succeed
	capped     atomic.Bool         // whether a capacity (full-disk / ENOSPC) limit is set
	capacity   int64               // max total allocated regular-file bytes (capacity/4096 4 KB blocks) when capped
	latency    atomic.Int64        // slow-disk: per-op delay in nanoseconds (0 = none)
}

// dstDiskSlow gates the per-op latency delay: true once any host's disk has a latency
// set this run, so the no-fault path is a single atomic load with no lock. Reset when
// the run epoch rolls (dstFSRoll).
var dstDiskSlow atomic.Bool

//go:linkname dstFSCurrentNode runtime.dstCurrentSlot
func dstFSCurrentNode() (host, proc uint32)

//go:linkname dstRecordCausalEvent runtime.dstRecordCausalEvent
func dstRecordCausalEvent(kind, flags uint8, hostA, hostB, proc uint32, pid int32, arg0, arg1, arg2 int64, str string)

//go:linkname dstRecordCausalCaller runtime.dstRecordCausalCaller
func dstRecordCausalCaller(kind, flags uint8, hostB uint32, arg0, arg1, arg2 int64, str string)

// Causal event kind codes and FS op codes — must match runtime and testing/simulation.
const (
	dstCausalCorruptFile     uint8 = 31
	dstCausalFSEIO           uint8 = 50
	dstCausalFSFsyncgateDrop uint8 = 51
	dstCausalFSENOSPC        uint8 = 52
)

const (
	dstFSOpRead      uint8 = 1
	dstFSOpPread     uint8 = 2
	dstFSOpWrite     uint8 = 3
	dstFSOpPwrite    uint8 = 4
	dstFSOpFsync     uint8 = 5
	dstFSOpFdatasync uint8 = 6
	dstFSOpFallocate uint8 = 7
	dstFSOpMmap      uint8 = 8
	dstFSOpCreate    uint8 = 9
	dstFSOpMkdir     uint8 = 10
)

//go:linkname dstSMaxBytesOS runtime.dstSMaxBytesOS
func dstSMaxBytesOS() int64

// dstSMaxBytes is the modeled s_maxbytes: no file's size may pass it, and
// every size-growth site — truncate, fallocate, the write clip — answers
// EFBIG at it, as the vfs does. Before it existed, huge growth ran into the
// page-cache mapping reserve and died with a runtime fatal no real kernel
// produces. On arches whose mapping region is off the runtime reports 0,
// which means no modeled bound — 1<<62 keeps every check site unconditional
// while refusing nothing a 32-bit address space could represent anyway.
var dstSMaxBytes = func() int64 {
	if v := dstSMaxBytesOS(); v > 0 {
		return v
	}
	return 1 << 62
}()

// dstFSRoll resets the per-run filesystem state when the run epoch advances. Caller
// holds dstFS.mu. Per-host trees are created lazily (dstFSDiskHere), so Roll only
// clears the maps.
func dstFSRoll() {
	if e := dstFSEpoch(); e != dstFS.epoch || dstFS.disks == nil {
		// The run that is ending owns page caches and their mappings. Drop them
		// before the nodes go: a mapping outliving its run would leave the
		// runtime's fault registry claiming addresses no simulated file owns.
		dstNodeReleaseRunLocked()
		dstFS.epoch = e
		dstFS.disks = make(map[uint32]*dstFSDisk)
		dstFS.cwds = make(map[[2]uint32]*dstFSDentry)
		dstFS.nextIno = 1
		// New run: no host is slow until a SlowDisk fault re-arms the gate (the
		// per-disk latency resets with the disks above).
		dstDiskSlow.Store(false)
	}
}

// dstFSRunTeardown releases a completed run's filesystem residue eagerly:
// every page cache's host memfd closes and the mapping region is taken back
// when the run ends, not at the next run's first filesystem operation. A run
// must leave the host descriptor table as it found it — the lazy roll alone
// left the last run's memfds open for the rest of the process (or until
// another run's first filesystem op), a host-isolation leak the fd census in
// TestDSTPipeBasic pins. Clearing the maps to nil (rather than to fresh maps)
// makes dstFSRoll re-initialize on the next run's first operation exactly as
// it does on an epoch advance.
//
// Called by testing/simulation at run teardown, after the bubble has exited
// (no simulated operation is in flight; a leaked handle's later use is fenced
// by the epoch checks, and a leaked mapping's later touch faults into the
// reset region's named-abort path, both as the leak contract records).
//
//go:linkname dstFSRunTeardown
func dstFSRunTeardown() {
	// The one sweep point for the out-of-line state tables: host
	// context, bubble dead, so weak.Value's potential GC park is an
	// ordinary wait (dst_filestate.go). Before the early return —
	// rows from a filesystem-free run's pipes still deserve the sweep.
	dstStateTablesSweep()
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstNodeReleaseRunLocked()
	if dstFS.disks == nil {
		return // the run made no filesystem use, or was already torn down
	}
	dstFS.disks = nil
	dstFS.cwds = nil
	dstDiskSlow.Store(false)
}

// dstFSNewNode allocates a node with its inode number and its metadata durable
// image already at the birth values. Metadata durability is an INODE property,
// and an inode reaches the disk with the mode and timestamp it was created
// with: once the parent directory's fsync makes the NAME durable, a crash
// recovers a file with its creation mode — not mode 0. (Later chmod/utimes
// still move only current state; the image advances on the node's own sync,
// per the durability contract's monotonicity invariant.) Content and entry
// images stay empty: no bytes and no children were ever committed.
//
// Every node-creation site goes through here, so the rule cannot be forgotten
// at one of them. typ carries the type bits a call site chooses (ModeDir,
// device bits; only ModeType bits survive), perm the caller-supplied mode,
// of which only dstFSModeMask survives — the split means a user perm cannot
// type a node, a call site cannot smuggle metadata through typ, and no
// creation path can skip the mask. Caller holds dstFS.mu.
func dstFSNewNode(disk *dstFSDisk, typ, perm FileMode) *dstFSNode {
	now := time.Now()
	mode := typ&ModeType | perm&dstFSModeMask
	node := &dstFSNode{
		isDir:         typ&ModeDir != 0,
		disk:          disk,
		nlink:         1,
		ino:           dstFSAllocIno(),
		mode:          mode,
		modTime:       now,
		syncedMode:    mode,
		syncedModTime: now,
	}
	switch {
	case node.isDir:
		node.entries = make(map[string]*dstFSNode)
	case node.isDevice():
		// A character device has no page cache: it stores nothing (writes
		// discard, reads are EOF), so there are no bytes to back or map.
	default:
		// A regular file's bytes live in its page cache from birth (see
		// dst_pagecache_linux.go); both create paths hold dstFS.mu.
		dstNodeBackLocked(node)
	}
	return node
}

// dstFSAllocIno returns the next synthetic inode number — a per-run monotonic
// counter shared by every host disk (st_dev separates hosts, so cross-host
// uniqueness is free and harmless). Allocation order rides the schedule, so
// inode numbers are a deterministic function of the seed. Caller holds dstFS.mu.
func dstFSAllocIno() uint64 {
	dstFS.nextIno++
	return dstFS.nextIno
}

// dstFSDiskHere returns the calling goroutine's host disk, creating it (with the
// pre-seeded /tmp) on first touch. Caller holds dstFS.mu.
func dstFSDiskHere() *dstFSDisk {
	host, _ := dstFSCurrentNode()
	d := dstFS.disks[host]
	if d == nil {
		d = newDstFSDisk()
		dstFS.disks[host] = d
	}
	return d
}

// newDstFSDisk builds a fresh host tree: an empty root containing /tmp (mode
// 1777), so TempDir-based CreateTemp/MkdirTemp work unmodified, and /dev with
// the null character device (the spec's initial-tree clause). Every host gets
// its own /tmp and /dev/null; os.TempDir reports the fixed path "/tmp" during
// a run.
func newDstFSDisk() *dstFSDisk {
	disk := &dstFSDisk{}
	root := dstFSNewNode(disk, ModeDir, 0o755)
	root.durableRefs = 1 // the root inode is permanently anchored on the platter
	root.dirLinks = 1
	root.attachEntryLocked("tmp", dstFSNewNode(disk, ModeDir, ModeSticky|0o777))
	dev := dstFSNewNode(disk, ModeDir, 0o755)
	dev.attachEntryLocked("null", dstFSNewNode(disk, ModeDevice|ModeCharDevice, 0o666))
	root.attachEntryLocked("dev", dev)
	disk.root = root
	disk.rootDentry = &dstFSDentry{node: root, name: "/"}
	// The mkfs image is part of the durable image: a filesystem is born with
	// its initial tree ON THE PLATTER, so a host crash preserves root, /tmp,
	// and /dev/null. Without this commit the tree is born unsynced and the
	// first crash erases "tmp" — unrecoverable short of fsyncing "/", which no
	// POSIX-disciplined program does for a pre-existing directory, so
	// fsync-disciplined state under the one directory the spec guarantees
	// exists would vanish (a false-positive generator for every
	// crash-recovery SUT) — and "dev", whose loss would make post-reboot
	// /dev/null opens fail with an ENOENT no real reboot produces.
	root.commitLocked()
	root.entries["tmp"].commitLocked()
	dev.commitLocked()
	dev.entries["null"].commitLocked()
	return disk
}

// dstFSDentry is a directory path edge (Linux struct dentry) binding a parent
// dentry and entry name to a directory inode. Separating dentries from inodes
// preserves per-alias parent chains (".." and Getwd) and per-alias unlink
// survival when crash recovery retains both old and new aliases of a renamed
// directory (design.md, durability contract).
type dstFSDentry struct {
	node     *dstFSNode
	parent   *dstFSDentry            // nil on disk.rootDentry; retained across unlink for ".."
	name     string                  // "/" on disk.rootDentry
	children map[string]*dstFSDentry // live child directory dentries
	unlinked bool                    // true once this specific alias edge is unlinked
}

// dstCloneKernelString copies a caller-supplied string into kernel-owned
// backing memory. Must be called inside a KernelMutex / dstKernelSectionEnter
// region (__tsan_thread_ignore_begin under -race) so the copy neither reads
// nor writes user-tracked TSan shadow state: subsequent readers in other
// goroutines or post-CrashHost reboots (e.g. Readdir, Stat, Getwd) then never
// alias the original caller's user-space string buffer.
func dstCloneKernelString(s string) string {
	if len(s) == 0 {
		return ""
	}
	b := make([]byte, len(s))
	copy(b, s)
	return unsafe.String(&b[0], len(b))
}

func (d *dstFSDentry) childLocked(name string, child *dstFSNode) *dstFSDentry {
	if c := d.children[name]; c != nil && c.node == child && !c.unlinked {
		return c
	}
	name = dstCloneKernelString(name)
	c := &dstFSDentry{
		node:   child,
		parent: d,
		name:   name,
	}
	if d.children == nil {
		d.children = make(map[string]*dstFSDentry)
	}
	d.children[name] = c
	child.dentries = append(child.dentries, c)
	return c
}

func (d *dstFSDentry) isDeadLocked(root *dstFSDentry) bool {
	if d == nil || d.node.unlinked {
		return true
	}
	for p := d; p != root; p = p.parent {
		if p == nil || p.unlinked || p.parent == nil || p.parent.node.entries[p.name] != p.node {
			return true
		}
	}
	return false
}

func (cur *dstFSDentry) dentryChainLocked(root *dstFSDentry) []*dstFSDentry {
	if cur == nil || cur == root {
		return []*dstFSDentry{root}
	}
	var rev []*dstFSDentry
	for p := cur; p != nil && p != root; p = p.parent {
		rev = append(rev, p)
	}
	stack := make([]*dstFSDentry, 1, 1+len(rev)+4)
	stack[0] = root
	for i := len(rev) - 1; i >= 0; i-- {
		stack = append(stack, rev[i])
	}
	return stack
}

// attachEntryLocked links child into parent under name, tracking live directory
// parent-edge counts (dirLinks) so unlinking one alias of a crash-recovered
// dual-alias directory leaves the surviving alias live. Caller holds dstFS.mu.
func (parent *dstFSNode) attachEntryLocked(name string, child *dstFSNode) {
	name = dstCloneKernelString(name)
	prev := parent.entries[name]
	parent.entries[name] = child
	if child.isDir && prev != child {
		child.dirLinks++
	}
}

// dstFSCwdDentryHere returns the calling process's working directory dentry
// (default disk.rootDentry); dstFSSetCwdDentry sets it. Keyed by (host, process)
// so co-located processes and root bodies across hosts maintain independent cwds
// (DST-NODE-ISOLATION). Caller holds dstFS.mu.
func dstFSCwdDentryHere() *dstFSDentry {
	disk := dstFSDiskHere()
	host, proc := dstFSCurrentNode()
	if c := dstFS.cwds[[2]uint32{host, proc}]; c != nil && c.node.disk == disk {
		return c
	}
	return disk.rootDentry
}

func dstFSSetCwdDentry(cwd *dstFSDentry) {
	host, proc := dstFSCurrentNode()
	dstFS.cwds[[2]uint32{host, proc}] = cwd
}

// dstFSIsRoot reports whether node is the calling goroutine's host-tree root (which
// cannot be removed or renamed). Caller holds dstFS.mu.
func dstFSIsRoot(node *dstFSNode) bool {
	return node == dstFSDiskHere().root
}

// isDevice reports whether the node is a character device (the pre-seeded
// /dev/null — the only device the tree mints). The kind rides the mode's type
// bits, which chmod preserves (chmodLocked), so there is no second field to
// disagree with the mode. A device node stores nothing: data, synced, and pc
// stay nil for its whole life, and the disk fault machinery (latency, EIO,
// capacity) never applies — a real /dev/null does not live on the disk.
func (node *dstFSNode) isDevice() bool {
	return node.mode&ModeCharDevice != 0
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
	disk  *dstFSDisk

	// ino is the node's synthetic inode number, allocated at creation from the
	// per-run counter (dstFSAllocIno) — schedule-deterministic, never reused
	// within a run. It is the file-identity half of fstat(2)'s (st_dev, st_ino)
	// pair (st_dev derives from the host id), which inode-keyed SUTs (the
	// SQLite/LMDB per-file lock-dedup pattern) require to distinguish files. It
	// rides the node: stable across rename and while unlinked-but-open.
	ino uint64

	// nlink is a regular file's hard-link count: 1 at creation, +1 per
	// link(2), -1 per dirent removal (Remove, RemoveAll subtree, a rename
	// replacing the target). Rename itself moves a dirent and leaves it
	// unchanged. Restored from the durable entry sets on a host crash (one
	// per durable parent edge). Directories do not use it (their fixed
	// Nlink synthesis is recorded in the spec).
	nlink int

	// durableRefs counts syncedEntries edges referencing this node across its
	// disk's directories; openFiles and liveMaps count open file descriptions
	// and live SUT mappings on regular files. Together with nlink they govern
	// dstNodeMaybeReleaseLocked (INV-NODE-BACKING, INV-DURABLE-REACH).
	durableRefs int
	openFiles   int
	liveMaps    int

	// unlinked marks a directory removed from the namespace (Remove/RemoveAll,
	// or replaced by Rename). The kernel fails entry CREATION in an rmdir'd
	// directory with ENOENT even through a still-open handle (a Root captured
	// before the removal); lookups need no flag — an unlinked dir is empty.
	unlinked bool

	// Regular file state. data aliases the node's page cache (dstNodeCache)
	// and MUST NOT be appended to: an append that reallocated would move the
	// bytes out of the page cache, and the system under test's mappings would
	// stop seeing them. dstNodeSetSizeLocked is the only way to change its
	// length. blocks tracks allocated 4 KB pages (holes from truncate-grow stay
	// unallocated until written, preallocated via fallocate, or dirtied through
	// a writable mmap); synced is a sparse COW 4 KB page image.
	data            []byte
	blocks          []dstBlockSpan
	synced          dstDurableImage
	pc              *dstNodeCache
	everMappedWrite atomic.Bool

	// wbDroppedPages / wbRedirtied encode the fsyncgate mark in O(redirtied)
	// space rather than O(file_pages) so multi-hundred-GiB sparse files stay
	// O(1) on failed writeback: every page index pg < wbDroppedPages not in
	// wbRedirtied was marked CLEAN without writing back when a data sync failed
	// with EIO (Linux >= 4.13). A write redirties touched pages (inserting into
	// wbRedirtied); a later successful sync commits everything except still-
	// dropped pages, which keep their platter bytes. Reset on host crash.
	wbDroppedPages int64
	wbRedirtied    map[int64]struct{}
	// wbSnap captures a sparse COW snapshot of node.data at the failed writeback
	// (or when a dropped file first gains a writable shared mapping) so stores
	// through MAP_SHARED PROT_WRITE mappings redirty touched 4 KB pages even
	// though CPU stores bypass writeAtLocked.
	wbSnap    dstDurableImage
	wbHasSnap bool

	// rot is the silent-corruption mark (the CorruptFile fault): XOR masks
	// keyed by byte offset into the durable image — bit rot sitting on the
	// platter. The page cache masks it (data is never touched; live reads
	// keep returning the written bytes, and the model never evicts), so it
	// surfaces where a real machine discovers latent corruption: when the
	// platter is next read, at host-crash restore, where the masks fold into
	// the restored content and clear. Writeback heals it sector by sector: a
	// successful sync clears the masks of every page whose committed bytes
	// CHANGED (content diff is the dirty proxy — a byte-identical rewrite
	// does not clear, a recorded bound equivalent to the harness re-injecting
	// after the write), while pages the sync did not rewrite — untouched
	// pages, and fsyncgate-dropped pages — keep their rot, as a real disk
	// keeps rot under blocks writeback never rewrites. Nil = no rot pending.
	rot map[int64]byte

	// Directory state.
	entries       map[string]*dstFSNode
	syncedEntries map[string]*dstFSNode
	dirLinks      int            // live parent-directory edges (1 normally; >1 if crash recovery retained aliases)
	dentries      []*dstFSDentry // live materialized dentries referencing this directory inode

	mode    FileMode
	modTime time.Time

	// Durable metadata image, advanced only by sync (with the content
	// image); a future simulated crash restores these alongside synced.
	syncedMode    FileMode
	syncedModTime time.Time

	// Advisory flock state. This is host file-node state, not durable file
	// content; fd and process ownership live in the lock owner key.
	flock dstFlockState

	// Scoped per-node race-detector synchronization word: dataSync orders
	// writes/truncates before reads of this node's file data without forging
	// cross-file or universe-global happens-before edges.
	dataSync uint64
}

type dstFlockOwner struct {
	host uint32
	proc uint32
	fd   int
}

type dstFlockState struct {
	holders  map[dstFlockOwner]bool // true means exclusive, false means shared
	wait     chan struct{}
	raceSync uint64
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

// dstFSResolve resolves name against the calling host's tree root and the calling
// process's working directory by a COMPONENT-WISE PHYSICAL walk — the way the kernel
// walks a path, not a lexical `path.Clean`. Every intermediate component must exist
// and be a directory (ENOENT / ENOTDIR), `..` is evaluated against the tree during
// the walk (never erased lexically first — so `/missing/../x` is ENOENT, not a
// silent success on `/x`), and a trailing slash asserts the final component is a
// directory (`open("/regularfile/")` is ENOTDIR). Returns the parent directory node,
// the base name, and the target node (nil if absent). Caller holds dstFS.mu. Errors
// are bare errnos; callers wrap.
func dstFSResolve(name string) (parent *dstFSNode, base string, node *dstFSNode, errno error) {
	return dstFSResolveWalk(name, true)
}

func dstFSResolveDentry(name string) (parentDentry *dstFSDentry, base string, node *dstFSNode, nodeDentry *dstFSDentry, errno error) {
	return dstFSResolveDentryWalk(name, true)
}

// dstFSResolveWalk is dstFSResolve with the FINAL component's trailing-slash
// directory assertion optional. rename(2) needs it suppressed: the kernel
// checks both paths' trailing slashes against the SOURCE's type only after
// both parent walks and the old final's lookup (fs/namei.c do_renameat2's
// "unless the source is a directory trailing slashes give -ENOTDIR"), so a
// resolve-time assertion would fire ahead of errors that precede it
// (host-probed: rename("missing", "existingfile/") is ENOENT, not ENOTDIR).
// Intermediate-component walk errors are unaffected.
func dstFSResolveWalk(name string, assertFinalSlash bool) (parent *dstFSNode, base string, node *dstFSNode, errno error) {
	pd, base, node, _, errno := dstFSResolveDentryWalk(name, assertFinalSlash)
	if pd != nil {
		parent = pd.node
	}
	return parent, base, node, errno
}

func dstFSResolveDentryWalk(name string, assertFinalSlash bool) (parentDentry *dstFSDentry, base string, node *dstFSNode, nodeDentry *dstFSDentry, errno error) {
	if name == "" {
		return nil, "", nil, nil, syscall.ENOENT
	}
	disk := dstFSDiskHere()
	rootDentry := disk.rootDentry
	comps, trailingSlash := dstFSComponents(name)
	trailingSlash = trailingSlash && assertFinalSlash
	// stack holds the dentry chain from rootDentry. Relative paths start at the
	// process's working directory dentry chain (fs->pwd); `..` pops toward root,
	// and at root stays (POSIX: `/..` == `/`).
	stack := []*dstFSDentry{rootDentry}
	if name[0] != '/' {
		stack = dstFSCwdDentryHere().dentryChainLocked(rootDentry)
	}
	terminalDir := func() (*dstFSDentry, string, *dstFSNode, *dstFSDentry, error) {
		// A path ending in `.`/`..` (or all-slashes) resolves to the current dir. A
		// trailing slash on a directory is fine.
		cur := stack[len(stack)-1]
		if len(stack) == 1 {
			return nil, "/", cur.node, cur, nil
		}
		return stack[len(stack)-2], cur.name, cur.node, cur, nil
	}
	for i, elem := range comps {
		last := i == len(comps)-1
		cur := stack[len(stack)-1]
		switch elem {
		case ".":
			if last {
				return terminalDir()
			}
		case "..":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			if last {
				return terminalDir()
			}
		default:
			if last {
				n := cur.node.entries[elem]
				if trailingSlash && n != nil && !n.isDir {
					return nil, "", nil, nil, syscall.ENOTDIR
				}
				var nd *dstFSDentry
				if n != nil && n.isDir {
					nd = cur.childLocked(elem, n)
				}
				return cur, elem, n, nd, nil
			}
			n := cur.node.entries[elem]
			if n == nil {
				return nil, "", nil, nil, syscall.ENOENT
			}
			if !n.isDir {
				return nil, "", nil, nil, syscall.ENOTDIR
			}
			stack = append(stack, cur.childLocked(elem, n))
		}
	}
	// No components (root, or all trailing slashes) → the root directory.
	return terminalDir()
}

// dstFSComponents splits name into path components WITHOUT collapsing `.`/`..`
// (those are resolved physically by the walk in dstFSResolveDentryWalk, starting
// from root for absolute paths or dstFSCwdDentryHere for relative paths) — only
// redundant slashes are dropped. It reports whether a non-root path had a
// trailing slash (which asserts the target is a directory).
func dstFSComponents(name string) (comps []string, trailingSlash bool) {
	trailingSlash = len(name) > 1 && name[len(name)-1] == '/'
	start := 0
	for i := 0; i <= len(name); i++ {
		if i == len(name) || name[i] == '/' {
			if i > start {
				comps = append(comps, name[start:i])
			}
			start = i + 1
		}
	}
	return comps, trailingSlash
}

// dstMkdir implements Mkdir on the simulated tree.
func dstMkdir(name string, perm FileMode) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	dstCoarsePathDep(name, true)
	dstDiskDelayPath(name)
	if dstProcReserved(name) {
		return true, &PathError{Op: "mkdir", Path: name, Err: dstErrUnsupportedFS}
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	wrap := func(e error) (bool, error) { return true, &PathError{Op: "mkdir", Path: name, Err: e} }
	if name == "" {
		return wrap(syscall.ENOENT)
	}
	// mkdir(2) reports a positive final dentry EEXIST before the trailing
	// slash's directory assertion (filename_create looks the dentry up first;
	// host-probed: mkdir("file/") is EEXIST, not ENOTDIR), so mkdir resolves
	// with trailing slashes stripped and lets the existing-node check answer.
	trimmed := name
	for len(trimmed) > 0 && trimmed[len(trimmed)-1] == '/' {
		trimmed = trimmed[:len(trimmed)-1]
	}
	if trimmed == "" {
		trimmed = name // "/" and all-slash names keep their root resolution
	}
	parentDentry, base, node, _, errno := dstFSResolveDentry(trimmed)
	if errno != nil {
		return wrap(errno)
	}
	if node != nil {
		return wrap(syscall.EEXIST)
	}
	disk := dstFSDiskHere()
	if disk.capped.Load() {
		dstCoarseRecord(uintptr(unsafe.Pointer(disk)), false)
	}
	if parentDentry.isDeadLocked(disk.rootDentry) {
		return wrap(syscall.ENOENT)
	}
	if disk.diskFullForCreate() {
		dstRecordCausalCaller(dstCausalFSENOSPC, dstFSOpMkdir, 0, 0, 0, 0, name)
		return wrap(syscall.ENOSPC)
	}
	parent := parentDentry.node
	parent.attachEntryLocked(base, dstFSNewNode(disk, ModeDir, perm))
	parent.modTime = time.Now()
	return true, nil
}

func dstFSUnlinkEntryLocked(parent *dstFSNode, base string, child *dstFSNode) {
	delete(parent.entries, base)
	if !child.isDir {
		dstFSDropLink(child)
		return
	}
	keep := child.dentries[:0]
	for _, d := range child.dentries {
		if !d.unlinked && d.parent != nil && d.parent.node == parent && d.name == base {
			d.unlinked = true
			if d.parent.children[base] == d {
				delete(d.parent.children, base)
			}
			continue
		}
		if !d.unlinked {
			keep = append(keep, d)
		}
	}
	clear(child.dentries[len(keep):])
	child.dentries = keep
	if child.dirLinks > 0 {
		child.dirLinks--
	}
	if child.dirLinks == 0 {
		dstFSMarkUnlinked(child)
	}
}

func dstFSClearDirEntriesLocked(dir *dstFSNode) {
	if !dir.isDir || len(dir.entries) == 0 {
		return
	}
	dstFSClearDirEntriesSeenLocked(dir, make(map[*dstFSNode]bool))
}

func dstFSClearDirEntriesSeenLocked(dir *dstFSNode, seen map[*dstFSNode]bool) {
	if !dir.isDir || seen[dir] {
		return
	}
	seen[dir] = true
	for name, child := range dir.entries {
		if child.isDir {
			dstFSClearDirEntriesSeenLocked(child, seen)
		}
		dstFSUnlinkEntryLocked(dir, name, child)
	}
}

func dstFSMoveEntryLocked(oldParentDentry *dstFSDentry, oldBase string, oldNode *dstFSNode, newParentDentry *dstFSDentry, newBase string, newNode *dstFSNode) {
	if newNode != nil {
		dstFSUnlinkEntryLocked(newParentDentry.node, newBase, newNode)
	}
	newBase = dstCloneKernelString(newBase)
	oldParent := oldParentDentry.node
	newParent := newParentDentry.node
	delete(oldParent.entries, oldBase)
	newParent.entries[newBase] = oldNode
	if oldNode.isDir {
		for _, d := range oldNode.dentries {
			if d.unlinked || d.parent == nil || d.parent.node != oldParent || d.name != oldBase {
				continue
			}
			if d.parent.children[oldBase] == d {
				delete(d.parent.children, oldBase)
			}
			targetParent := newParentDentry
			preserveSiblingParent := oldParent == newParent && d.parent != oldParentDentry
			if preserveSiblingParent {
				targetParent = d.parent
			}
			wasPrimary := d.parent == oldParentDentry || preserveSiblingParent
			d.parent = targetParent
			d.name = newBase
			if targetParent.children == nil {
				targetParent.children = make(map[string]*dstFSDentry)
			}
			if wasPrimary || targetParent.children[newBase] == nil {
				targetParent.children[newBase] = d
			}
		}
	}
}

// dstFSMarkUnlinked marks a directory inode whose last parent link was removed
// from the namespace — and every directory under it — so entry creation and
// readdir through a still-open handle fail ENOENT as the kernel's do in an
// rmdir'd directory. Unlinked-but-open file CONTENT is untouched: it lives on
// the node, and open handles keep it per the POSIX contract. Caller holds dstFS.mu.
func dstFSMarkUnlinked(node *dstFSNode) {
	if !node.isDir || node.unlinked {
		return
	}
	node.unlinked = true
	for _, d := range node.dentries {
		d.unlinked = true
		if d.parent != nil && d.parent.children[d.name] == d {
			delete(d.parent.children, d.name)
		}
	}
	node.dentries = nil
	dstFSClearDirEntriesLocked(node)
	if node.durableRefs == 0 {
		node.dropSyncedEntriesLocked()
	}
}

func (dir *dstFSNode) dropSyncedEntriesLocked() {
	if len(dir.syncedEntries) == 0 {
		dir.syncedEntries = nil
		return
	}
	old := dir.syncedEntries
	dir.syncedEntries = nil
	for _, child := range old {
		dstNodeDropDurableRefLocked(child)
	}
}

func dstNodeAddDurableRefLocked(node *dstFSNode) {
	if node == nil || node.isDevice() {
		return
	}
	node.durableRefs++
}

func dstNodeDropDurableRefLocked(node *dstFSNode) {
	if node == nil || node.isDevice() || node.durableRefs <= 0 {
		return
	}
	node.durableRefs--
	if node.isDir {
		if node.unlinked && node.durableRefs == 0 {
			node.dropSyncedEntriesLocked()
		}
		return
	}
	dstNodeMaybeReleaseLocked(node)
}

// dstFSDropLink accounts one dirent's removal against a regular file's
// hard-link count. Every path that deletes a file's dirent (Remove, a
// RemoveAll subtree clear, a rename replacing its target) routes one
// call here; the node's content lives on while other links — or open
// handles — remain, exactly the POSIX contract. Caller holds dstFS.mu.
func dstFSDropLink(node *dstFSNode) {
	if !node.isDir && node.nlink > 0 {
		node.nlink--
		dstNodeMaybeReleaseLocked(node)
	}
}

// dstRemove implements Remove: files and empty directories. The node outlives
// the entry while open handles reference it (unlinked-but-open).
func dstRemove(name string) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	dstCoarsePathDep(name, true)
	dstDiskDelayPath(name)
	if dstProcReserved(name) {
		return true, &PathError{Op: "remove", Path: name, Err: dstErrUnsupportedFS}
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	wrap := func(e error) (bool, error) { return true, &PathError{Op: "remove", Path: name, Err: e} }
	if name == "" {
		return wrap(syscall.ENOENT)
	}
	parent, base, node, errno := dstFSResolve(name)
	if errno != nil {
		return wrap(errno)
	}
	if node == nil {
		return wrap(syscall.ENOENT)
	}
	if terminal := path.Base(name); terminal == "." || terminal == ".." {
		return wrap(syscall.EINVAL)
	}
	if dstFSIsRoot(node) {
		return wrap(syscall.EBUSY)
	}
	if node.isDir && len(node.entries) > 0 {
		return wrap(syscall.ENOTEMPTY)
	}
	if disk := dstFSDiskHere(); disk.capped.Load() {
		dstCoarseRecord(uintptr(unsafe.Pointer(disk)), true)
	}
	dstFSUnlinkEntryLocked(parent, base, node)
	parent.modTime = time.Now()
	return true, nil
}

// dstRemoveAll implements RemoveAll directly on the tree: one atomic subtree
// unlink under the lock (the portable removeAll uses openat-family syscalls
// that never reach the gated funnels). A missing path is success, like the
// host's.
func dstRemoveAll(name string) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	dstCoarsePathDep(name, true)
	dstDiskDelayPath(name)
	if dstProcReserved(name) {
		return true, &PathError{Op: "removeall", Path: name, Err: dstErrUnsupportedFS}
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	if name == "" {
		return true, nil // RemoveAll("") is a no-op success, like the host
	}
	parent, base, node, errno := dstFSResolve(name)
	if errno != nil {
		if errno == syscall.ENOENT {
			return true, nil
		}
		return true, &PathError{Op: "removeall", Path: name, Err: errno}
	}
	if node == nil {
		return true, nil
	}
	if terminal := path.Base(name); terminal == "." || terminal == ".." {
		return true, &PathError{Op: "RemoveAll", Path: name, Err: syscall.EINVAL}
	}
	if dstFSIsRoot(node) {
		// Match RemoveAll("/"): refuse to destroy the root.
		return true, &PathError{Op: "removeall", Path: name, Err: syscall.EBUSY}
	}
	if disk := dstFSDiskHere(); disk.capped.Load() {
		dstCoarseRecord(uintptr(unsafe.Pointer(disk)), true)
	}
	if node.isDir {
		dstFSClearDirEntriesLocked(node)
	}
	dstFSUnlinkEntryLocked(parent, base, node)
	parent.modTime = time.Now()
	return true, nil
}

// dstLink implements link(2) on the tree: a second dirent for the same
// node — SameFile identity, shared bytes through every name and mapping,
// content surviving until the last link AND handle goes. Error order is
// the kernel's, host-probed on tmpfs: the old walk resolves first
// (missing old answers ENOENT even with a slashed new), a slashed
// regular-file old is ENOTDIR, ANY directory old — ".", "..", the root
// included — is EPERM; then the new walk's errors, a positive new is
// EEXIST (beating the slash rule), and a slashed MISSING new is ENOENT
// (link cannot create a directory-asserted name). Cross-device EXDEV is
// unconstructible: one tree per host. Raw SYS_LINK/SYS_LINKAT stay
// fenced — the modeled entry is os.Link.
func dstLink(oldname, newname string) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	dstCoarsePathDep2(oldname, newname, true)
	dstDiskDelayPath2(oldname, newname)
	if dstProcReserved(oldname) || dstProcReserved(newname) {
		return true, &LinkError{Op: "link", Old: oldname, New: newname, Err: dstErrUnsupportedFS}
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	wrap := func(e error) (bool, error) { return true, &LinkError{Op: "link", Old: oldname, New: newname, Err: e} }
	if oldname == "" || newname == "" {
		return wrap(syscall.ENOENT)
	}
	_, _, oldNode, errno := dstFSResolveWalk(oldname, false)
	if errno != nil {
		return wrap(errno)
	}
	if oldNode == nil {
		return wrap(syscall.ENOENT)
	}
	if _, oldSlash := dstFSComponents(oldname); oldSlash && !oldNode.isDir {
		return wrap(syscall.ENOTDIR)
	}
	newParentDentry, newBase, newNode, _, errno := dstFSResolveDentryWalk(newname, false)
	if errno != nil {
		return wrap(errno)
	}
	if newNode != nil {
		return wrap(syscall.EEXIST)
	}
	if _, newSlash := dstFSComponents(newname); newSlash {
		return wrap(syscall.ENOENT)
	}
	disk := dstFSDiskHere()
	if newParentDentry.isDeadLocked(disk.rootDentry) {
		return wrap(syscall.ENOENT)
	}
	// LAST, after every new-side answer — host-probed: a directory old
	// with an existing new is EEXIST, with a slashed or unreachable new
	// ENOENT; EPERM only when the new side would have succeeded.
	if oldNode.isDir {
		return wrap(syscall.EPERM)
	}
	newParent := newParentDentry.node
	newParent.attachEntryLocked(newBase, oldNode)
	oldNode.nlink++
	newParent.modTime = time.Now()
	return true, nil
}

// dstRename implements the rename syscall's semantics on the tree: atomic in
// the namespace (observers under the same lock see old or new, never
// neither), replacing a file target, requiring an empty directory target for
// a directory source, and refusing ancestor moves.
func dstRename(oldname, newname string) (handled bool, err error) {
	return dstRenameFlags(oldname, newname, false)
}

// dstRenameFlags is dstRename with renameat2's RENAME_NOREPLACE arm: when
// noreplace is set, an existing newname answers EEXIST — including the
// same-node case, which the kernel refuses before its same-file no-op check
// (the NOREPLACE lookup fails on any positive target dentry).
func dstRenameFlags(oldname, newname string, noreplace bool) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	dstCoarsePathDep2(oldname, newname, true)
	dstDiskDelayPath2(oldname, newname)
	if dstProcReserved(oldname) || dstProcReserved(newname) {
		return true, &LinkError{Op: "rename", Old: oldname, New: newname, Err: dstErrUnsupportedFS}
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	wrap := func(e error) (bool, error) { return true, &LinkError{Op: "rename", Old: oldname, New: newname, Err: e} }
	if oldname == "" || newname == "" {
		return wrap(syscall.ENOENT)
	}
	// Check order mirrors rename(2), which resolves BOTH parent walks
	// (old first, then new) and the terminal-dot restrictions before it
	// looks up the old final component: a destination walk error
	// (ENOTDIR through a file, ENOENT through a missing directory)
	// precedes the missing-source ENOENT, and either path's "." / ".."
	// terminal is EBUSY ahead of it too. Trailing slashes on EITHER
	// path's final component are checked against the SOURCE's type,
	// after the old final's existence check (fs/namei.c do_renameat2:
	// "unless the source is a directory trailing slashes give
	// -ENOTDIR") — so rename("missing", "x/") is ENOENT, not ENOTDIR,
	// and a DIRECTORY source renames onto a trailing-slash missing
	// newpath successfully (host-probed on ext4 and tmpfs alike). The
	// final-slash walk assertion is therefore suppressed here and
	// applied at the source-type stage below.
	oldParentDentry, oldBase, oldNode, _, errno := dstFSResolveDentryWalk(oldname, false)
	if errno != nil {
		return wrap(errno)
	}
	_, oldTrailingSlash := dstFSComponents(oldname)
	newParentDentry, newBase, newNode, _, errno := dstFSResolveDentryWalk(newname, false)
	if errno != nil {
		return wrap(errno)
	}
	_, newTrailingSlash := dstFSComponents(newname)
	if terminal := path.Base(oldname); terminal == "." || terminal == ".." {
		return wrap(syscall.EBUSY)
	}
	if terminal := path.Base(newname); terminal == "." || terminal == ".." {
		return wrap(syscall.EBUSY)
	}
	if oldNode == nil {
		return wrap(syscall.ENOENT)
	}
	if dstFSIsRoot(oldNode) || dstFSIsRoot(newNode) {
		// Before the source-type slash check: a root-resolving path is
		// EBUSY (the kernel's LAST_ROOT refusal), never ENOTDIR. The
		// shape is unreachable through os.Rename — its preamble
		// intercepts every existing-dir newname (EEXIST) and errors a
		// missing "/" oldname in the preamble stat — so this arm pins
		// the internal ladder's order, not a surface behavior.
		return wrap(syscall.EBUSY)
	}
	if noreplace && newNode != nil {
		// RENAME_NOREPLACE: any positive target refuses EEXIST — ahead of
		// the trailing-slash source-type ENOTDIR AND the same-node no-op
		// (host-probed: rename("f/", "g", NOREPLACE) and
		// rename("f", "g/", NOREPLACE) with both files present answer
		// EEXIST on tmpfs; do_renameat2's positive-target check precedes
		// the slash rule).
		return wrap(syscall.EEXIST)
	}
	disk := dstFSDiskHere()
	if newNode == nil && newParentDentry != nil && newParentDentry.isDeadLocked(disk.rootDentry) {
		return wrap(syscall.ENOENT)
	}
	if !oldNode.isDir && (oldTrailingSlash || newTrailingSlash) {
		return wrap(syscall.ENOTDIR)
	}
	if newNode == oldNode {
		return true, nil // same file: POSIX no-op success
	}
	oldParent := oldParentDentry.node
	newParent := newParentDentry.node
	if oldNode.isDir && dstNodeContains(oldNode, newParent) {
		return wrap(syscall.EINVAL) // new is inside old
	}
	if newNode != nil {
		switch {
		case newNode.isDir && !oldNode.isDir:
			return wrap(syscall.EISDIR)
		case !newNode.isDir && oldNode.isDir:
			return wrap(syscall.ENOTDIR)
		case newNode.isDir && len(newNode.entries) > 0:
			return wrap(syscall.ENOTEMPTY)
		}
	}
	if newNode != nil && disk.capped.Load() {
		dstCoarseRecord(uintptr(unsafe.Pointer(disk)), true)
	}
	dstFSMoveEntryLocked(oldParentDentry, oldBase, oldNode, newParentDentry, newBase, newNode)
	now := time.Now()
	oldParent.modTime = now
	newParent.modTime = now
	return true, nil
}

func dstNodeContains(root, node *dstFSNode) bool {
	return dstNodeContainsSeen(root, node, make(map[*dstFSNode]bool))
}

func dstNodeContainsSeen(root, node *dstFSNode, seen map[*dstFSNode]bool) bool {
	if root == node {
		return true
	}
	if !root.isDir || seen[root] {
		return false
	}
	seen[root] = true
	for _, child := range root.entries {
		if dstNodeContainsSeen(child, node, seen) {
			return true
		}
	}
	return false
}

// dstStatName implements named Stat (and Lstat — no symlinks in the base
// model, so they coincide).
func dstStatName(op, name string) (FileInfo, bool, error) {
	if !dstFSActive() {
		return nil, false, nil
	}
	dstCoarsePathDep(name, false)
	dstDiskDelayPath(name)
	if fi, handled, err := dstProcStatName(op, name); handled {
		return fi, true, err
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	if name == "" {
		return nil, true, &PathError{Op: op, Path: name, Err: syscall.ENOENT}
	}
	_, _, node, errno := dstFSResolve(name)
	if errno != nil {
		return nil, true, &PathError{Op: op, Path: name, Err: errno}
	}
	if node == nil {
		return nil, true, &PathError{Op: op, Path: name, Err: syscall.ENOENT}
	}
	dstCoarseNodeRecord(node, false)
	return dstFileInfoForNode(filepathlite.Base(name), node), true, nil
}

// dstTempDir reports the fixed simulated temp directory while a run is
// active: the host's $TMPDIR-derived string is machine state.
func dstTempDir() (string, bool) {
	if !dstFSActive() {
		return "", false
	}
	return "/tmp", true
}

// dstTruncateName implements the named Truncate (truncate(2) shapes:
// EISDIR for directories). Mutates current content only.
func dstTruncateName(name string, size int64) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	dstCoarsePathDep(name, true)
	dstDiskDelayPath(name)
	if dstProcReserved(name) {
		return true, &PathError{Op: "truncate", Path: name, Err: dstErrUnsupportedFS}
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	wrap := func(e error) (bool, error) { return true, &PathError{Op: "truncate", Path: name, Err: e} }
	if name == "" {
		return wrap(syscall.ENOENT)
	}
	if size < 0 {
		return wrap(syscall.EINVAL)
	}
	_, _, node, errno := dstFSResolve(name)
	if errno != nil {
		return wrap(errno)
	}
	if node == nil {
		return wrap(syscall.ENOENT)
	}
	if node.isDir {
		return wrap(syscall.EISDIR)
	}
	if node.isDevice() {
		// truncate(2) on a character device is EINVAL (host-verified).
		return wrap(syscall.EINVAL)
	}
	if disk := dstFSDiskHere(); disk.capped.Load() {
		dstCoarseRecord(uintptr(unsafe.Pointer(disk)), true)
	}
	if err := node.truncateLocked(size); err != nil {
		return wrap(err)
	}
	return true, nil
}

// truncateLocked clamps or zero-extends current content. Caller holds
// dstFS.mu. Shared by handle and named truncate. A shrink under a live
// mapping is ordinary: the cut pages trap on access from then on (the
// process dies, as under production SIGBUS) and the partial page's tail
// zeroes — ftruncate semantics, which the page cache provides directly.
func (node *dstFSNode) truncateLocked(size int64) error {
	if size > dstSMaxBytes {
		// inode_newsize_ok: growth past the filesystem's maximum file size
		// is EFBIG, never a mapping-reserve fatal.
		return syscall.EFBIG
	}
	if size < int64(len(node.data)) {
		// A shrink zeroes the kept partial page's tail (redirtying size/4096)
		// and destroys pages wholly beyond size (>= ceil(size/4096)), leaving
		// only dropped pages strictly below size/dstPageSize.
		keepPages := size / dstPageSize
		if keepPages < node.wbDroppedPages {
			node.wbDroppedPages = keepPages
			for pg := range node.wbRedirtied {
				if pg >= keepPages {
					delete(node.wbRedirtied, pg)
				}
			}
			if int64(len(node.wbRedirtied)) == node.wbDroppedPages {
				node.clearWritebackDroppedLocked()
			}
		}
		node.syncMmapBlocksLocked()
		node.blocks = dstBlocksTruncate(node.blocks, (size+dstPageSize-1)/dstPageSize)
	}
	dstNodeSetSizeLocked(node, size)
	node.modTime = time.Now()
	dstCoarseNodeRecord(node, true)
	dstKernelRaceReleaseMerge(unsafe.Pointer(&node.dataSync))
	return nil
}

// chmodLocked applies a mode change to a resolved node: permission plus the
// setuid/setgid/sticky bits, preserving the type bits. A mutation — the
// durable metadata image moves only on sync. ModTime is untouched: chmod(2)
// updates ctime only, which is not modeled.
func (node *dstFSNode) chmodLocked(mode FileMode) {
	dstCoarseNodeRecord(node, true)
	node.mode = node.mode&^dstFSModeMask | mode&dstFSModeMask
}

// dstChmod implements the named Chmod.
func dstChmod(name string, mode FileMode) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	dstCoarsePathDep(name, true)
	dstDiskDelayPath(name)
	if dstProcReserved(name) {
		return true, &PathError{Op: "chmod", Path: name, Err: dstErrUnsupportedFS}
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	wrap := func(e error) (bool, error) { return true, &PathError{Op: "chmod", Path: name, Err: e} }
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
	node.chmodLocked(mode)
	return true, nil
}

// chmodHandle implements File.Chmod on a simulated handle.
func (d *dstFile) chmodHandle(mode FileMode) error {
	d.coarseOp(false, true)
	d.diskDelay()
	if err := d.enter(); err != nil {
		return err
	}
	defer d.leave()
	d.node.chmodLocked(mode)
	return nil
}

// dstChtimes implements Chtimes: the zero time leaves the corresponding
// stamp unchanged (the os contract). Only modTime is modeled; atime is not
// represented (the host's relatime makes it untestable state anyway).
func dstChtimes(name string, atime, mtime time.Time) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	dstCoarsePathDep(name, true)
	dstDiskDelayPath(name)
	if dstProcReserved(name) {
		return true, &PathError{Op: "chtimes", Path: name, Err: dstErrUnsupportedFS}
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	wrap := func(e error) (bool, error) { return true, &PathError{Op: "chtimes", Path: name, Err: e} }
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
	dstCoarseNodeRecord(node, true)
	if !mtime.IsZero() {
		node.modTime = mtime
	}
	_ = atime
	return true, nil
}

// dstGetwd / dstChdir: the per-process working directory dentry.
func dstGetwd() (string, bool, error) {
	if !dstFSActive() {
		return "", false, nil
	}
	dstCoarseDep(dstCoarseCwdID(), false)
	dstCoarseRecord(dstCoarseNamespaceID(), false)
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	disk := dstFSDiskHere()
	root := disk.rootDentry
	cur := dstFSCwdDentryHere()
	if cur == root {
		return "/", true, nil
	}
	if cur.isDeadLocked(root) {
		return "", true, NewSyscallError("getwd", syscall.ENOENT)
	}
	var rev []string
	for p := cur; p != root; p = p.parent {
		rev = append(rev, p.name)
	}
	var n int
	for _, s := range rev {
		n += 1 + len(s)
	}
	buf := make([]byte, 0, n)
	for i := len(rev) - 1; i >= 0; i-- {
		buf = append(buf, '/')
		buf = append(buf, rev[i]...)
	}
	return string(buf), true, nil
}

func dstChdir(dir string) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	dstCoarseDep(dstCoarseNamespaceID(), false)
	dstCoarseRecord(dstCoarseCwdID(), true)
	dstDiskDelayPath(dir)
	if dstProcReserved(dir) {
		return true, &PathError{Op: "chdir", Path: dir, Err: dstErrUnsupportedFS}
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	wrap := func(e error) (bool, error) { return true, &PathError{Op: "chdir", Path: dir, Err: e} }
	if dir == "" {
		return wrap(syscall.ENOENT)
	}
	_, _, node, nodeDentry, errno := dstFSResolveDentry(dir)
	if errno != nil {
		return wrap(errno)
	}
	if node == nil {
		return wrap(syscall.ENOENT)
	}
	if !node.isDir {
		return wrap(syscall.ENOTDIR)
	}
	dstFSSetCwdDentry(nodeDentry)
	return true, nil
}

// dstFile is the open-handle state for a simulated file: the node reference,
// the seek offset, and the access mode. It is the tree-file backend behind
// the os.File dst seam (dstFileBackend); the pipe (dst_pipe.go) is the
// sibling stream backend — the non-foreclosure shape recorded in the spec.
var _ dstFileBackend = (*dstFile)(nil)

type dstFile struct {
	mu        isync.KernelMutex
	node      *dstFSNode
	dentry    *dstFSDentry // non-nil on directory handles (for fchdir)
	disk      *dstFSDisk   // the host disk this handle was opened on (for disk faults)
	path      string
	off       int64
	dirpos    int // directory read cursor (sorted-name index)
	rd, wr    bool
	app       bool
	osync     bool // full O_SYNC
	odsync    bool // O_DSYNC: data and size, but not timestamps
	closed    bool
	closeSync uint64
	// epoch is the run this handle belongs to, stamped at creation
	// (dstNewFile). enter() refuses a handle from a dead run as it refuses a
	// closed one: the run's nodes were released with the run (their page
	// caches are gone — node.pc is nil), and before this gate a leaked *File
	// dereferenced them. The virtual-fd front door has the same gate
	// (dstFDLookup's epoch check answers EBADF).
	epoch uint64
	// flockPending is a flock this description still holds after its
	// descriptor closed: Linux releases a BSD flock with the open file
	// description's last reference, and a shared mapping taken through
	// the description is one, so the close defers the release to the last
	// unmap (dstFlockReleaseFD, dstMMapReleaseFile). Atomic: set and taken
	// under the mapping registry's lock, never under this handle's mu.
	flockPending atomic.Pointer[dstFlockPending]
}

// dstOpenFile implements OpenFile against the simulated tree while a run is
// active. handled=false outside a run (fall through to the host).
func dstOpenFile(name string, flag int, perm FileMode) (f *File, handled bool, err error) {
	if !dstFSActive() {
		return nil, false, nil
	}
	// A create-mode or O_TRUNC open can mutate the namespace/inode; a plain open
	// only resolves it. Announced before /proc dispatch so relative paths into
	// /proc carry the same cwd + namespace read conflict in a single yield.
	dstCoarsePathDep(name, flag&(O_CREATE|O_TRUNC) != 0)
	dstDiskDelayPath(name)
	if f, handled, err := dstProcOpenFile(name, flag); handled {
		return f, true, err
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

	parentDentry, base, node, nodeDentry, errno := dstFSResolveDentry(name)
	if errno != nil {
		return wrap(errno)
	}
	disk := dstFSDiskHere()

	accWrite := flag&(O_WRONLY|O_RDWR) != 0
	if node != nil && node.isDir {
		// do_open's ordering: the O_CREAT|O_EXCL existence check precedes BOTH
		// may_open's EISDIR and the write/O_TRUNC access checks — an existing
		// directory answers EEXIST for any O_CREAT|O_EXCL open regardless of
		// access mode or O_TRUNC (kernel-verified). Only then does the
		// directory reject write access, O_TRUNC (regardless of access mode),
		// and plain O_CREAT — before any mutation, so the truncate below must
		// never run on — nor bump the mtime of — a directory.
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
		if len(name) > 1 && name[len(name)-1] == '/' {
			// A trailing slash asserts a directory; O_CREAT cannot mint a regular
			// file through one (real Linux: EISDIR). The resolver already rejects a
			// trailing slash on an existing non-dir (ENOTDIR); this is its create leg.
			return wrap(syscall.EISDIR)
		}
		if disk.capped.Load() {
			dstCoarseRecord(uintptr(unsafe.Pointer(disk)), false)
		}
		if parentDentry.isDeadLocked(disk.rootDentry) {
			return wrap(syscall.ENOENT)
		}
		if disk.diskFullForCreate() {
			dstRecordCausalCaller(dstCausalFSENOSPC, dstFSOpCreate, 0, 0, 0, 0, name)
			return wrap(syscall.ENOSPC)
		}
		node = dstFSNewNode(disk, 0, perm)
		parent := parentDentry.node
		parent.attachEntryLocked(base, node)
		parent.modTime = time.Now()
	}
	if flag&O_TRUNC != 0 && !node.isDevice() {
		// Linux truncates even for O_RDONLY|O_TRUNC; match the host's shape.
		// Truncation mutates current content only; the durable image moves
		// on Sync, never on a mutation (the durability contract). On a
		// character device the kernel ignores O_TRUNC (handle_truncate runs
		// for regular files only; host-verified error-free).
		if disk.capped.Load() {
			dstCoarseRecord(uintptr(unsafe.Pointer(disk)), true)
		}
		if err := node.truncateLocked(0); err != nil {
			return wrap(err)
		}
	}

	osync, odsync := dstSyncModes(flag)
	d := &dstFile{
		node:   node,
		dentry: nodeDentry,
		disk:   disk,
		path:   name,
		rd:     flag&O_WRONLY == 0,
		wr:     accWrite,
		app:    flag&O_APPEND != 0,
		osync:  osync,
		odsync: odsync,
	}
	return dstNewFile(d, name), true, nil
}

// dstOpenDir is openDirNolog's counterpart: the target must be a directory
// (ENOTDIR for a file — the O_DIRECTORY shape), opened read-only.
func dstOpenDir(name string) (f *File, handled bool, err error) {
	if !dstFSActive() {
		return nil, false, nil
	}
	dstCoarsePathDep(name, false)
	dstDiskDelayPath(name)
	if dstProcReserved(name) {
		return nil, true, &PathError{Op: "open", Path: name, Err: dstErrUnsupportedFS}
	}
	wrap := func(e error) (*File, bool, error) {
		return nil, true, &PathError{Op: "open", Path: name, Err: e}
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	if name == "" {
		return wrap(syscall.ENOENT)
	}
	_, _, node, nodeDentry, errno := dstFSResolveDentry(name)
	if errno != nil {
		return wrap(errno)
	}
	if node == nil {
		return wrap(syscall.ENOENT)
	}
	if !node.isDir {
		return wrap(syscall.ENOTDIR)
	}
	d := &dstFile{node: node, dentry: nodeDentry, disk: dstFSDiskHere(), path: name, rd: true}
	return dstNewFile(d, name), true, nil
}

// dstNewFile builds an *os.File backed by a simulated file. The pfd's
// Sysfd carries the out-of-line state row's slot (below the -1
// sentinel, so still invalid): any not-yet-gated path fails with EBADF
// deterministically instead of touching a real descriptor
// (dst_filestate.go).
func dstNewFile(d dstFileBackend, name string) *File {
	dstKernelSectionEnter()
	defer dstKernelSectionExit()
	name = dstCloneKernelString(name)
	switch b := d.(type) {
	case *dstFile:
		b.epoch = dstFSEpoch()
		b.path = dstCloneKernelString(b.path)
		if b.node != nil && !b.node.isDir && !b.node.isDevice() {
			b.node.openFiles++
		}
	case *dstProcFile:
		b.epoch = dstFSEpoch()
		b.name = dstCloneKernelString(b.name)
		b.base = dstCloneKernelString(b.base)
	}
	f := &File{&file{name: name}}
	dstSetFileBackend(f.file, d)
	host, proc := dstFSCurrentNode()
	dstRegisterOpenFile(f.file, host, proc)
	return f
}

// enter marks the start of a handle operation: it validates the handle and
// acquires the tree lock (handle lock first, then tree lock — the fixed
// order). Callers must call leave (via defer) when it returns nil.
func (d *dstFile) enter() error {
	d.mu.Lock()
	if d.closed || !dstFSActive() || d.epoch != dstFSEpoch() {
		if d.closed && dstFSActive() && d.epoch == dstFSEpoch() {
			dstKernelRaceAcquire(unsafe.Pointer(&d.closeSync))
		}
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

//go:linkname dstKernelSectionEnter runtime.dstKernelSectionEnter
func dstKernelSectionEnter()

//go:linkname dstKernelSectionExit runtime.dstKernelSectionExit
func dstKernelSectionExit()

//go:linkname dstKernelRaceReleaseMerge runtime.dstKernelRaceReleaseMerge
func dstKernelRaceReleaseMerge(addr unsafe.Pointer)

//go:linkname dstKernelRaceAcquire runtime.dstKernelRaceAcquire
func dstKernelRaceAcquire(addr unsafe.Pointer)

//go:linkname dstCoarseDep runtime.dstCoarseDep
func dstCoarseDep(id uintptr, write bool)

//go:linkname dstCoarseRecord runtime.dstCoarseRecord
func dstCoarseRecord(id uintptr, write bool)

// Synthetic coarse DPOR dependency identities partition page 0 ([1, 4095]) so
// every domain stays collision-free against heap/global pointers on both 32-bit
// and 64-bit targets without allocating or locking:
//   - [1, 1024]    : per-host filesystem namespace (os)
//   - [1025, 2048] : per-host network listener/port table (net)
//   - [2049, 3072] : per-(host, proc) working directory (os)
//   - [3073, 3584] : virtual OS file descriptors (os)
//   - [3585, 4095] : simulated socket option descriptors (net)
const (
	dstCoarseFSNamespaceBase = 1
	dstCoarseFSNamespaceSpan = 1024
	dstCoarseCwdBase         = 2049
	dstCoarseCwdSpan         = 1024
	dstCoarseFDBase          = 3073
	dstCoarseFDSpan          = 512
)

func dstCoarseNamespaceForHost(host uint32) uintptr {
	return dstCoarseFSNamespaceBase + uintptr(host)%dstCoarseFSNamespaceSpan
}

func dstCoarseNamespaceID() uintptr {
	host, _ := dstFSCurrentNode()
	return dstCoarseNamespaceForHost(host)
}

func dstCoarseCwdID() uintptr {
	host, proc := dstFSCurrentNode()
	return dstCoarseCwdBase + (uintptr(host)*31+uintptr(proc))%dstCoarseCwdSpan
}

func dstCoarseFDID(fd int) uintptr {
	return dstCoarseFDBase + uintptr(uint(fd))%dstCoarseFDSpan
}

func dstCoarseNodeRecord(node *dstFSNode, write bool) {
	if node == nil || node.isDevice() {
		return
	}
	dstCoarseRecord(uintptr(unsafe.Pointer(node)), write)
}

func (d *dstFile) coarseHandleOnly(write bool) {
	if !dstFSActive() {
		return
	}
	dstCoarseDep(uintptr(unsafe.Pointer(d)), write)
}

// coarseOp announces this open file handle and, when live on a non-device
// inode, its backing node and host disk in a single scheduling step
// (dstCoarseDep + dstCoarseRecord). Handle-state mutators (read/write/seek/
// readdir/close) pass handleWrite=true so concurrent cursor/close operations on
// one *os.File conflict while independent handles (d1 != d2) and concurrent
// pread calls on one handle still commute. Called BEFORE enter()/the tree lock.
func (d *dstFile) coarseOp(handleWrite, nodeWrite bool) {
	if !dstFSActive() {
		return
	}
	d.mu.Lock()
	node := d.node
	disk := d.disk
	closed := d.closed
	epochOk := d.epoch == dstFSEpoch()
	isDevice := node != nil && node.isDevice()
	d.mu.Unlock()
	dstCoarseDep(uintptr(unsafe.Pointer(d)), handleWrite)
	if !closed && epochOk && node != nil && !isDevice {
		dstCoarseRecord(uintptr(unsafe.Pointer(node)), nodeWrite)
		if disk != nil {
			dstCoarseRecord(uintptr(unsafe.Pointer(disk)), nodeWrite && disk.capped.Load())
		}
	}
}

// dstCoarseNamespaceDep announces the calling host's NAMESPACE as a
// coarse dependency identity for a named (path) op.
func dstCoarseNamespaceDep(write bool) {
	dstCoarseDep(dstCoarseNamespaceID(), write)
}

func dstCoarsePathDep(name string, write bool) {
	dstCoarseDep(dstCoarseNamespaceID(), write)
	if len(name) > 0 && name[0] != '/' {
		dstCoarseRecord(dstCoarseCwdID(), false)
	}
}

func dstCoarsePathDep2(a, b string, write bool) {
	dstCoarseDep(dstCoarseNamespaceID(), write)
	if (len(a) > 0 && a[0] != '/') || (len(b) > 0 && b[0] != '/') {
		dstCoarseRecord(dstCoarseCwdID(), false)
	}
}

// diskDelay sleeps this handle's disk's per-op latency before a disk-touching op (a
// slow disk). It reads the latency lock-free and sleeps OUTSIDE the tree lock — the
// op takes dstFS.mu afterward — so a slow disk on one host never blocks another's
// filesystem. The gate makes the no-fault path a single atomic load. A closed handle
// is skipped — a closed fd returns EBADF without touching the disk — and so is a
// device handle (/dev/null is not on the disk): a delay on either would be one a
// real slow disk never imposes (DST-FAULT-SOUND).
func (d *dstFile) diskDelay() {
	dstKernelSectionEnter()
	if !dstDiskSlow.Load() {
		dstKernelSectionExit()
		return
	}
	lat := d.disk.latency.Load()
	if lat <= 0 {
		dstKernelSectionExit()
		return
	}
	d.mu.Lock()
	skip := d.closed || !dstFSActive() || d.epoch != dstFSEpoch() || (d.node != nil && d.node.isDevice())
	d.mu.Unlock()
	dstKernelSectionExit()
	if !skip {
		time.Sleep(time.Duration(lat))
	}
}

func dstDiskDelayPath(name string) {
	dstKernelSectionEnter()
	slow := dstDiskSlow.Load()
	dstKernelSectionExit()
	if slow && !dstProcReserved(name) {
		dstDiskDelayHere()
	}
}

func dstDiskDelayPath2(a, b string) {
	dstKernelSectionEnter()
	slow := dstDiskSlow.Load()
	dstKernelSectionExit()
	if slow && !dstProcReserved(a) && !dstProcReserved(b) {
		dstDiskDelayHere()
	}
}

// dstDiskDelayHere is diskDelay for a named (path) op, which has no handle: it sleeps
// the calling host's disk latency before the op takes the tree lock. The brief lock
// is only to look up the host's disk (a map read); the sleep happens after releasing
// it. Gated like diskDelay so an unslowed run pays only the atomic load.
func dstDiskDelayHere() {
	dstKernelSectionEnter()
	if !dstDiskSlow.Load() {
		dstKernelSectionExit()
		return
	}
	dstFS.mu.Lock()
	dstFSRoll()
	lat := dstFSDiskHere().latency.Load()
	dstFS.mu.Unlock()
	dstKernelSectionExit()
	if lat > 0 {
		time.Sleep(time.Duration(lat))
	}
}

// diskEIO returns syscall.EIO if this handle's disk is under a host-wide EIO fault
// or this file's node is under a per-file EIO fault, else nil. Caller holds
// dstFS.mu. Checked only at the calls a real disk can fail with EIO (read / write /
// sync), never at an infallible call (seek, in-memory stat) — DST-FAULT-SOUND.
func (d *dstFile) diskEIOOp(op uint8) error {
	if d.disk.eio || d.disk.eioFiles[d.node] {
		dstRecordCausalCaller(dstCausalFSEIO, op, 0, 0, 0, 0, d.path)
		return syscall.EIO
	}
	return nil
}

// syncEIOOp returns syscall.EIO if a sync of this handle must fail: any fault
// diskEIOOp reports, or the host's writeback fault (wbFail). Writeback failure
// fails ONLY here — a failing medium under a live page cache still serves
// buffered reads and writes (the cache needs no device op, and this model
// never evicts), so the read/write chokes consult diskEIOOp alone; injecting
// EIO there under wbFail would surface an error no real kernel produces at a
// cache-served call (DST-FAULT-SOUND). Caller holds dstFS.mu.
func (d *dstFile) syncEIOOp(op uint8) error {
	if d.disk.wbFail {
		dstRecordCausalCaller(dstCausalFSEIO, op, 0, 0, 0, 0, d.path)
		return syscall.EIO
	}
	return d.diskEIOOp(op)
}

// syncMmapBlocksLocked folds non-zero pages dirtied through a PROT_WRITE
// mapping into node.blocks, skipping kernel holes via SEEK_DATA/SEEK_HOLE and
// already-recorded spans in O(extents). On Linux shmem, a read fault on a
// MAP_SHARED hole can instantiate a zeroed page in the memfd's radix tree; the
// !dstBytesAllZero check ensures reads never charge disk blocks or trigger
// ENOSPC (the only uncharged case is an mmap store of 0x00 into a hole, which
// errs in the sound <=-real direction). Caller holds dstFS.mu.
func (node *dstFSNode) syncMmapBlocksLocked() {
	if node.pc == nil || !node.everMappedWrite.Load() {
		return
	}
	size := int64(len(node.data))
	if size == 0 {
		return
	}
	for off := int64(0); off < size; {
		d, h, ok := dstPageCacheNextData(node.pc.fd, off, size)
		if !ok {
			break
		}
		p0 := d / dstPageSize
		p1 := (h + dstPageSize - 1) / dstPageSize
		for pg := p0; pg < p1; {
			if allocEnd := dstBlocksSpanEndAt(node.blocks, pg); allocEnd > pg {
				pg = allocEnd
				continue
			}
			pStart := pg * dstPageSize
			pEnd := min(pStart+dstPageSize, size)
			if !dstBytesAllZero(node.data[pStart:pEnd]) {
				node.blocks = dstBlocksAdd(node.blocks, pg, pg+1)
			}
			pg++
		}
		off = h
	}
}

func (node *dstFSNode) allocatedPagesLocked() int64 {
	if node.isDir || node.isDevice() {
		return 0
	}
	node.syncMmapBlocksLocked()
	return dstBlocksCount(node.blocks)
}

// residentPagesLocked sums the allocated 4 KB pages across all reachable
// regular files on the disk. Sparse holes (from truncate-grow) cost 0 blocks
// until written, preallocated via fallocate, or dirtied through PROT_WRITE
// mmap. Summed on demand so unlinking or truncating down frees blocks
// immediately; identity tracking counts hard-linked inodes once, and
// removed-but-open files are omitted (which only ever under-counts space in use
// — never a false ENOSPC). Caller holds dstFS.mu.
func (disk *dstFSDisk) residentPagesLocked() int64 {
	pages, _ := disk.usageLocked(false)
	return int64(pages)
}

func (disk *dstFSDisk) usageLocked(recordDep bool) (pages, inodes uint64) {
	seen := make(map[*dstFSNode]bool)
	var walk func(n *dstFSNode)
	walk = func(n *dstFSNode) {
		if n == nil || seen[n] {
			return
		}
		seen[n] = true
		inodes++
		if n.isDir {
			for _, c := range n.entries {
				walk(c)
			}
			return
		}
		if recordDep {
			dstCoarseNodeRecord(n, false)
		}
		pages += uint64(n.allocatedPagesLocked())
	}
	walk(disk.root)
	return pages, inodes
}

func (disk *dstFSDisk) freeBlocksLocked() int64 {
	if disk.capacity <= 0 {
		return 0
	}
	capPages := disk.capacity / dstPageSize
	usedPages := disk.residentPagesLocked()
	if usedPages >= capPages {
		return 0
	}
	return capPages - usedPages
}

// writeAllowed returns how many of the n bytes a write at off may store before
// EFBIG or the disk's 4 KB block capacity (ENOSPC) binds. In-place overwrites
// of already-allocated 4 KB blocks consume zero free blocks; writes into sparse
// holes or past EOF charge 1 block per newly touched 4 KB page, clipping at the
// exact 4 KB block boundary when remaining free blocks run out mid-write.
// Caller holds dstFS.mu.
func (d *dstFile) writeAllowed(off, n int64) (int64, syscall.Errno) {
	// generic_write_checks first: a write starting at or past s_maxbytes is
	// EFBIG outright; one crossing it is clipped, and the caller's retry at
	// the boundary surfaces EFBIG — checked before the capacity clip, as
	// the vfs does, so EFBIG wins when both boundaries bind at one offset.
	if off >= dstSMaxBytes {
		return 0, syscall.EFBIG
	}
	refuse := syscall.EFBIG
	if room := dstSMaxBytes - off; n > room {
		n = room
	}
	if n <= 0 || !d.disk.capped.Load() {
		return n, refuse
	}
	d.node.syncMmapBlocksLocked()
	startPg := off / dstPageSize
	endPg := (off+n-1)/dstPageSize + 1
	need := dstBlocksUnallocatedIn(d.node.blocks, startPg, endPg)
	if need == 0 {
		return n, refuse
	}
	free := d.disk.freeBlocksLocked()
	if need <= free {
		return n, refuse
	}
	rem := free
	for pg := startPg; pg < endPg; {
		if allocEnd := dstBlocksSpanEndAt(d.node.blocks, pg); allocEnd > pg {
			pg = min(allocEnd, endPg)
			continue
		}
		nextAlloc := dstBlocksNextStartAfter(d.node.blocks, pg, endPg)
		holeLen := nextAlloc - pg
		if rem < holeLen {
			stopOff := (pg + rem) * dstPageSize
			if stopOff <= off {
				return 0, syscall.ENOSPC
			}
			return stopOff - off, syscall.ENOSPC
		}
		rem -= holeLen
		pg = nextAlloc
	}
	return n, refuse
}

// diskFullForCreate reports whether a capped disk has zero free 4 KB blocks
// left (ENOSPC on create/mkdir on a full disk). Caller holds dstFS.mu.
func (disk *dstFSDisk) diskFullForCreate() bool {
	return disk.capped.Load() && disk.freeBlocksLocked() == 0
}

func (d *dstFile) read(b []byte) (n int, err error) {
	d.coarseOp(true, false)
	d.diskDelay()
	var kbuf []byte
	err = func() error {
		if err := d.enter(); err != nil {
			return err
		}
		defer d.leave()
		if len(b) == 0 {
			// Upstream poll.FD.Read returns (0, nil) for empty buffers before
			// the access-mode and EOF checks; mirror that order exactly.
			return nil
		}
		if !d.rd {
			return syscall.EBADF
		}
		if d.node.isDir {
			return syscall.EISDIR
		}
		if d.node.isDevice() {
			// /dev/null: always empty — but the wrong-direction EBADF above
			// still wins, as on the host. No diskEIO: the device is not on the
			// disk.
			return io.EOF
		}
		if err := d.diskEIOOp(dstFSOpRead); err != nil {
			return err
		}
		if d.off >= int64(len(d.node.data)) {
			dstKernelRaceAcquire(unsafe.Pointer(&d.node.dataSync))
			return io.EOF
		}
		avail := min(int64(len(b)), int64(len(d.node.data))-d.off)
		kbuf = make([]byte, avail)
		copy(kbuf, d.node.data[d.off:d.off+avail])
		d.off += avail
		dstKernelRaceAcquire(unsafe.Pointer(&d.node.dataSync))
		return nil
	}()
	if len(kbuf) > 0 {
		n = copy(b, kbuf)
	}
	return n, err
}

func (d *dstFile) pread(b []byte, off int64) (n int, err error) {
	d.coarseOp(false, false)
	d.diskDelay()
	var kbuf []byte
	err = func() error {
		if err := d.enter(); err != nil {
			return err
		}
		defer d.leave()
		// No empty-buffer early return (unlike read): pread is reached only from
		// File.ReadAt's `for len(b) > 0` loop, so b is never empty here.
		if !d.rd {
			return syscall.EBADF
		}
		if d.node.isDir {
			return syscall.EISDIR
		}
		if d.node.isDevice() {
			// /dev/null: EOF at every offset (host-verified via ReadAt).
			return io.EOF
		}
		if err := d.diskEIOOp(dstFSOpPread); err != nil {
			return err
		}
		if off >= int64(len(d.node.data)) {
			dstKernelRaceAcquire(unsafe.Pointer(&d.node.dataSync))
			return io.EOF
		}
		avail := min(int64(len(b)), int64(len(d.node.data))-off)
		if avail > 0 {
			kbuf = make([]byte, avail)
			copy(kbuf, d.node.data[off:off+avail])
		}
		dstKernelRaceAcquire(unsafe.Pointer(&d.node.dataSync))
		return nil
	}()
	if len(kbuf) > 0 {
		n = copy(b, kbuf)
	}
	return n, err
}

// writePreflight checks immutable/monotonic handle gates before copying from
// the caller's slice, so closed/read-only handles and /dev/null never touch or
// allocate for b.
func (d *dstFile) writePreflight(n int) (done bool, written int, err error) {
	if err := d.enter(); err != nil {
		return true, 0, err
	}
	defer d.leave()
	if !d.wr {
		return true, 0, syscall.EBADF
	}
	if d.node.isDevice() {
		// /dev/null: the write discards its bytes and reports the full
		// count. No disk coupling (EIO/ENOSPC/latency), no mtime movement,
		// no O_SYNC commit — the device stores nothing and is not on the
		// disk (all host-verified, including the unchanged mtime).
		return true, n, nil
	}
	return false, 0, nil
}

func (d *dstFile) write(b []byte) (int, error) {
	return d.writeOp(b, true)
}

func (d *dstFile) writeOp(b []byte, combineShortErr bool) (n int, err error) {
	d.coarseOp(true, true)
	d.diskDelay()
	if done, wn, preErr := d.writePreflight(len(b)); done {
		return wn, preErr
	}
	var kbuf []byte
	if len(b) > 0 {
		take := min(int64(len(b)), dstSMaxBytes)
		kbuf = make([]byte, take)
		copy(kbuf, b[:take])
	}
	if err := d.enter(); err != nil {
		return 0, err
	}
	defer d.leave()
	// The write position is a COPY of the handle offset, mirroring the
	// kernel: ksys_write hands vfs_write a copy of f_pos and stores it back
	// only for a non-negative return, so every error exit — EFBIG, EIO, a
	// fully-refused ENOSPC, a failed O_SYNC writeback — leaves d.off at its
	// exact pre-call value. For an O_APPEND handle that is wherever a prior
	// seek/read left it, NOT the end-of-file the append repositions the copy
	// to internally.
	pos := d.off
	if d.app {
		pos = int64(len(d.node.data))
	}
	if pos >= dstSMaxBytes && len(b) > 0 {
		// generic_write_checks precedes any device submission: at or past
		// the bound the answer is EFBIG even on a disk whose injected fault
		// would EIO a write that never gets submitted. The clip-binding
		// PARTIAL keeps EIO precedence — its bytes do submit.
		return 0, syscall.EFBIG
	}
	if err := d.diskEIOOp(dstFSOpWrite); err != nil {
		return 0, err
	}
	allowed, refuse := d.writeAllowed(pos, int64(len(b)))
	if allowed == 0 && len(b) > 0 {
		if refuse == syscall.ENOSPC {
			dstRecordCausalCaller(dstCausalFSENOSPC, dstFSOpWrite, 0, 0, int64(len(b)), 0, d.path)
		}
		return 0, refuse
	}
	n, werr := d.writeAtLocked(kbuf[:allowed], pos)
	if n > 0 {
		dstKernelRaceReleaseMerge(unsafe.Pointer(&d.node.dataSync))
	}
	if werr != nil {
		return n, werr
	}
	if (d.osync || d.odsync) && n > 0 {
		// generic_write_sync fires only for ret > 0: a zero-length write, or
		// one the ENOSPC cap fully refused, syncs nothing on real Linux. A
		// PARTIAL write (0 < n < len(b), the disk filling mid-write) still
		// commits its n bytes, matching the kernel. Without the guard a crash
		// after Write(nil) on an O_SYNC handle would durably preserve prior
		// unsynced writes hardware could lose, narrowing the crash-tear surface.
		if d.disk.wbFail {
			// generic_write_sync fails under the writeback fault: the bytes
			// reached the page cache (the buffered write above succeeded and
			// its mutation stands) but the synchronous writeback got EIO,
			// which REPLACES the count — write(2) returns plain EIO, so the
			// caller's loop reports (0, EIO) with the data cached-but-not-
			// durable, exactly the kernel's shape. The failed write does NOT
			// advance the file offset either — d.off is stored only below,
			// on the non-negative outcomes, exactly as ksys_write stores
			// f_pos. The failed writeback drops the dirty pages (fsyncgate),
			// as in sync — file-wide, which is pessimistic relative to the
			// kernel's RANGED synchronous writeback but stays ⊆ real: the
			// background flusher is free to have attempted (and dropped)
			// every dirty page of the file during the fault window.
			d.node.markWritebackDroppedLocked()
			dstRecordCausalCaller(dstCausalFSEIO, dstFSOpWrite, 0, 0, 0, 0, d.path)
			dstRecordCausalCaller(dstCausalFSFsyncgateDrop, dstFSOpWrite, 0, 0, 0, 0, d.path)
			return 0, syscall.EIO
		}
		if d.osync {
			d.node.commitLocked()
		} else {
			d.node.commitDataLocked()
		}
	}
	// The non-negative outcomes — full success, and the partial fill whose
	// short count a real write(2) returns before the retry's ENOSPC — store
	// the advanced position back to the handle, as ksys_write does.
	d.off = pos + int64(n)
	if combineShortErr && n < len(b) {
		// The boundary bound mid-write: the remaining bytes fail with the
		// binding boundary's errno (ENOSPC for a filled disk, EFBIG at
		// s_maxbytes), reported together with the partial count in ONE call —
		// mirroring internal/poll.FD.Write's loop, which retries a short
		// kernel write and surfaces (n, err). Returning a bare short count
		// instead would let os.File.Write report io.ErrShortWrite, an error
		// identity a real regular-file write cannot produce, so the SUT's
		// errors.Is recovery would miss exactly the faulted write. Raw
		// syscall.Write (combineShortErr == false) models a single write(2),
		// returning (n, nil) without surfacing or logging ENOSPC unless a
		// follow-up call hits allowed == 0.
		if refuse == syscall.ENOSPC {
			dstRecordCausalCaller(dstCausalFSENOSPC, dstFSOpWrite, 0, int64(n), int64(len(b)), 0, d.path)
		}
		return n, refuse
	}
	return n, nil
}

func (d *dstFile) pwrite(b []byte, off int64) (n int, err error) {
	d.coarseOp(false, true)
	d.diskDelay()
	if done, wn, preErr := d.writePreflight(len(b)); done {
		return wn, preErr
	}
	var kbuf []byte
	if len(b) > 0 {
		take := min(int64(len(b)), dstSMaxBytes)
		kbuf = make([]byte, take)
		copy(kbuf, b[:take])
	}
	if err := d.enter(); err != nil {
		return 0, err
	}
	defer d.leave()
	if d.app {
		// Linux pwrite(2), BUGS section: on a file opened O_APPEND, pwrite
		// APPENDS to the end of the file regardless of the offset.
		// os.File.WriteAt refuses append handles at the os layer
		// (ErrWriteAtInAppendMode), so only the raw syscall.Pwrite surface
		// reaches here with one — and it must get the kernel's shape.
		off = int64(len(d.node.data))
	}
	if off >= dstSMaxBytes && len(b) > 0 {
		// As in write: generic_write_checks' EFBIG precedes any submission,
		// so it beats an injected device EIO for the outright case.
		return 0, syscall.EFBIG
	}
	if err := d.diskEIOOp(dstFSOpPwrite); err != nil {
		return 0, err
	}
	// pwrite models a SINGLE pwrite(2): os.File.WriteAt loops over it and adds the
	// count only after the error check, so a partial fill must return (n, nil) here
	// and let the loop surface ENOSPC on the next zero-byte call — returning a
	// combined (n, ENOSPC) would make WriteAt discard the n. (The single-call Write
	// path, by contrast, needs the combined return — see write.)
	allowed, refuse := d.writeAllowed(off, int64(len(b)))
	if allowed == 0 && len(b) > 0 {
		if refuse == syscall.ENOSPC {
			dstRecordCausalCaller(dstCausalFSENOSPC, dstFSOpPwrite, 0, 0, int64(len(b)), 0, d.path)
		}
		return 0, refuse
	}
	n, werr := d.writeAtLocked(kbuf[:allowed], off)
	if n > 0 {
		dstKernelRaceReleaseMerge(unsafe.Pointer(&d.node.dataSync))
	}
	if werr != nil {
		return n, werr
	}
	if (d.osync || d.odsync) && n > 0 {
		// generic_write_sync fires only for ret > 0 (see write). On this
		// path n == 0 is unreachable — a zero-length WriteAt is diverted
		// before the backend (dstFDPwrite's len==0 short-circuit) and a
		// fully-refused write already returned ENOSPC above — so the n > 0
		// term is defensive parity with write, not a live branch.
		if d.disk.wbFail {
			// generic_write_sync fails under the writeback fault, replacing
			// the count: pwrite(2) returns plain EIO with the bytes cached
			// but not durable, and the failed writeback drops the dirty
			// pages (fsyncgate) — as in write.
			d.node.markWritebackDroppedLocked()
			dstRecordCausalCaller(dstCausalFSEIO, dstFSOpPwrite, 0, 0, 0, 0, d.path)
			dstRecordCausalCaller(dstCausalFSFsyncgateDrop, dstFSOpPwrite, 0, 0, 0, 0, d.path)
			return 0, syscall.EIO
		}
		if d.osync {
			d.node.commitLocked()
		} else {
			d.node.commitDataLocked()
		}
	}
	return n, nil
}

// writeAtLocked extends-with-zeros as needed and copies b at off. Caller
// holds both locks. Mutates current content only (never the durable image).
// writeAtLocked's caller checked the offset is non-negative; the far bound is
// checked here: an offset so large the write's end overflows int64 is EFBIG on
// real Linux (the file size limit), where the overflowed arithmetic would have
// skipped the growth and indexed past the slice instead.
func (d *dstFile) writeAtLocked(b []byte, off int64) (int, error) {
	node := d.node
	if len(b) == 0 {
		// A write whose effective slice is empty — a zero-length write, or
		// one fully refused by the ENOSPC cap — has no effect at all: POSIX
		// gives a zero-length write(2) no side effects (no growth to the
		// offset, no mtime), and the refusal path must not grow the file it
		// just refused to write (zero-filling to the seek offset would also
		// count against the disk budget with no path to recover it —
		// the fault model's capacity invariant broken by its own refusal).
		return 0, nil
	}
	const maxInt64 = 1<<63 - 1
	if off > maxInt64-int64(len(b)) {
		return 0, syscall.EFBIG
	}
	if need := off + int64(len(b)); need > int64(len(node.data)) {
		dstNodeSetSizeLocked(node, need)
	}
	// A backed node's data IS the page cache, so the copy is the whole of the
	// write: every mapping of this file already sees it.
	n := copy(node.data[off:], b)
	startPg := off / dstPageSize
	endPg := (off+int64(n)-1)/dstPageSize + 1
	node.blocks = dstBlocksAdd(node.blocks, startPg, endPg)
	node.dstMarkRedirtiedLocked(off, int64(n))
	node.modTime = time.Now()
	return n, nil
}

func (d *dstFile) seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		// SEEK_END reads the shared file size (or directory entry count) —
		// the seek form whose result a peer's concurrent append/truncate/create
		// determines, while all seeks mutate this handle's cursor.
		d.coarseOp(true, false)
	} else {
		d.coarseHandleOnly(true)
	}
	if err := d.enter(); err != nil {
		return 0, err
	}
	defer d.leave()
	if whence == io.SeekEnd && d.node.isDir {
		dstCoarseRecord(dstCoarseNamespaceID(), false)
	}
	if d.node.isDevice() {
		// /dev/null: lseek succeeds and the position is pinned at 0 — every
		// whence, every offset, including negative ones that would be EINVAL
		// on a regular file (host-verified: null_lseek returns 0
		// unconditionally, and the file position never moves).
		return 0, nil
	}
	var base int64
	switch whence {
	case io.SeekStart:
		base = 0
	case io.SeekCurrent:
		if d.node.isDir {
			base = int64(d.dirpos) // the telldir idiom: lseek(fd, 0, SEEK_CUR) reads the cookie
		} else {
			base = d.off
		}
	case io.SeekEnd:
		if d.node.isDir {
			base = int64(len(d.node.entries)) // the cookie space's end: the entry count
		} else {
			base = int64(len(d.node.data))
		}
	default:
		return 0, syscall.EINVAL
	}
	pos := base + offset
	if pos < 0 {
		return 0, syscall.EINVAL
	}
	if d.node.isDir {
		// lseek on a directory fd is permitted at any offset, as Linux
		// permits: the offset is an opaque readdir cookie — here the
		// sorted-name index, the deterministic listing order the spec
		// promises — and getdents resumes there (readDirLocked clamps a
		// past-end position). Rewinding to 0 restarts the listing, the
		// rewinddir shape. The stored cursor clamps to the int range so a
		// 32-bit host cannot wrap it negative; the clamp is invisible (any
		// cookie past the entry count lists nothing).
		cookie := pos
		if maxInt := int64(^uint(0) >> 1); cookie > maxInt {
			cookie = maxInt
		}
		d.dirpos = int(cookie)
		return pos, nil
	}
	d.off = pos
	return pos, nil
}

// sortedEntryNames returns the directory's entry names sorted ascending.
// Caller holds both locks. Sorted order is the deterministic listing order
// the spec promises (and matches os.ReadDir's documented sorting).
func (d *dstFile) sortedEntryNamesLocked() []string {
	names := make([]string, 0, len(d.node.entries))
	for name := range d.node.entries {
		names = append(names, name)
	}
	for i := 1; i < len(names); i++ { // insertion sort: no new imports
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// readdir implements the File.Readdir/ReadDir/Readdirnames funnel for a
// simulated directory handle: sorted names with a stable cursor for chunked
// (n > 0) reads; io.EOF at exhaustion for chunked mode, as the host funnel.
func (d *dstFile) readdir(n int) (names []string, infos []FileInfo, err error) {
	d.coarseOp(true, false)
	if dstFSActive() {
		dstCoarseRecord(dstCoarseNamespaceID(), false)
	}
	d.diskDelay()
	if e := d.enter(); e != nil {
		return nil, nil, e
	}
	defer d.leave()
	if !d.node.isDir {
		return nil, nil, syscall.ENOTDIR
	}
	if d.node.unlinked {
		// Linux's iterate_dir refuses a dead directory (IS_DEADDIR):
		// getdents on an rmdir'd directory is ENOENT (host-probed on
		// tmpfs and ext alike), never an empty listing.
		return nil, nil, syscall.ENOENT
	}
	all := d.sortedEntryNamesLocked()
	if d.dirpos > len(all) {
		d.dirpos = len(all)
	}
	rest := all[d.dirpos:]
	if n > 0 {
		if len(rest) == 0 {
			return nil, nil, io.EOF
		}
		if len(rest) > n {
			rest = rest[:n]
		}
	}
	d.dirpos += len(rest)
	names = make([]string, 0, len(rest))
	names = append(names, rest...)
	infos = make([]FileInfo, 0, len(names))
	for _, name := range names {
		node := d.node.entries[name]
		if node == nil {
			continue // racing remove between snapshot and here is impossible under the lock; defensive
		}
		infos = append(infos, dstFileInfoForNode(name, node))
	}
	return names, infos, nil
}

// chdirHandle implements File.Chdir for a simulated directory handle.
func (d *dstFile) chdirHandle() error {
	d.coarseHandleOnly(false)
	if dstFSActive() {
		dstCoarseRecord(dstCoarseCwdID(), true)
	}
	if e := d.enter(); e != nil {
		return e
	}
	defer d.leave()
	if d.disk != dstFSDiskHere() {
		return syscall.EBADF
	}
	if !d.node.isDir {
		return syscall.ENOTDIR
	}
	dstFSSetCwdDentry(d.dentry)
	return nil
}

func (d *dstFile) truncate(size int64) error {
	d.coarseOp(false, true)
	d.diskDelay()
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
	if d.node.isDevice() {
		// ftruncate on a character device is EINVAL (host-verified).
		return syscall.EINVAL
	}
	return d.node.truncateLocked(size)
}

// fallocate backs fallocate(2) mode 0 — preallocation, the WAL warmed-segment
// idiom (allocate the file's full span up front, write into place, fdatasync).
// Mode 0 preallocates every unallocated 4 KB page in [off, off+len), growing
// the file with zeros if off+len exceeds EOF, and checks unallocated 4 KB pages
// against the disk capacity all-or-nothing first (a real fallocate's reason to
// exist is failing ENOSPC up front rather than mid-write; it allocates nothing
// when it refuses). Already-allocated pages in [off, off+len) consume zero
// additional blocks, while sparse holes inside EOF charge 1 block per hole page
// and persist in st_blocks across sync and crash restore. The growth and block
// allocation are durable only after a sync, and the crash tear's
// intermediate-size draws apply to growth. Gate order is vfs_fallocate's
// (fs/open.c, host-probed): arguments (EINVAL); then modes OUTSIDE
// FALLOC_FL_SUPPORTED_MASK (EOPNOTSUPP — the vfs's own pre-access gate);
// then write access (EBADF — which also covers directories, never
// writable) and file type (a device answers ENODEV); then IN-mask modes
// this filesystem does not implement (EOPNOTSUPP — the capability gate,
// which on a real kernel sits with the fs op, AFTER access: a read-only fd
// with KEEP_SIZE is EBADF, not EOPNOTSUPP); then the span check (EFBIG).
// Only mode 0 is modeled.
//
// dstFallocSupportedMask is FALLOC_FL_SUPPORTED_MASK: KEEP_SIZE |
// PUNCH_HOLE | COLLAPSE_RANGE | ZERO_RANGE | INSERT_RANGE | UNSHARE_RANGE.
// Bits outside it are refused before the access checks, as the vfs does.
const dstFallocSupportedMask = 0x7B

func (d *dstFile) fallocate(mode int, off, length int64) error {
	d.coarseOp(false, true)
	d.diskDelay()
	if err := d.enter(); err != nil {
		return err
	}
	defer d.leave()
	if off < 0 || length <= 0 {
		return syscall.EINVAL
	}
	if mode&^dstFallocSupportedMask != 0 {
		return syscall.EOPNOTSUPP
	}
	if !d.wr {
		return syscall.EBADF
	}
	if d.node.isDevice() {
		return syscall.ENODEV
	}
	if mode != 0 {
		return syscall.EOPNOTSUPP
	}
	newSize := off + length
	if newSize < 0 {
		// off and length are both non-negative here, so a negative sum is
		// int64 overflow: the span passes the filesystem's maximum file size.
		return syscall.EFBIG
	}
	if newSize > dstSMaxBytes {
		// vfs_fallocate checks offset+len against s_maxbytes before the
		// filesystem op — so EFBIG wins over ENOSPC when both bind.
		return syscall.EFBIG
	}
	if err := d.diskEIOOp(dstFSOpFallocate); err != nil {
		return err
	}
	d.node.syncMmapBlocksLocked()
	startPg := off / dstPageSize
	endPg := (newSize-1)/dstPageSize + 1
	need := dstBlocksUnallocatedIn(d.node.blocks, startPg, endPg)
	if d.disk.capped.Load() && need > d.disk.freeBlocksLocked() {
		dstRecordCausalCaller(dstCausalFSENOSPC, dstFSOpFallocate, 0, 0, length, 0, d.path)
		return syscall.ENOSPC // refused whole: a failed fallocate allocates nothing
	}
	if newSize > int64(len(d.node.data)) {
		if err := d.node.truncateLocked(newSize); err != nil {
			return err
		}
	}
	d.node.blocks = dstBlocksAdd(d.node.blocks, startPg, endPg)
	return nil
}

// sync commits the durable image (the durability contract's commit points):
// for a file, the current content and metadata; for a directory, the current
// entry set (entry durability is the parent directory's property — POSIX's
// fsync-the-file-then-fsync-the-directory model). Mutations never touch the
// durable image; on the mutation paths this is its only writer (plus
// O_SYNC's per-write commit, which routes through the same helper). The one
// other writer is the host-crash restore, which re-bases the durable image
// to the platter's post-crash state (dstRestoreNodeLocked).
func (d *dstFile) sync() error {
	d.coarseOp(false, true)
	d.diskDelay()
	if err := d.enter(); err != nil {
		return err
	}
	defer d.leave()
	if d.node.isDevice() {
		// fsync on a character device is EINVAL — devtmpfs supplies no fsync
		// op (host-verified). Nothing to commit: the device has no durable
		// image.
		return syscall.EINVAL
	}
	if err := d.syncEIOOp(dstFSOpFsync); err != nil {
		if !d.node.isDir {
			// fsyncgate: the failed writeback marks the file's dirty pages
			// clean without persisting them (see dstFSNode.wbDropped).
			// Directory entry writeback keeps the full-commit model — a
			// recorded bound: the drop is modeled for file DATA pages.
			d.node.markWritebackDroppedLocked()
			dstRecordCausalCaller(dstCausalFSFsyncgateDrop, dstFSOpFsync, 0, 0, 0, 0, d.path)
		}
		return err
	}
	d.node.commitLocked()
	return nil
}

func (d *dstFile) datasync() error {
	d.coarseOp(false, true)
	d.diskDelay()
	if err := d.enter(); err != nil {
		return err
	}
	defer d.leave()
	if d.node.isDir {
		// fdatasync on a directory commits entry durability exactly as
		// directory fsync does (host-verified: Linux fdatasync succeeds on a
		// directory fd — the create/rename-then-fdatasync-the-directory
		// durability idiom is production-legal). Directory entry writeback
		// keeps the full-commit model on EIO, as in sync.
		if err := d.syncEIOOp(dstFSOpFdatasync); err != nil {
			return err
		}
		d.node.commitLocked()
		return nil
	}
	if d.node.isDevice() {
		// fdatasync on a character device is EINVAL, as in sync
		// (host-verified).
		return syscall.EINVAL
	}
	if err := d.syncEIOOp(dstFSOpFdatasync); err != nil {
		// fsyncgate, as in sync.
		d.node.markWritebackDroppedLocked()
		dstRecordCausalCaller(dstCausalFSFsyncgateDrop, dstFSOpFdatasync, 0, 0, 0, 0, d.path)
		return err
	}
	d.node.commitDataLocked()
	return nil
}

// commitLocked advances the node's durable image to its current state.
// Caller holds dstFS.mu.
func (node *dstFSNode) commitLocked() {
	if node.isDir {
		old := node.syncedEntries
		if node.unlinked && node.durableRefs == 0 {
			node.syncedEntries = nil
		} else {
			next := make(map[string]*dstFSNode, len(node.entries))
			for name, child := range node.entries {
				next[name] = child
				dstNodeAddDurableRefLocked(child)
			}
			node.syncedEntries = next
		}
		for _, child := range old {
			dstNodeDropDurableRefLocked(child)
		}
	} else {
		node.commitDataLocked()
	}
	node.syncedMode = node.mode
	node.syncedModTime = node.modTime
}

// scanDataPagesLocked scans non-hole 4 KB pages of node.data via SEEK_DATA,
// skipping pages where skip != nil && skip(pg), and constructs a sparse COW
// page map reusing unchanged pages from node.synced.pages. Caller holds
// dstFS.mu and has synced mmap blocks.
func (node *dstFSNode) scanDataPagesLocked(skip func(int64) bool) map[int64]dstDurablePage {
	size := int64(len(node.data))
	if node.pc == nil || size == 0 {
		return nil
	}
	old := node.synced
	var pages map[int64]dstDurablePage
	for off := int64(0); off < size; {
		d, h, ok := dstPageCacheNextData(node.pc.fd, off, size)
		if !ok {
			break
		}
		p0 := d / dstPageSize
		p1 := (h + dstPageSize - 1) / dstPageSize
		for pg := p0; pg < p1; pg++ {
			if skip != nil && skip(pg) {
				continue
			}
			pStart := pg * dstPageSize
			pEnd := min(pStart+dstPageSize, size)
			p := dstDurablePageFromSlice(old.pages[pg], node.data[pStart:pEnd])
			if p == nil {
				continue
			}
			if pages == nil {
				pages = make(map[int64]dstDurablePage)
			}
			pages[pg] = p
		}
		off = h
	}
	return pages
}

func (node *dstFSNode) snapshotDataLocked() dstDurableImage {
	if node.pc == nil {
		return dstDurableImage{}
	}
	node.syncMmapBlocksLocked()
	size := int64(len(node.data))
	if size == 0 {
		return dstDurableImage{}
	}
	pages := node.scanDataPagesLocked(nil)
	maxPages := (size + dstPageSize - 1) / dstPageSize
	return dstDurableImage{
		size:   size,
		pages:  pages,
		blocks: dstBlocksTruncate(slices.Clone(node.blocks), maxPages),
	}
}

func (node *dstFSNode) isPageDroppedLocked(pg int64) bool {
	if pg >= node.wbDroppedPages {
		return false
	}
	if _, redirtied := node.wbRedirtied[pg]; redirtied {
		return false
	}
	if node.wbHasSnap && node.everMappedWrite.Load() {
		pStart := pg * dstPageSize
		if pStart >= int64(len(node.data)) {
			return false
		}
		n := int(min(int64(dstPageSize), int64(len(node.data))-pStart))
		cur := node.data[pStart : pStart+int64(n)]
		oldValid := int(max(0, min(int64(n), node.wbSnap.size-pStart)))
		snapPg := node.wbSnap.pages[pg]
		var unchanged bool
		if snapPg == nil {
			unchanged = dstBytesAllZero(cur)
		} else {
			unchanged = slices.Equal(cur[:oldValid], snapPg[:oldValid]) && dstBytesAllZero(cur[oldValid:])
		}
		if !unchanged {
			node.dstMarkPageRedirtiedLocked(pg)
			return false
		}
	}
	return true
}

func (node *dstFSNode) commitDataLocked() {
	if node.pc == nil {
		return
	}
	node.syncMmapBlocksLocked()
	old := node.synced
	size := int64(len(node.data))
	pages := node.scanDataPagesLocked(node.isPageDroppedLocked)
	if node.wbDroppedPages > 0 {
		for pg, oldPg := range old.pages {
			if pg*dstPageSize >= size || !node.isPageDroppedLocked(pg) {
				continue
			}
			limit := int(min(int64(dstPageSize), size-pg*dstPageSize))
			if clipped := dstDurablePageClipped(oldPg, limit); clipped != nil {
				if pages == nil {
					pages = make(map[int64]dstDurablePage)
				}
				pages[pg] = clipped
			}
		}
	}
	var blocks []dstBlockSpan
	maxPages := (size + dstPageSize - 1) / dstPageSize
	if node.wbDroppedPages == 0 {
		blocks = dstBlocksTruncate(slices.Clone(node.blocks), maxPages)
	} else {
		// Merge non-dropped current block spans with dropped durable block spans.
		for _, s := range node.blocks {
			for pg := s.start; pg < min(s.end, maxPages); {
				if !node.isPageDroppedLocked(pg) {
					end := pg + 1
					for end < min(s.end, maxPages) && !node.isPageDroppedLocked(end) {
						end++
					}
					blocks = dstBlocksAdd(blocks, pg, end)
					pg = end
				} else {
					pg++
				}
			}
		}
		for _, s := range old.blocks {
			for pg := s.start; pg < min(s.end, maxPages); pg++ {
				if node.isPageDroppedLocked(pg) {
					blocks = dstBlocksAdd(blocks, pg, pg+1)
				}
			}
		}
	}
	node.synced = dstDurableImage{
		size:   size,
		pages:  pages,
		blocks: blocks,
	}
	node.clearRotWrittenBackLocked(old)
}

// clearRotWrittenBackLocked drops the rot masks writeback just healed: a sync
// that changed a page's committed bytes rewrote that page's sectors, so rot
// under them is gone (dstFSNode.rot — the diff against the pre-commit image is
// the dirty proxy; fsyncgate-dropped pages kept their old bytes above, so
// their rot survives by the same comparison). Masks past the new image fall
// off the platter's readable surface (a shrink freed the i_size range) and are
// dropped with it. Caller holds dstFS.mu; old is the pre-commit durable image.
func (node *dstFSNode) clearRotWrittenBackLocked(old dstDurableImage) {
	if node.rot == nil {
		return
	}
	size := node.synced.size
	for off := range node.rot {
		if off >= size {
			delete(node.rot, off)
			continue
		}
		pg := off / dstPageSize
		pStart := pg * dstPageSize
		n := int(min(int64(dstPageSize), size-pStart))
		oldValid := int(max(0, min(int64(n), old.size-pStart)))
		if !dstDurablePageEqualAt(node.synced.pages[pg], n, old.pages[pg], oldValid, n) {
			delete(node.rot, off)
		}
	}
	if len(node.rot) == 0 {
		node.rot = nil
	}
}

func (node *dstFSNode) clearWritebackDroppedLocked() {
	node.wbDroppedPages = 0
	node.wbRedirtied = nil
	node.wbSnap = dstDurableImage{}
	node.wbHasSnap = false
}

func (node *dstFSNode) ensureWritebackSnapLocked() {
	if node.wbDroppedPages > 0 && !node.wbHasSnap {
		node.wbSnap = node.snapshotDataLocked()
		node.wbHasSnap = true
	}
}

// markWritebackDroppedLocked records the fsyncgate mark: every page in
// [0, ceil(len(data)/4096)) is dropped until a write or MAP_SHARED store
// redirties it. Caller holds dstFS.mu.
func (node *dstFSNode) markWritebackDroppedLocked() {
	node.syncMmapBlocksLocked()
	node.wbDroppedPages = (int64(len(node.data)) + dstPageSize - 1) / dstPageSize
	clear(node.wbRedirtied)
	if node.wbDroppedPages > 0 && node.everMappedWrite.Load() {
		node.wbSnap = node.snapshotDataLocked()
		node.wbHasSnap = true
	} else {
		node.wbSnap = dstDurableImage{}
		node.wbHasSnap = false
	}
}

func (node *dstFSNode) dstMarkPageRedirtiedLocked(pg int64) {
	if pg < 0 || pg >= node.wbDroppedPages {
		return
	}
	if node.wbRedirtied == nil {
		node.wbRedirtied = make(map[int64]struct{})
	}
	node.wbRedirtied[pg] = struct{}{}
	if int64(len(node.wbRedirtied)) == node.wbDroppedPages {
		node.clearWritebackDroppedLocked()
	}
}

// dstMarkRedirtiedLocked removes the pages [off, off+n) touch from the
// dropped set: a write event redirties them, so the next successful sync
// writes them back. Caller holds dstFS.mu.
func (node *dstFSNode) dstMarkRedirtiedLocked(off, n int64) {
	if node.wbDroppedPages == 0 || n <= 0 {
		return
	}
	p0 := off / dstPageSize
	p1 := min((off+n-1)/dstPageSize, node.wbDroppedPages-1)
	if p0 > p1 {
		return
	}
	for pg := p0; pg <= p1; pg++ {
		node.dstMarkPageRedirtiedLocked(pg)
		if node.wbDroppedPages == 0 {
			return
		}
	}
}

func dstFileInfoForNode(name string, node *dstFSNode) *dstFileInfo {
	return &dstFileInfo{
		name:    name,
		size:    int64(len(node.data)),
		mode:    node.mode,
		modTime: node.modTime,
		isDir:   node.isDir,
		ident:   node,
		ino:     node.ino,
		nlink:   node.nlink,
		blocks:  node.allocatedPagesLocked() * (dstPageSize / 512),
	}
}

func (d *dstFile) stat() (FileInfo, error) {
	// fstat reads shared node state (size, mtime) a peer's write moves —
	// a coarse read-conflict on the node (no disk delay: fstat stays an
	// in-core read; the announce is a dependency record, not a device op).
	d.coarseOp(false, false)
	if err := d.enter(); err != nil {
		return nil, err
	}
	defer d.leave()
	return dstFileInfoForNode(filepathlite.Base(d.path), d.node), nil
}

// setDeadline implements the deadline half of the backend seam for tree
// files: the host's regular files and directories are not pollable, so
// SetDeadline fails with the (unwrapped) ErrNoDeadline shape there, and the
// simulated tree mirrors that — including the host's precedence, where a
// closed handle reports the (also unwrapped) closed shape first. Pipes are
// the pollable case — see dst_pipe.go.
func (d *dstFile) setDeadline(rd, wd bool, t time.Time) error {
	d.coarseHandleOnly(false)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || !dstFSActive() || d.epoch != dstFSEpoch() {
		if d.closed && dstFSActive() && d.epoch == dstFSEpoch() {
			dstKernelRaceAcquire(unsafe.Pointer(&d.closeSync))
		}
		return poll.ErrFileClosing
	}
	return poll.ErrNoDeadline
}

// closeFile closes the handle. The node stays alive while other handles or
// directory entries reference it.
func (d *dstFile) closeFile() error {
	d.coarseHandleOnly(true)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		if dstFSActive() && d.epoch == dstFSEpoch() {
			dstKernelRaceAcquire(unsafe.Pointer(&d.closeSync))
		}
		return poll.ErrFileClosing
	}
	d.closed = true
	if dstFSActive() && d.epoch == dstFSEpoch() {
		dstKernelRaceReleaseMerge(unsafe.Pointer(&d.closeSync))
	}
	return nil
}

func (d *dstFile) dropClosedNode() {
	d.mu.Lock()
	var node *dstFSNode
	var epoch uint64
	if d.closed && d.node != nil {
		node = d.node
		epoch = d.epoch
		d.node = nil
		d.dentry = nil
	}
	d.mu.Unlock()
	if node == nil || node.isDir || node.isDevice() {
		return
	}
	dstFS.mu.Lock()
	if dstFS.disks != nil && epoch == dstFSEpoch() {
		node.openFiles--
		dstNodeMaybeReleaseLocked(node)
	}
	dstFS.mu.Unlock()
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
	ident   any // identity for SameFile: *dstFSNode, or *dstPipe (both ends share it, as both host fds share one pipe inode)
	ino     uint64
	nlink   int
	blocks  int64
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
	return ok1 && ok2 && d1.ident == d2.ident && d1.ident != nil, true
}

func (fi *dstFileInfo) Name() string       { return fi.name }
func (fi *dstFileInfo) Size() int64        { return fi.size }
func (fi *dstFileInfo) Mode() FileMode     { return fi.mode }
func (fi *dstFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *dstFileInfo) IsDir() bool        { return fi.isDir }
func (fi *dstFileInfo) Sys() any           { return nil }
