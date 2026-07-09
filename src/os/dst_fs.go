// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import (
	"cmp"
	"internal/poll"
	"io"
	"path"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
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

// dstFS is the process-global simulated filesystem. The tree is per HOST: each
// host (testing/simulation.Host) owns its own root, so co-located processes share a
// filesystem while different hosts are isolated — process A on host hA cannot
// observe or mutate host hB's files except over the simulated network
// (DST-NODE-ISOLATION). The working directory is per PROCESS: a path into the
// process's host tree, so one process's Chdir does not move another's even on the
// same host. Both maps are keyed by the run epoch (reset in dstFSRoll) so a new run
// starts fresh with no teardown hook. The default host/process (id 0 — a program
// that declares neither) is one host, one process, identical to a plain run. All
// node state is guarded by mu; per-handle state by the handle's own mutex, acquired
// before mu where both are needed.
var dstFS struct {
	mu      sync.Mutex
	epoch   uint64
	disks   map[uint32]*dstFSDisk // host id -> the host's in-memory tree
	cwds    map[[2]uint32]string  // (host id, process id) -> that process's working directory into its host tree
	nextIno uint64                // last synthetic inode number handed out (dstFSAllocIno)
}

var dstOpenFiles struct {
	mu    sync.Mutex
	epoch uint64
	files map[*file]dstOpenFileEntry
	seq   uint64 // per-run registration counter (see dstOpenFileEntry.seq)
}

