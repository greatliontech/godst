// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst && unix

package syscall

// Stock build: the interception-boundary fences fold away. dstSimFenced is a
// constant false, so every `if dstSimFenced && …` branch is dead-code-
// eliminated and the identifiers below exist only to satisfy the type checker.
const dstSimFenced = false

func dstFenceActive() bool { return false }

var dstErrUnsupported error

func dstTryRead(fd int, p []byte) (n int, err Errno, handled bool) { return 0, 0, false }

func dstTryWrite(fd int, p []byte) (n int, err Errno, handled bool) { return 0, 0, false }

func dstTryPread(fd int, p []byte, offset int64) (n int, err Errno, handled bool) {
	return 0, 0, false
}

func dstTryPwrite(fd int, p []byte, offset int64) (n int, err Errno, handled bool) {
	return 0, 0, false
}

func dstTryClose(fd int) (err Errno, handled bool) { return 0, false }

func dstTryFstat(fd int, stat *Stat_t) (err Errno, handled bool) { return 0, false }

func dstTrySeek(fd int, offset int64, whence int) (off int64, err Errno, handled bool) {
	return 0, 0, false
}

func dstTryFsync(fd int) (err Errno, handled bool) { return 0, false }

func dstTryFdatasync(fd int) (err Errno, handled bool) { return 0, false }

func dstTryFlock(fd int, how int) (err Errno, handled bool) { return 0, false }

func dstTryMmap(fd int, offset int64, length int, prot int, flags int) (data []byte, err Errno, handled bool) {
	return nil, 0, false
}

func dstTryMunmap(data []byte) (err Errno, handled bool) { return 0, false }

func dstTryMprotect(data []byte, prot int) (err Errno, handled bool) { return 0, false }

func dstTryMadvise(data []byte, advice int) (err Errno, handled bool) { return 0, false }

func dstTryKill(pid int, sig Signal) (err Errno, handled bool) { return 0, false }

func dstTryClockGettime(trap, clockid, ts uintptr) (r1, r2 uintptr, err Errno, handled bool) {
	return 0, 0, 0, false
}
