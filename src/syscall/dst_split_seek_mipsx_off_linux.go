// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst && linux && (mips || mipsle)

package syscall

import "unsafe"

func Seek(fd int, offset int64, whence int) (off int64, err error) {
	_, _, e := Syscall6(SYS__LLSEEK, uintptr(fd), uintptr(offset>>32), uintptr(offset), uintptr(unsafe.Pointer(&off)), uintptr(whence), 0)
	if e != 0 {
		err = errnoErr(e)
	}
	return
}