type dstOpenFileEntry struct {
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

func dstRegisterOpenFile(f *file, proc uint32) {
	dstOpenFiles.mu.Lock()
	dstOpenFilesRollLocked()
	dstOpenFiles.seq++
	dstOpenFiles.files[f] = dstOpenFileEntry{proc: proc, seq: dstOpenFiles.seq}
	dstOpenFiles.mu.Unlock()
}

func dstUnregisterOpenFile(f *file) {
	dstOpenFiles.mu.Lock()
	dstOpenFilesRollLocked()
	delete(dstOpenFiles.files, f)
	dstOpenFiles.mu.Unlock()
}

func dstCloseProcFiles(proc uint32) {
	dstOpenFiles.mu.Lock()
	dstOpenFilesRollLocked()
	type victim struct {
		f   *file
		seq uint64
	}
	var victims []victim
	for f, entry := range dstOpenFiles.files {
		if entry.proc == proc {
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
		if f.dstf == nil {
			continue
		}
		_ = f.dstf.closeFile()
		dstReleaseFD(f)
		dstDropClosedNode(f.dstf)
	}
}

// dstFSDisk is one host's tree (its filesystem). The durability state lives on the
// nodes; a host's disk is what a host (power-loss) crash later restores.
//
// Disk faults are policy on the disk itself (one source of truth, reset with the
// disk when the run epoch rolls — no separate teardown): eio fails the whole disk's
// I/O with EIO, and eioFiles fails just the listed nodes (a bad sector on one
// file). A node, not a path, is the per-file key, so a faulted file stays faulted
// across a rename and a removed-but-open handle keeps failing — the physical
// bad-block semantics. capped/capacity model a full disk: writes that would grow the
// disk past capacity, and creates on an already-full disk, fail with ENOSPC. The
// space in use is summed on demand from the live tree (residentLocked), not tracked
// incrementally, so a delete or truncate-down frees space for the next write with no
// accounting threaded through every mutation — and never a false ENOSPC a
// budget-that-ignores-frees would produce. latency models a slow disk: a per-op
// delay (nanoseconds) the calling goroutine sleeps before each disk-touching op. It
// is atomic, not under dstFS.mu, because the delay is read and slept on BEFORE the op
// takes the tree lock — sleeping while holding dstFS.mu would freeze every host's
// filesystem, not just the slow one. (eio/capacity are read inside the op under the
// lock; only latency needs the lock-free read.) Set through the runtime relay (see
// dst_disk_fault.go).
type dstFSDisk struct {
	root     *dstFSNode
	eio      bool                // host-disk EIO: every read/write/sync on this disk fails EIO
	eioFiles map[*dstFSNode]bool // per-file EIO: just these nodes fail (a bad sector)
	capped   bool                // whether a capacity (full-disk / ENOSPC) limit is set
	capacity int64               // max total regular-file bytes when capped
	latency  atomic.Int64        // slow-disk: per-op delay in nanoseconds (0 = none)
}

// dstDiskSlow gates the per-op latency delay: true once any host's disk has a latency
// set this run, so the no-fault path is a single atomic load with no lock. Reset when
// the run epoch rolls (dstFSRoll).
var dstDiskSlow atomic.Bool

//go:linkname dstFSCurrentNode runtime.dstCurrentNode
func dstFSCurrentNode() (host, proc uint32)

// dstFSRoll resets the per-run filesystem state when the run epoch advances. Caller
// holds dstFS.mu. Per-host trees are created lazily (dstFSDiskHere), so Roll only
// clears the maps.
func dstFSRoll() {
	if e := dstFSEpoch(); e != dstFS.epoch || dstFS.disks == nil {
		dstFS.epoch = e
		dstFS.disks = make(map[uint32]*dstFSDisk)
		dstFS.cwds = make(map[[2]uint32]string)
		dstFS.nextIno = 1
		// New run: no host is slow until a SlowDisk fault re-arms the gate (the
		// per-disk latency resets with the disks above).
		dstDiskSlow.Store(false)
	}
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

// newDstFSDisk builds a fresh host tree: an empty root containing only /tmp (mode
// 1777), so TempDir-based CreateTemp/MkdirTemp work unmodified (the spec's
// empty-tree clause). Every host gets its own /tmp; os.TempDir reports the fixed
// path "/tmp" during a run.
func newDstFSDisk() *dstFSDisk {
	root := &dstFSNode{
		isDir:   true,
		ino:     dstFSAllocIno(),
		entries: make(map[string]*dstFSNode),
		mode:    ModeDir | 0o755,
		modTime: time.Now(),
	}
	root.entries["tmp"] = &dstFSNode{
		isDir:   true,
		ino:     dstFSAllocIno(),
		entries: make(map[string]*dstFSNode),
		mode:    ModeDir | ModeSticky | 0o777,
		modTime: time.Now(),
	}
	return &dstFSDisk{root: root}
}

// dstFSCwdHere returns the calling process's working directory (default "/");
// dstFSSetCwd sets it. The key is the (host, process) pair — the real process
// identity — because a process id is not unique across hosts: a Host with no
// Process runs at process id 0 on every host, and a same-named Process on two hosts
// shares a process id, yet they are different machines whose cwds must be
// independent (DST-NODE-ISOLATION). Caller holds dstFS.mu.
func dstFSCwdHere() string {
	host, proc := dstFSCurrentNode()
	if c, ok := dstFS.cwds[[2]uint32{host, proc}]; ok {
		return c
	}
	return "/"
}

func dstFSSetCwd(cwd string) {
	host, proc := dstFSCurrentNode()
	dstFS.cwds[[2]uint32{host, proc}] = cwd
}

// dstFSIsRoot reports whether node is the calling goroutine's host-tree root (which
// cannot be removed or renamed). Caller holds dstFS.mu.
func dstFSIsRoot(node *dstFSNode) bool {
	return node == dstFSDiskHere().root
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

	// ino is the node's synthetic inode number, allocated at creation from the
	// per-run counter (dstFSAllocIno) — schedule-deterministic, never reused
	// within a run. It is the file-identity half of fstat(2)'s (st_dev, st_ino)
	// pair (st_dev derives from the host id), which inode-keyed SUTs (the
	// SQLite/LMDB per-file lock-dedup pattern) require to distinguish files. It
	// rides the node: stable across rename and while unlinked-but-open.
	ino uint64

	// unlinked marks a directory removed from the namespace (Remove/RemoveAll,
	// or replaced by Rename). The kernel fails entry CREATION in an rmdir'd
	// directory with ENOENT even through a still-open handle (a Root captured
	// before the removal); lookups need no flag — an unlinked dir is empty.
	unlinked bool

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

	// Advisory flock state. This is host file-node state, not durable file
	// content; fd and process ownership live in the lock owner key.
	flock dstFlockState
}

type dstFlockOwner struct {
	host uint32
	proc uint32
	fd   int
}

type dstFlockState struct {
	holders map[dstFlockOwner]bool // true means exclusive, false means shared
	wait    chan struct{}
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
	root := dstFSDiskHere().root
	comps, trailingSlash := dstFSComponents(name)
	// stack holds the directory chain from root; names[i] is stack[i]'s name in its
	// parent (names[0] is the sentinel "/"). A `..` pops toward root; at root it
	// stays (POSIX: `/..` == `/`).
	stack := []*dstFSNode{root}
	names := []string{"/"}
	terminalDir := func() (parent *dstFSNode, base string, node *dstFSNode, errno error) {
		// A path ending in `.`/`..` (or all-slashes) resolves to the current dir. A
		// trailing slash on a directory is fine.
		cur := stack[len(stack)-1]
		if len(stack) == 1 {
			return nil, "/", cur, nil
		}
		return stack[len(stack)-2], names[len(names)-1], cur, nil
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
				names = names[:len(names)-1]
			}
			if last {
				return terminalDir()
			}
		default:
			if last {
				n := cur.entries[elem]
				if trailingSlash && n != nil && !n.isDir {
					return nil, "", nil, syscall.ENOTDIR
				}
				return cur, elem, n, nil
			}
			n := cur.entries[elem]
			if n == nil {
				return nil, "", nil, syscall.ENOENT
			}
			if !n.isDir {
				return nil, "", nil, syscall.ENOTDIR
			}
			stack = append(stack, n)
			names = append(names, elem)
		}
	}
	// No components (root, or all trailing slashes) → the root directory.
	return terminalDir()
}

// dstFSComponents joins name against the calling process's working directory (if
// relative) and splits it into path components WITHOUT collapsing `.`/`..` (those
// are resolved physically by the walk in dstFSResolve) — only redundant slashes are
// dropped. It reports whether the path had a trailing slash (which asserts the target
// is a directory). Caller holds dstFS.mu.
func dstFSComponents(name string) (comps []string, trailingSlash bool) {
	full := name
	if len(name) == 0 || name[0] != '/' {
		full = dstFSCwdHere() + "/" + name
	}
	trailingSlash = len(full) > 1 && full[len(full)-1] == '/'
	start := 0
	for i := 0; i <= len(full); i++ {
		if i == len(full) || full[i] == '/' {
			if i > start {
				comps = append(comps, full[start:i])
			}
			start = i + 1
		}
	}
	return comps, trailingSlash
}

// dstFSAbs resolves name against the calling process's working directory and cleans
// it LEXICALLY (`path.Clean`) — the canonical path STRING for the stored handle path,
// the working directory, and rename same-path comparison. Path RESOLUTION against the
// tree is dstFSResolve's physical walk, not this; the cwd is stored clean and contains
// no `..`, so joining a clean cwd with a name preserves any `..` in the name for that
// walk. Caller holds dstFS.mu.
func dstFSAbs(name string) string {
	if len(name) > 0 && name[0] == '/' {
		return path.Clean(name)
	}
	return path.Clean(dstFSCwdHere() + "/" + name)
}

// dstMkdir implements Mkdir on the simulated tree.
func dstMkdir(name string, perm FileMode) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	if dstProcReserved(name) {
		return true, &PathError{Op: "mkdir", Path: name, Err: dstErrUnsupportedFS}
	}
	dstDiskDelayHere()
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	wrap := func(e error) (bool, error) { return true, &PathError{Op: "mkdir", Path: name, Err: e} }
	if name == "" {
		return wrap(syscall.ENOENT)
	}
	parent, base, node, errno := dstFSResolve(name)
	if errno != nil {
		return wrap(errno)
	}
	if node != nil {
		return wrap(syscall.EEXIST)
	}
	if dstFSDiskHere().diskFullForCreate() {
		return wrap(syscall.ENOSPC)
	}
	parent.entries[base] = &dstFSNode{
		isDir:   true,
		ino:     dstFSAllocIno(),
		entries: make(map[string]*dstFSNode),
		mode:    ModeDir | perm&ModePerm,
		modTime: time.Now(),
	}
	parent.modTime = time.Now()
	return true, nil
}

