// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (386 || arm)

package syscall

import _ "unsafe"

func rawSeek(fd int, offset int64, whence int) (newoffset int64, err Errno)

//go:linkname seek
func seek(fd int, offset int64, whence int) (newoffset int64, err Errno) {
	if dstSimFenced {
		if dstFenceActive() && !dstHostIOActive() {
			dstSyscallRefuse(SYS__LLSEEK)
		}
	}
	return rawSeek(fd, offset, whence)
}

func seekFD(fd int, offset int64, whence int) (newoffset int64, err error) {
	newoffset, errno := seek(fd, offset, whence)
	if errno != 0 {
		return 0, errno
	}
	return newoffset, nil
}
