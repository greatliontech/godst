// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && loong64

package syscall

//go:nosplit
func dstSyscallVirtualFDArchTrap(trap uintptr) bool {
	return trap == SYS_STATX
}

// statx uses dirfd only with a relative path or AT_EMPTY_PATH. The raw
// descriptor form modeled by Fstat is the empty-path form.
//
//go:nosplit
func dstSyscallPageCacheFDArchTrap(trap, flags uintptr) bool {
	return trap == SYS_STATX && flags&_AT_EMPTY_PATH != 0
}

func dstSyscallFstatTrap() uintptr { return SYS_STATX }
