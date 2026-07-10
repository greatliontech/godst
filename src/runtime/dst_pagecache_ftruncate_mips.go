// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && mips

package runtime

import "internal/runtime/syscall/linux"

// dstFtruncate resizes the page cache. Big-endian MIPS o32 pads the 64-bit
// argument into an even register pair like its little-endian sibling, but
// packs the HIGH word first: the pair is read as one big-endian 64-bit value.
// syscall.Ftruncate encodes the same split (zsyscall_linux_mips.go passes
// length>>32 before length; zsyscall_linux_mipsle.go passes length first).
// Swapping the halves here would ask the kernel for a multi-terabyte file.
func dstFtruncate(fd int32, size int64) uintptr {
	_, _, errno := linux.Syscall6(dstSysFtruncate, uintptr(fd), 0,
		uintptr(uint32(size>>32)), uintptr(uint32(size)), 0, 0)
	return errno
}
