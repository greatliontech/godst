// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (arm || mipsle)

package runtime

import "internal/runtime/syscall/linux"

// dstFtruncate resizes the page cache. off_t is 32 bits on these arches, so
// this is ftruncate64. ARM's EABI and MIPS's o32 both require a 64-bit
// argument to start in an even-numbered register, so the halves land in the
// third and fourth slots and the second is padding the kernel ignores. These
// are little-endian, so the low word goes first — see the big-endian sibling.
func dstFtruncate(fd int32, size int64) uintptr {
	_, _, errno := linux.Syscall6(dstSysFtruncate, uintptr(fd), 0,
		uintptr(uint32(size)), uintptr(uint32(size>>32)), 0, 0)
	return errno
}
