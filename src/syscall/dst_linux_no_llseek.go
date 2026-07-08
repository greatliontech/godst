// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && !(386 || arm || mips || mipsle)

package syscall

//go:nosplit
func dstSyscallAllowedArchTrap(trap uintptr) bool {
	return false
}

//go:nosplit
func dstSyscallVirtualFDArchTrap(trap uintptr) bool {
	return false
}
