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
// first, and frees count, so deleting makes room (DST-FAULT-SOUND). Faults are
// explicit toggles (no fault-RNG draw), so
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
)

// FailDisk makes every read, write, and fsync on the named host's disk fail with
// EIO, modeling a failing disk or controller, until HealDisk(host) restores it. It
// targets exactly the host's disk: another host's I/O, and metadata-only operations
// that do not touch the media, are unaffected. In-memory file content is not lost —
// a heal resumes normal I/O — so a SUT can test how it tolerates and recovers from a
// disk that returns errors.
func FailDisk(host string) {
	dstDiskFaultOp(diskOpFailDisk, internHost(host), 0, "")
}

// HealDisk clears the host-wide EIO fault set by FailDisk; the host's reads, writes,
// and fsyncs succeed again. Per-file faults set by FailFile are unaffected.
func HealDisk(host string) {
	dstDiskFaultOp(diskOpHealDisk, internHost(host), 0, "")
}

// FailFile makes reads, writes, and fsyncs of one regular file on the named host
// fail with EIO — a bad sector on that file's blocks — while the rest of the host's
// disk works, until HealFile restores it. path is host-absolute. The fault keys on
// the file itself, not its path, so it follows the file across a rename and a
// removed-but-open handle keeps failing; faulting a path that does not exist, or that
// names a directory, is a no-op (there is no file's I/O to fail).
func FailFile(host, path string) {
	dstDiskFaultOp(diskOpFailFile, internHost(host), 0, path)
}

// HealFile clears the per-file EIO fault set by FailFile on the named host's file.
func HealFile(host, path string) {
	dstDiskFaultOp(diskOpHealFile, internHost(host), 0, path)
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
func LimitDisk(host string, bytes int64) {
	if bytes < 0 {
		panic("testing/simulation: LimitDisk bytes must be >= 0")
	}
	dstDiskFaultOp(diskOpLimit, internHost(host), bytes, "")
}

// UnlimitDisk removes the capacity set by LimitDisk on the named host's disk; writes
// and creates stop failing with ENOSPC.
func UnlimitDisk(host string) {
	dstDiskFaultOp(diskOpUnlimit, internHost(host), 0, "")
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
	if perOp < 0 {
		panic("testing/simulation: SlowDisk perOp must be >= 0")
	}
	dstDiskFaultOp(diskOpSlow, internHost(host), int64(perOp), "")
}
