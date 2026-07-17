// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"time"
	_ "unsafe" // for go:linkname
)

// Disk faults over the per-host filesystem seam (docs/dst/faults.md "Disk faults"),
// the storage-axis counterpart of the network targeting API (Partition / Reset). A
// fault names a host (the same name passed to Host) — and, for the per-file form, a
// host-absolute path — interns the host to the id os keys its disks by, and drives
// the fault through runtime (always linked) into os, so simulation needs no direct
// dependency on os. A disk fault is a real disk degree of freedom: EIO is what a real
// disk returns from a read / write / fsync that hits bad media or a failing
// controller, so it is injected only at those calls (never at an infallible call
// like seek), and it never touches the durable image — a failed fsync does not
// advance it; ENOSPC is a full disk, so it is injected only where writing more, or
// creating a file, needs space the cap does not allow — a real disk fills what it can
// first, and frees count, so deleting makes room (DST-FAULT-SOUND). The EIO,
// ENOSPC, and latency faults are explicit toggles (no fault-RNG draw), and the
// corruption fault draws only at injection from the stream-isolated fault RNG, so
// the same seed + same fault schedule replays identically (DST-FAULT-REPLAY), and
// each affects exactly the named host's disk — or, for FailFile, exactly the named
// file — leaving every other host and file untouched (DST-FAULT-VICTIM). Calls
// outside a run, or in a run whose binary links no filesystem use, are no-ops. Call
// them from within a Run.

//go:linkname dstDiskFaultOp runtime.dstDiskFaultOp
func dstDiskFaultOp(op, host uint32, arg int64, name string)

// Disk-fault op codes — must match os's dst_disk_fault.go.
const (
	diskOpFailDisk uint32 = iota + 1
	diskOpHealDisk
	diskOpFailFile
	diskOpHealFile
	diskOpLimit
	diskOpUnlimit
	diskOpSlow
	diskOpCorruptFile
)

// The victim-naming rule for every fault below: an undeclared host name panics
// during a run (a typo'd victim must fail loud, never silently test nothing);
// calls outside a run are no-ops.
//
// FailDisk makes every read, write, and fsync on the named host's disk fail with
// EIO, modeling a failing disk or controller, until HealDisk(host) restores it. It
// targets exactly the host's disk: another host's I/O, and metadata-only operations
// that do not touch the media, are unaffected. In-memory file content is not lost
// and a heal resumes normal I/O — but a data sync that failed under the fault has
// DROPPED the file's dirty pages from the writeback set, as Linux >= 4.13 does: a
// retried sync after the heal succeeds without those pages reaching the durable
// image, and only pages rewritten after the failure are written back. A recovery
// that merely retries fsync after EIO therefore passes the retry and still loses
// the data on power loss; rewriting the data first is the recovery that works.
func FailDisk(host string) {
	withBubbleFaultCaller("FailDisk", func() { dstDiskFaultOp(diskOpFailDisk, lookupHost(host), 0, "") })
}

// HealDisk clears the host-wide EIO fault set by FailDisk; the host's reads, writes,
// and fsyncs succeed again. Per-file faults set by FailFile are unaffected.
func HealDisk(host string) {
	withBubbleFaultCaller("HealDisk", func() { dstDiskFaultOp(diskOpHealDisk, lookupHost(host), 0, "") })
}

// FailFile makes reads, writes, and fsyncs of one regular file on the named host
// fail with EIO — a bad sector on that file's blocks — while the rest of the host's
// disk works, until HealFile restores it. path is host-absolute. The fault keys on
// the file itself, not its path, so it follows the file across a rename and a
// removed-but-open handle keeps failing; faulting a path that does not exist, or that
// names a directory, is a no-op (there is no file's I/O to fail).
func FailFile(host, path string) {
	withBubbleFaultCaller("FailFile", func() { dstDiskFaultOp(diskOpFailFile, lookupHost(host), 0, path) })
}

// HealFile clears the per-file EIO fault set by FailFile on the named host's file.
func HealFile(host, path string) {
	withBubbleFaultCaller("HealFile", func() { dstDiskFaultOp(diskOpHealFile, lookupHost(host), 0, path) })
}