// dstFSMarkUnlinked marks a node removed from the namespace — and, for a
// directory subtree (RemoveAll), every directory under it — so entry creation
// through a still-open handle (a captured Root) fails ENOENT as the kernel's
// does in an rmdir'd directory. The entries clear too: the host's RemoveAll
// unlinks bottom-up, so a removed directory reads EMPTY through a surviving
// handle — leaving the detached children visible would be a sim-only listing.
// (Unlinked-but-open file CONTENT is untouched: it lives on the node, and
// open handles keep it per the POSIX contract.) Caller holds dstFS.mu.
func dstFSMarkUnlinked(node *dstFSNode) {
	if !node.isDir {
		return
	}
	node.unlinked = true
	for _, child := range node.entries {
		dstFSMarkUnlinked(child)
	}
	clear(node.entries)
}

// dstRemove implements Remove: files and empty directories. The node outlives
// the entry while open handles reference it (unlinked-but-open).
func dstRemove(name string) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	if dstProcReserved(name) {
		return true, &PathError{Op: "remove", Path: name, Err: dstErrUnsupportedFS}
	}
	dstDiskDelayHere()
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	wrap := func(e error) (bool, error) { return true, &PathError{Op: "remove", Path: name, Err: e} }
	if name == "" {
		return wrap(syscall.ENOENT)
	}
	if base := path.Base(name); base == "." || base == ".." {
		// unlink/rmdir of "." and ".." are rejected by the host (EINVAL /
		// ENOTEMPTY); EINVAL is os.Remove's observed shape for ".".
		return wrap(syscall.EINVAL)
	}
	parent, base, node, errno := dstFSResolve(name)
	if errno != nil {
		return wrap(errno)
	}
	if node == nil {
		return wrap(syscall.ENOENT)
	}
	if dstFSIsRoot(node) {
		return wrap(syscall.EBUSY)
	}
	if node.isDir && len(node.entries) > 0 {
		return wrap(syscall.ENOTEMPTY)
	}
	dstFSMarkUnlinked(node)
	delete(parent.entries, base)
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
	if dstProcReserved(name) {
		return true, &PathError{Op: "removeall", Path: name, Err: dstErrUnsupportedFS}
	}
	dstDiskDelayHere()
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	if name == "" {
		return true, nil // RemoveAll("") is a no-op success, like the host
	}
	if base := path.Base(name); base == "." || base == ".." {
		// The host's removeall rejects trailing-dot paths with EINVAL.
		return true, &PathError{Op: "RemoveAll", Path: name, Err: syscall.EINVAL}
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
	if dstFSIsRoot(node) {
		// Match RemoveAll("/"): refuse to destroy the root.
		return true, &PathError{Op: "removeall", Path: name, Err: syscall.EBUSY}
	}
	dstFSMarkUnlinked(node)
	delete(parent.entries, base)
	parent.modTime = time.Now()
	return true, nil
}

