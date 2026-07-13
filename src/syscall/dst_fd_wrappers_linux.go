// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package syscall

func Close(fd int) (err error) {
	if e1, handled := dstTryClose(fd); handled {
		if e1 != 0 {
			err = errnoErr(e1)
		}
		return
	}
	return closeFD(fd)
}

func Fstat(fd int, stat *Stat_t) (err error) {
	if e1, handled := dstTryFstat(fd, stat); handled {
		if e1 != 0 {
			err = errnoErr(e1)
		}
		return
	}
	return fstatFD(fd, stat)
}

func Seek(fd int, offset int64, whence int) (off int64, err error) {
	if r0, e1, handled := dstTrySeek(fd, offset, whence); handled {
		off = r0
		if e1 != 0 {
			err = errnoErr(e1)
		}
		return
	}
	if dstSimFenced && dstFenceActive() && !dstHostIOActive() {
		dstSyscallRefuse(SYS_LSEEK)
	}
	return seekFD(fd, offset, whence)
}

func Fsync(fd int) (err error) {
	if e1, handled := dstTryFsync(fd); handled {
		if e1 != 0 {
			err = errnoErr(e1)
		}
		return
	}
	return fsync(fd)
}

func Fdatasync(fd int) (err error) {
	if e1, handled := dstTryFdatasync(fd); handled {
		if e1 != 0 {
			err = errnoErr(e1)
		}
		return
	}
	return fdatasync(fd)
}

func Flock(fd int, how int) (err error) {
	if e1, handled := dstTryFlock(fd, how); handled {
		if e1 != 0 {
			err = errnoErr(e1)
		}
		return
	}
	return flock(fd, how)
}
