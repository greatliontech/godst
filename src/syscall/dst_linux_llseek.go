// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (386 || arm)

package syscall

//go:nosplit
func dstSyscallVirtualFDArchTrap(trap uintptr) bool {
	return trap == SYS__LLSEEK || trap == SYS_FSTAT || trap == SYS_FSTAT64 || trap == SYS_FCNTL64
}

//go:nosplit
func dstSyscallPageCacheFDArchTrap(trap, _ uintptr) bool {
	return trap == SYS__LLSEEK || trap == SYS_FSTAT || trap == SYS_FSTAT64 || trap == SYS_FCNTL64
}

func dstSyscallFstatTrap() uintptr { return SYS_FSTAT64 }
