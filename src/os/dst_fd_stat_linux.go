// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import (
	"syscall"
	"unsafe"
)

const (
	dstStatfsExt4Magic  = 0xef53 // EXT4_SUPER_MAGIC
	dstStatfsProcMagic  = 0x9fa0 // PROC_SUPER_MAGIC
	dstStatfsTotalFiles = 1 << 20
)

// dstStatDev converts the simulated host id to the arch's st_dev field width
// (uint64 on most Linux arches, uint32 on mips) without per-arch files: the
// zero-valued field pins T. Host ids are small; +1 keeps device 0 unused.
func dstStatNlink[T ~uint32 | ~uint64](_ T, n int) T { return T(n) }

func dstStatDev[T ~uint32 | ~uint64](_ T, host uint32) T {
	return T(host) + 1
}

func dstStatfsVal[T ~int32 | ~uint32 | ~int64](_ T, v int64) T {
	return T(v)
}

func dstFDFstat(fd int, stat *syscall.Stat_t) (syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return errno, handled
	}
	if stat == nil {
		return syscall.EFAULT, true
	}
	info, err := entry.backend.stat()
	if errno := dstFDErr(err); errno != 0 {
		return errno, true
	}
	*stat = syscall.Stat_t{}
	stat.Size = info.Size()
	stat.Nlink = 1
	stat.Blksize = 4096
	if stat.Size > 0 {
		stat.Blocks = (stat.Size + 511) / 512
	}
	// (st_dev, st_ino) is the file-identity pair inode-keyed SUTs (the
	// SQLite/LMDB per-file lock-dedup pattern) require: dev derives from the
	// owning host's id (+1 so no simulated device is 0), ino is the node's
	// synthetic inode. Proc-overlay fds carry no tree node and keep (dev, ino)
	// zero — no SUT keys identity on synthetic procfs stats. All node fields
	// come from the dstFileInfo snapshot captured under dstFile.enter() so
	// fstat never races with concurrent close or link/block mutations.
	_, isTree := entry.backend.(*dstFile)
	if isTree {
		dfi := info.(*dstFileInfo)
		stat.Dev = dstStatDev(stat.Dev, entry.host)
		stat.Ino = dfi.ino
		stat.Blocks = dfi.blocks
		if !dfi.isDir {
			// The live hard-link count; 0 for an unlinked-but-open file,
			// as fstat(2) reports. Nlink's width is per-arch.
			stat.Nlink = dstStatNlink(stat.Nlink, dfi.nlink)
		}
	}
	// Proc-overlay fds carry no tree node: (st_dev, st_ino) stays zero —
	// synthetic procfs identity, per the spec's proc-fd contract — and so
	// do the timestamps (the overlay's zero mtime would otherwise turn
	// into UnixNano's garbage negative, a shape no kernel reports), so the
	// mtime stamping below is scoped to tree files.
	stat.Mode = syscallMode(info.Mode())
	switch {
	case info.IsDir():
		stat.Mode |= syscall.S_IFDIR
		// A directory's link count is at least 2 ("." and its parent's entry);
		// per-subdirectory increments are not modeled (recorded in the spec).
		stat.Nlink = 2
	case info.Mode()&ModeCharDevice != 0:
		// The simulated /dev/null. st_rdev is the DEVICE's identity, fixed by
		// Linux at (major 1, minor 3) — machine-independent, unlike st_dev
		// (host-verified rdev shape; minor lives in the low byte, major in
		// the next twelve bits for small numbers).
		stat.Mode |= syscall.S_IFCHR
		stat.Rdev = 1<<8 | 3
	default:
		stat.Mode |= syscall.S_IFREG
	}
	if isTree {
		mtime := info.ModTime()
		stat.Mtim = syscall.NsecToTimespec(mtime.UnixNano())
		stat.Atim = stat.Mtim
		stat.Ctim = stat.Mtim
	}
	return 0, true
}

