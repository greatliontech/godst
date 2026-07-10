// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && 386

package runtime

import "internal/runtime/syscall/linux"

// dstFtruncate resizes the page cache. 386's off_t is 32 bits, so this is
// ftruncate64(fd, offset_low, offset_high) — x86 passes the halves in
// consecutive registers, with no alignment padding.
func dstFtruncate(fd int32, size int64) uintptr {
	_, _, errno := linux.Syscall6(dstSysFtruncate, uintptr(fd),
		uintptr(uint32(size)), uintptr(uint32(size>>32)), 0, 0, 0)
	return errno
}
