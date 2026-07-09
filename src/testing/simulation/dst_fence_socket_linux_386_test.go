// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && 386

package simulation

import (
	"syscall"
	"unsafe"
)

const socketSubcall = 1

func rawSocketSyscall(typ int) (uintptr, syscall.Errno) {
	args := [...]uintptr{uintptr(syscall.AF_INET), uintptr(typ), 0}
	fd, _, errno := syscall.RawSyscall(syscall.SYS_SOCKETCALL, socketSubcall, uintptr(unsafe.Pointer(&args[0])), 0)
	return fd, errno
}
