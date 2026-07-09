// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && loong64

package syscall

//go:nosplit
func dstSyscallAllowedArchTrap(trap uintptr) bool {
	return false
}

//go:nosplit
func dstSyscallVirtualFDArchTrap(trap uintptr) bool {
	return false
}

//go:nosplit
func dstSyscallFcntlArchTrap(trap uintptr) bool {
	return false // loong64 has no fcntl64 trap
}
