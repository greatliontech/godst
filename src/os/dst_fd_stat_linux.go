// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import "syscall"

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
	stat.Mode = syscallMode(info.Mode())
	if info.IsDir() {
		stat.Mode |= syscall.S_IFDIR
	} else {
		stat.Mode |= syscall.S_IFREG
	}
	mtime := info.ModTime()
	stat.Mtim = syscall.NsecToTimespec(mtime.UnixNano())
	stat.Atim = stat.Mtim
	stat.Ctim = stat.Mtim
	return 0, true
}
