// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && loong64

package main

import (
	"syscall"
	"unsafe"
)

func dstPageCacheFstatRawFP(fd int) error {
	var path [1]byte
	var stat [256]byte
	_, _, errno := syscall.Syscall6(syscall.SYS_STATX, uintptr(fd), uintptr(unsafe.Pointer(&path[0])), 0x800|0x1000, 0x7ff, uintptr(unsafe.Pointer(&stat[0])), 0)
	return errno
}