// dstRename implements the rename syscall's semantics on the tree: atomic in
// the namespace (observers under the same lock see old or new, never
// neither), replacing a file target, requiring an empty directory target for
// a directory source, and refusing ancestor moves.
func dstRename(oldname, newname string) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	if dstProcReserved(oldname) || dstProcReserved(newname) {
		return true, &LinkError{Op: "rename", Old: oldname, New: newname, Err: dstErrUnsupportedFS}
	}
	dstDiskDelayHere()
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	wrap := func(e error) (bool, error) { return true, &LinkError{Op: "rename", Old: oldname, New: newname, Err: e} }
	if oldname == "" || newname == "" {
		return wrap(syscall.ENOENT)
	}
	if b := path.Base(oldname); b == "." || b == ".." {
		return wrap(syscall.EBUSY) // rename(2): oldpath/newpath "." or ".."
	}
	if b := path.Base(newname); b == "." || b == ".." {
		return wrap(syscall.EBUSY)
	}
	oldParent, oldBase, oldNode, errno := dstFSResolve(oldname)
	if errno != nil {
		return wrap(errno)
	}
	if oldNode == nil {
		return wrap(syscall.ENOENT)
	}
	_, newTrailingSlash := dstFSComponents(newname)
	newParent, newBase, newNode, errno := dstFSResolve(newname)
	if errno != nil {
		return wrap(errno)
	}
	if newTrailingSlash && newNode == nil {
		return wrap(syscall.ENOTDIR)
	}
	if dstFSIsRoot(oldNode) || dstFSIsRoot(newNode) {
		return wrap(syscall.EBUSY)
	}
	if newNode == oldNode {
		return true, nil // same file: POSIX no-op success
	}
	oldAbs, newAbs := dstFSAbs(oldname), dstFSAbs(newname)
	if newAbs == oldAbs || (len(newAbs) > len(oldAbs) && newAbs[:len(oldAbs)] == oldAbs && newAbs[len(oldAbs)] == '/') {
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
	if newNode != nil {
		// The replaced target leaves the namespace exactly as a Remove would
		// (rename-over is atomic replace); an empty replaced directory becomes
		// unlinked for any Root still holding it.
		dstFSMarkUnlinked(newNode)
	}
	delete(oldParent.entries, oldBase)
	newParent.entries[newBase] = oldNode
	now := time.Now()
	oldParent.modTime = now
	newParent.modTime = now
	return true, nil
}