// LimitDisk caps the named host's disk at bytes total of regular-file content,
// modeling a full (or filling) disk: a write that would grow the disk past the cap
// fills the remaining space and the rest fails with ENOSPC, and creating a new file
// or directory on an already-full disk fails with ENOSPC. Space in use is the live
// total, so deleting or truncating a file frees room for later writes; an in-place
// overwrite (no growth) always succeeds, and reads are unaffected. A cap below the
// current usage is allowed (the disk is over quota: growth and creates fail until
// enough is freed). bytes must be >= 0. UnlimitDisk removes the cap. Call from within
// a Run.
//
// Recorded modeling boundary (docs/dst/faults.md, ENOSPC): the accounting is
// LOGICAL bytes, not allocated blocks, so sparse files diverge from a real
// disk in both directions — a sparse Truncate-grow's hole counts against the
// cap (a real hole allocates nothing), writes filling a hole are never
// charged, and truncate growth is charged but not itself ENOSPC-checked. A
// SUT relying on sparse preallocation is outside this fault's honest surface.
func LimitDisk(host string, bytes int64) {
	withBubbleFaultCaller("LimitDisk", func() {
		if bytes < 0 {
			panic("testing/simulation: LimitDisk bytes must be >= 0")
		}
		dstDiskFaultOp(diskOpLimit, lookupHost(host), bytes, "")
	})
}

// UnlimitDisk removes the capacity set by LimitDisk on the named host's disk; writes
// and creates stop failing with ENOSPC.
func UnlimitDisk(host string) {
	withBubbleFaultCaller("UnlimitDisk", func() { dstDiskFaultOp(diskOpUnlimit, lookupHost(host), 0, "") })
}

// SlowDisk models a slow disk: every disk-touching filesystem operation on the named
// host — read, write, fsync, open, stat, mkdir, remove, rename, readdir, truncate,
// chmod, chtimes — takes perOp longer (virtual time), as if serviced by a slow
// device. Pure in-memory operations that a real slow disk does not slow (seek, Getwd)
// are unaffected. The delay is the calling goroutine's alone — a slow disk on one
// host does not stall another's filesystem — and is deterministic (an explicit
// duration, no fault-RNG draw), so the same seed and schedule replay identically.
// The delay is per backend operation, so a composite helper pays it once for each
// op it issues: os.Rename does an internal stat then the rename (two delays), and
// os.ReadFile / os.WriteFile open, then read or write, then close (close is not a
// disk op) — as a real slow disk would charge each syscall. perOp of 0 removes the
// latency. Negative perOp is invalid. Call from within a Run.
func SlowDisk(host string, perOp time.Duration) {
	withBubbleFaultCaller("SlowDisk", func() {
		if perOp < 0 {
			panic("testing/simulation: SlowDisk perOp must be >= 0")
		}
		dstDiskFaultOp(diskOpSlow, lookupHost(host), int64(perOp), "")
	})
}

// CorruptFile flips one bit of one byte of the named host file's DURABLE image —
// silent media corruption (bit rot), the fault a checksum exists to catch, as
// distinct from FailFile's unreadable sector (EIO). The byte offset and the bit
// are drawn from the seeded fault RNG, so where the rot lands is a deterministic
// function of the run seed and the fault schedule (DST-FAULT-REPLAY); repeated
// calls accumulate independent flips (XOR semantics: a repeat that draws the
// same offset and bit cancels it — the platter byte reverts). path is
// host-absolute.
//
// The flip lands on the platter, not in the page cache: reads keep returning
// the bytes the program wrote (a real kernel serves cached pages, and the model
// never evicts), and the corruption surfaces where a real machine discovers
// latent rot — when the platter is next read, at a host crash: after
// CrashHost(host) and a Host re-declaration, the file's content carries the
// flipped bit, which is exactly what a recovery-path integrity check must
// detect rather than silently accept. Writeback heals rot page by page: a
// successful sync clears the flips in every page whose committed bytes changed
// (a byte-identical rewrite does not clear them — the content diff is the dirty
// proxy, a recorded modeling bound equivalent to the rot recurring after the
// write), while pages the sync never rewrote keep theirs, so corrupting a
// write-once region survives later appends-and-syncs elsewhere in the file.
//
// Corrupting a path that does not exist, that names a directory or device, or a
// file whose durable image is empty (nothing was ever committed to the platter)
// is a no-op — and draws nothing from the fault RNG, so a
// skipped target never shifts a later fault's stream. Call from within a Run.
func CorruptFile(host, path string) {
	withBubbleFaultCaller("CorruptFile", func() { dstDiskFaultOp(diskOpCorruptFile, lookupHost(host), 0, path) })
}
