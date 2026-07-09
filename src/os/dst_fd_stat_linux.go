// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import "syscall"

// dstStatDev converts the simulated host id to the arch's st_dev field width
// (uint64 on most Linux arches, uint32 on mips) without per-arch files: the
// zero-valued field pins T. Host ids are small; +1 keeps device 0 unused.
func dstStatDev[T ~uint32 | ~uint64](_ T, host uint32) T {
	return T(host) + 1
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
	// zero — no SUT keys identity on synthetic procfs stats.
	stat.Dev = dstStatDev(stat.Dev, entry.host)
	if file, ok := entry.backend.(*dstFile); ok {
		stat.Ino = file.node.ino
	}
	stat.Mode = syscallMode(info.Mode())
	if info.IsDir() {
		stat.Mode |= syscall.S_IFDIR
		// A directory's link count is at least 2 ("." and its parent's entry);
		// per-subdirectory increments are not modeled (recorded in the spec).
		stat.Nlink = 2
	} else {
		stat.Mode |= syscall.S_IFREG
	}
	mtime := info.ModTime()
	stat.Mtim = syscall.NsecToTimespec(mtime.UnixNano())
	stat.Atim = stat.Mtim
	stat.Ctim = stat.Mtim
	return 0, true
}