// dstStatName implements named Stat (and Lstat — no symlinks in the base
// model, so they coincide).
func dstStatName(op, name string) (FileInfo, bool, error) {
	if !dstFSActive() {
		return nil, false, nil
	}
	if fi, handled, err := dstProcStatName(op, name); handled {
		return fi, true, err
	}
	dstDiskDelayHere()
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	if name == "" {
		return nil, true, &PathError{Op: op, Path: name, Err: syscall.ENOENT}
	}
	_, base, node, errno := dstFSResolve(name)
	if errno != nil {
		return nil, true, &PathError{Op: op, Path: name, Err: errno}
	}
	if node == nil {
		return nil, true, &PathError{Op: op, Path: name, Err: syscall.ENOENT}
	}
	if dstFSIsRoot(node) {
		base = "/"
	}
	size := int64(len(node.data))
	return &dstFileInfo{
		name:    base,
		size:    size,
		mode:    node.mode,
		modTime: node.modTime,
		isDir:   node.isDir,
		ident:   node,
	}, true, nil
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
	if dstProcReserved(name) {
		return true, &PathError{Op: "truncate", Path: name, Err: dstErrUnsupportedFS}
	}
	dstDiskDelayHere()
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
	if err := node.truncateLocked(size); err != nil {
		return wrap(err)
	}
	return true, nil
}

// truncateLocked clamps or zero-extends current content. Caller holds
// dstFS.mu. Shared by handle and named truncate. Fails with the unsupported
// shape when the shrink would cut bytes under a live mapping (see
// dstMMapShrinkFencedLocked); callers must not have mutated anything first.
func (node *dstFSNode) truncateLocked(size int64) error {
	if dstMMapShrinkFencedLocked(node, size) {
		return dstErrUnsupportedFS
	}
	dstMMapSyncLocked(node)
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

// chmodLocked applies a mode change to a resolved node: permission plus the
// setuid/setgid/sticky bits, preserving the type bits. A mutation — the
// durable metadata image moves only on sync. ModTime is untouched: chmod(2)
// updates ctime only, which is not modeled.
func (node *dstFSNode) chmodLocked(mode FileMode) {
	const changeable = ModePerm | ModeSetuid | ModeSetgid | ModeSticky
	node.mode = node.mode&^changeable | mode&changeable
}

// dstChmod implements the named Chmod.
func dstChmod(name string, mode FileMode) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
	if dstProcReserved(name) {
		return true, &PathError{Op: "chmod", Path: name, Err: dstErrUnsupportedFS}
	}
	dstDiskDelayHere()
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
	if dstProcReserved(name) {
		return true, &PathError{Op: "chtimes", Path: name, Err: dstErrUnsupportedFS}
	}
	dstDiskDelayHere()
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
	if !mtime.IsZero() {
		node.modTime = mtime
	}
	_ = atime
	return true, nil
}

// dstGetwd / dstChdir: the per-bubble working directory.
func dstGetwd() (string, bool, error) {
	if !dstFSActive() {
		return "", false, nil
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	return dstFSCwdHere(), true, nil
}

func dstChdir(dir string) (handled bool, err error) {
	if !dstFSActive() {
		return false, nil
	}
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
	_, _, node, errno := dstFSResolve(dir)
	if errno != nil {
		return wrap(errno)
	}
	if node == nil {
		return wrap(syscall.ENOENT)
	}
	if !node.isDir {
		return wrap(syscall.ENOTDIR)
	}
	dstFSSetCwd(dstFSAbs(dir))
	return true, nil
}

// dstFile is the open-handle state for a simulated file: the node reference,
// the seek offset, and the access mode. It is the tree-file backend behind
// the os.File dst seam (dstFileBackend); the pipe (dst_pipe.go) is the
// sibling stream backend — the non-foreclosure shape recorded in the spec.
type dstFile struct {
	mu     sync.Mutex
	node   *dstFSNode
	disk   *dstFSDisk // the host disk this handle was opened on (for disk faults)
	path   string
	off    int64
	dirpos int // directory read cursor (sorted-name index)
	rd, wr bool
	app    bool
	osync  bool // O_SYNC: every write commits the durable image
	closed bool
}

// dstOpenFile implements OpenFile against the simulated tree while a run is
// active. handled=false outside a run (fall through to the host).
func dstOpenFile(name string, flag int, perm FileMode) (f *File, handled bool, err error) {
	if !dstFSActive() {
		return nil, false, nil
	}
	if f, handled, err := dstProcOpenFile(name, flag); handled {
		return f, true, err
	}
	wrap := func(e error) (*File, bool, error) {
		return nil, true, &PathError{Op: "open", Path: name, Err: e}
	}
	if name == "" {
		return wrap(syscall.ENOENT)
	}

	dstDiskDelayHere()
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()

	parent, base, node, errno := dstFSResolve(name)
	if errno != nil {
		return wrap(errno)
	}

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
		if dstFSDiskHere().diskFullForCreate() {
			return wrap(syscall.ENOSPC)
		}
		node = &dstFSNode{
			ino:     dstFSAllocIno(),
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
		if err := node.truncateLocked(0); err != nil {
			return wrap(err)
		}
	}

	d := &dstFile{
		node:  node,
		disk:  dstFSDiskHere(),
		path:  dstFSAbs(name),
		rd:    flag&O_WRONLY == 0,
		wr:    accWrite,
		app:   flag&O_APPEND != 0,
		osync: flag&O_SYNC != 0,
	}
	return dstNewFile(d, name), true, nil
}

// dstOpenDir is openDirNolog's counterpart: the target must be a directory
// (ENOTDIR for a file — the O_DIRECTORY shape), opened read-only.
func dstOpenDir(name string) (f *File, handled bool, err error) {
	if !dstFSActive() {
		return nil, false, nil
	}
	if dstProcReserved(name) {
		return nil, true, &PathError{Op: "open", Path: name, Err: dstErrUnsupportedFS}
	}
	wrap := func(e error) (*File, bool, error) {
		return nil, true, &PathError{Op: "open", Path: name, Err: e}
	}
	dstDiskDelayHere()
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
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
		return wrap(syscall.ENOTDIR)
	}
	d := &dstFile{node: node, disk: dstFSDiskHere(), path: dstFSAbs(name), rd: true}
	return dstNewFile(d, name), true, nil
}