func dstFillTreeStatfsLocked(buf *syscall.Statfs_t, host uint32, disk *dstFSDisk) {
	usedPages, usedInodes := disk.usageLocked(true)
	var totalPages, freePages uint64
	if disk.capped.Load() {
		if disk.capacity > 0 {
			totalPages = uint64(disk.capacity / dstPageSize)
		}
		if usedPages < totalPages {
			freePages = totalPages - usedPages
		}
	} else {
		defPages := uint64(1) << 25
		if dstSMaxBytes > 0 {
			defPages = uint64(dstSMaxBytes / dstPageSize)
		}
		totalPages = max(usedPages, defPages)
		freePages = totalPages - usedPages
	}
	totalFiles := max(usedInodes, uint64(dstStatfsTotalFiles))
	freeFiles := totalFiles - usedInodes

	*buf = syscall.Statfs_t{}
	buf.Type = dstStatfsVal(buf.Type, dstStatfsExt4Magic)
	buf.Bsize = dstStatfsVal(buf.Bsize, dstPageSize)
	buf.Frsize = dstStatfsVal(buf.Frsize, dstPageSize)
	buf.Namelen = dstStatfsVal(buf.Namelen, 255)
	buf.Blocks = totalPages
	buf.Bfree = freePages
	buf.Bavail = freePages
	buf.Files = totalFiles
	buf.Ffree = freeFiles
	buf.Fsid.X__val[0] = int32(host) + 1
}

func dstFillProcStatfs(buf *syscall.Statfs_t) {
	*buf = syscall.Statfs_t{}
	buf.Type = dstStatfsVal(buf.Type, dstStatfsProcMagic)
	buf.Bsize = dstStatfsVal(buf.Bsize, dstPageSize)
	buf.Frsize = dstStatfsVal(buf.Frsize, dstPageSize)
	buf.Namelen = dstStatfsVal(buf.Namelen, 255)
}

func dstStatfs(path string, buf *syscall.Statfs_t) (syscall.Errno, bool) {
	if !dstFSActive() {
		return 0, false
	}
	dstCoarsePathDep(path, false)
	dstDiskDelayPath(path)
	if _, _, _, handled, errno := dstProcStatData(path); handled {
		if errno != nil {
			if e, ok := errno.(syscall.Errno); ok {
				return e, true
			}
			return syscall.EIO, true
		}
		if buf == nil {
			return syscall.EFAULT, true
		}
		dstFillProcStatfs(buf)
		return 0, true
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	if path == "" {
		return syscall.ENOENT, true
	}
	_, _, node, errno := dstFSResolve(path)
	if errno != nil {
		if e, ok := errno.(syscall.Errno); ok {
			return e, true
		}
		return syscall.EIO, true
	}
	if node == nil {
		return syscall.ENOENT, true
	}
	if buf == nil {
		return syscall.EFAULT, true
	}
	host, _ := dstFSCurrentNode()
	disk := dstFSDiskHere()
	dstCoarseRecord(uintptr(unsafe.Pointer(disk)), false)
	dstFillTreeStatfsLocked(buf, host, disk)
	return 0, true
}

func dstFDFstatfs(fd int, buf *syscall.Statfs_t) (syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return errno, handled
	}
	if f, ok := entry.backend.(*dstFile); ok {
		f.coarseOp(false, false)
		dstCoarseRecord(dstCoarseNamespaceForHost(entry.host), false)
		if err := f.enter(); err != nil {
			return dstFDErr(err), true
		}
		defer f.leave()
		if buf == nil {
			return syscall.EFAULT, true
		}
		dstCoarseRecord(uintptr(unsafe.Pointer(f.disk)), false)
		dstFillTreeStatfsLocked(buf, entry.host, f.disk)
		return 0, true
	}
	if _, err := entry.backend.stat(); err != nil {
		return dstFDErr(err), true
	}
	if buf == nil {
		return syscall.EFAULT, true
	}
	dstFillProcStatfs(buf)
	return 0, true
}
