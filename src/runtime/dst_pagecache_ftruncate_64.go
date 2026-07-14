// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (amd64 || arm64 || ppc64 || ppc64le || riscv64 || s390x)

package runtime

import "internal/runtime/syscall/linux"

// dstFtruncate resizes the page cache. On a 64-bit arch off_t is a register.
func dstFtruncate(fd int32, size int64) uintptr {
	_, _, errno := linux.Syscall6(dstSysFtruncate, uintptr(fd), uintptr(size), 0, 0, 0, 0)
	return errno
}