// dstNewFile builds an *os.File backed by a simulated file. The pfd is left
// with an invalid Sysfd so any not-yet-gated path fails with EBADF
// deterministically instead of touching a real descriptor.
func dstNewFile(d dstFileBackend, name string) *File {
	f := &File{&file{name: name, dstf: d}}
	f.pfd.Sysfd = -1
	_, proc := dstFSCurrentNode()
	dstRegisterOpenFile(f.file, proc)
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

// diskDelay sleeps this handle's disk's per-op latency before a disk-touching op (a
// slow disk). It reads the latency lock-free and sleeps OUTSIDE the tree lock — the
// op takes dstFS.mu afterward — so a slow disk on one host never blocks another's
// filesystem. The gate makes the no-fault path a single atomic load. A closed handle
// is skipped: a closed fd returns EBADF without touching the disk, so a delay there
// would be one a real slow disk never imposes (DST-FAULT-SOUND).
func (d *dstFile) diskDelay() {
	if !dstDiskSlow.Load() {
		return
	}
	lat := d.disk.latency.Load()
	if lat <= 0 {
		return
	}
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if !closed {
		time.Sleep(time.Duration(lat))
	}
}

// dstDiskDelayHere is diskDelay for a named (path) op, which has no handle: it sleeps
// the calling host's disk latency before the op takes the tree lock. The brief lock
// is only to look up the host's disk (a map read); the sleep happens after releasing
// it. Gated like diskDelay so an unslowed run pays only the atomic load.
func dstDiskDelayHere() {
	if !dstDiskSlow.Load() {
		return
	}
	dstFS.mu.Lock()
	dstFSRoll()
	lat := dstFSDiskHere().latency.Load()
	dstFS.mu.Unlock()
	if lat > 0 {
		time.Sleep(time.Duration(lat))
	}
}

// diskEIO returns syscall.EIO if this handle's disk is under a host-wide EIO fault
// or this file's node is under a per-file EIO fault, else nil. Caller holds
// dstFS.mu. Checked only at the calls a real disk can fail with EIO (read / write /
// sync), never at an infallible call (seek, in-memory stat) — DST-FAULT-SOUND.
func (d *dstFile) diskEIO() error {
	if d.disk.eio || d.disk.eioFiles[d.node] {
		return syscall.EIO
	}
	return nil
}

// residentLocked sums the live byte size of every regular file on the disk — the
// space a capacity is measured against. Summed on demand (not tracked incrementally)
// so a delete or truncate-down frees space for the next write with no accounting in
// the mutation paths; each node has exactly one name (no hard links — os.Link is
// fenced under simulation), so the walk counts each file once. Removed-but-open files
// are not in the tree and so are not counted, which only ever under-counts space in
// use — never a false ENOSPC. Caller holds dstFS.mu.
func (disk *dstFSDisk) residentLocked() int64 {
	var total int64
	var walk func(n *dstFSNode)
	walk = func(n *dstFSNode) {
		if n.isDir {
			for _, c := range n.entries {
				walk(c)
			}
			return
		}
		total += int64(len(n.data))
	}
	walk(disk.root)
	return total
}

// enospcAllowed returns how many of the n bytes a write at off may store before the
// disk's capacity is hit. It is a dstFile method (not dstFSDisk) because it needs the
// file's current length to tell growth from an in-place overwrite. Equal to n when
// the disk is uncapped, when the write does
// not grow the file (a pure in-place overwrite consumes no space), or when the growth
// fits; otherwise it is the short count that fills the remaining space (0 if none is
// left). A real disk fills what it can and reports the shortfall, so the caller writes
// the allowed prefix and returns ENOSPC only when nothing fit (DST-FAULT-SOUND: a
// growth a real disk would partially satisfy is never failed outright). Caller holds
// dstFS.mu.
func (d *dstFile) enospcAllowed(off, n int64) int64 {
	if !d.disk.capped {
		return n
	}
	L := int64(len(d.node.data))
	end := off + n
	if end <= L {
		return n // pure overwrite: no growth, no space consumed
	}
	room := d.disk.capacity - d.disk.residentLocked()
	if room < 0 {
		room = 0
	}
	writableEnd := L + room
	if end <= writableEnd {
		return n // the growth fits
	}
	if writableEnd <= off {
		return 0 // not even the write's start offset is reachable
	}
	return writableEnd - off
}

// diskFullForCreate reports whether the disk has no room to allocate a new file or
// directory (ENOSPC on create/mkdir on a full disk). A 0-byte create consumes no file
// bytes, so it is refused only once the disk is already at or over capacity. Caller
// holds dstFS.mu.
func (disk *dstFSDisk) diskFullForCreate() bool {
	return disk.capped && disk.residentLocked() >= disk.capacity
}

func (d *dstFile) read(b []byte) (int, error) {
	d.diskDelay()
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
	if d.node.isDir {
		return 0, syscall.EISDIR
	}
	if err := d.diskEIO(); err != nil {
		return 0, err
	}
	dstMMapSyncLocked(d.node)
	if d.off >= int64(len(d.node.data)) {
		return 0, io.EOF
	}
	n := copy(b, d.node.data[d.off:])
	d.off += int64(n)
	return n, nil
}

func (d *dstFile) pread(b []byte, off int64) (int, error) {
	d.diskDelay()
	if err := d.enter(); err != nil {
		return 0, err
	}
	defer d.leave()
	// No empty-buffer early return (unlike read): pread is reached only from
	// File.ReadAt's `for len(b) > 0` loop, so b is never empty here.
	if !d.rd {
		return 0, syscall.EBADF
	}
	if d.node.isDir {
		return 0, syscall.EISDIR
	}
	if err := d.diskEIO(); err != nil {
		return 0, err
	}
	dstMMapSyncLocked(d.node)
	if off >= int64(len(d.node.data)) {
		return 0, io.EOF
	}
	return copy(b, d.node.data[off:]), nil
}

func (d *dstFile) write(b []byte) (int, error) {
	d.diskDelay()
	if err := d.enter(); err != nil {
		return 0, err
	}
	defer d.leave()
	if !d.wr {
		return 0, syscall.EBADF
	}
	if err := d.diskEIO(); err != nil {
		return 0, err
	}
	if d.app {
		d.off = int64(len(d.node.data))
	}
	allowed := d.enospcAllowed(d.off, int64(len(b)))
	n := d.writeAtLocked(b[:allowed], d.off)
	d.off += int64(n)
	if d.osync {
		d.node.commitLocked()
	}
	if n < len(b) {
		// The disk filled: the remaining bytes fail ENOSPC, reported together with
		// the partial count in ONE call — mirroring internal/poll.FD.Write's loop,
		// which retries a short kernel write and surfaces (n, ENOSPC). Returning a
		// bare short count instead would let os.File.Write report io.ErrShortWrite,
		// an error identity a real regular-file write cannot produce, so the SUT's
		// errors.Is(err, ENOSPC) recovery would miss exactly the faulted write.
		return n, syscall.ENOSPC
	}
	return n, nil
}

func (d *dstFile) pwrite(b []byte, off int64) (int, error) {
	d.diskDelay()
	if err := d.enter(); err != nil {
		return 0, err
	}
	defer d.leave()
	if !d.wr {
		return 0, syscall.EBADF
	}
	if err := d.diskEIO(); err != nil {
		return 0, err
	}
	// pwrite models a SINGLE pwrite(2): os.File.WriteAt loops over it and adds the
	// count only after the error check, so a partial fill must return (n, nil) here
	// and let the loop surface ENOSPC on the next zero-byte call — returning a
	// combined (n, ENOSPC) would make WriteAt discard the n. (The single-call Write
	// path, by contrast, needs the combined return — see write.)
	allowed := d.enospcAllowed(off, int64(len(b)))
	if allowed == 0 && len(b) > 0 {
		return 0, syscall.ENOSPC
	}
	n := d.writeAtLocked(b[:allowed], off)
	if d.osync {
		d.node.commitLocked()
	}
	return n, nil
}

// writeAtLocked extends-with-zeros as needed and copies b at off. Caller
// holds both locks. Mutates current content only (never the durable image).
func (d *dstFile) writeAtLocked(b []byte, off int64) int {
	node := d.node
	dstMMapSyncLocked(node)
	if need := off + int64(len(b)); need > int64(len(node.data)) {
		if need <= int64(cap(node.data)) {
			// Re-extending within capacity: zero the gap, or bytes from a
			// previous longer state resurrect (truncate-down then extend).
			oldLen := len(node.data)
			node.data = node.data[:need]
			clear(node.data[oldLen:])
		} else {
			// append for amortized growth (an exact-size make would copy
			// O(n^2) over append-heavy workloads).
			node.data = append(node.data, make([]byte, need-int64(len(node.data)))...)
		}
	}
	n := copy(node.data[off:], b)
	dstMMapWriteLocked(node, off, b[:n])
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
	if d.node.isDir {
		if pos != 0 {
			return 0, syscall.EISDIR
		}
		d.dirpos = 0
		return 0, nil
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
	d.diskDelay()
	if e := d.enter(); e != nil {
		return nil, nil, e
	}
	defer d.leave()
	if !d.node.isDir {
		return nil, nil, syscall.ENOTDIR
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
		infos = append(infos, &dstFileInfo{
			name:    name,
			size:    int64(len(node.data)),
			mode:    node.mode,
			modTime: node.modTime,
			isDir:   node.isDir,
			ident:   node,
		})
	}
	return names, infos, nil
}

// chdirHandle implements File.Chdir for a simulated directory handle.
func (d *dstFile) chdirHandle() error {
	if e := d.enter(); e != nil {
		return e
	}
	defer d.leave()
	if !d.node.isDir {
		return syscall.ENOTDIR
	}
	dstFSSetCwd(d.path)
	return nil
}

func (d *dstFile) truncate(size int64) error {
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
	return d.node.truncateLocked(size)
}

// sync commits the durable image (the durability contract's commit points):
// for a file, the current content and metadata; for a directory, the current
// entry set (entry durability is the parent directory's property — POSIX's
// fsync-the-file-then-fsync-the-directory model). Mutations never touch the
// durable image; this is its only writer (plus O_SYNC's per-write commit,
// which routes through the same helper).
func (d *dstFile) sync() error {
	d.diskDelay()
	if err := d.enter(); err != nil {
		return err
	}
	defer d.leave()
	if err := d.diskEIO(); err != nil {
		return err
	}
	d.node.commitLocked()
	return nil
}

func (d *dstFile) datasync() error {
	d.diskDelay()
	if err := d.enter(); err != nil {
		return err
	}
	defer d.leave()
	if d.node.isDir {
		return syscall.EINVAL
	}
	if err := d.diskEIO(); err != nil {
		return err
	}
	d.node.commitDataLocked()
	return nil
}

// commitLocked advances the node's durable image to its current state.
// Caller holds dstFS.mu.
func (node *dstFSNode) commitLocked() {
	if node.isDir {
		node.syncedEntries = make(map[string]*dstFSNode, len(node.entries))
		for name, child := range node.entries {
			node.syncedEntries[name] = child
		}
	} else {
		node.commitDataLocked()
	}
	node.syncedMode = node.mode
	node.syncedModTime = node.modTime
}

func (node *dstFSNode) commitDataLocked() {
	dstMMapSyncLocked(node)
	node.synced = append([]byte(nil), node.data...)
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
		ident:   d.node,
	}, nil
}

// setDeadline implements the deadline half of the backend seam for tree
// files: the host's regular files and directories are not pollable, so
// SetDeadline fails with the (unwrapped) ErrNoDeadline shape there, and the
// simulated tree mirrors that — including the host's precedence, where a
// closed handle reports the (also unwrapped) closed shape first. Pipes are
// the pollable case — see dst_pipe.go.
func (d *dstFile) setDeadline(rd, wd bool, t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return poll.ErrFileClosing
	}
	return poll.ErrNoDeadline
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
	return nil
}

func (d *dstFile) dropClosedNode() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		d.node = nil
	}
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
