// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (386 || arm || mips || mipsle)

package syscall

//go:nosplit
func dstSyscallAllowedArchTrap(trap uintptr) bool {
	// SYS_FCNTL64 is these arches' fcntl: allowlisted for the same probe
	// commands as SYS_FCNTL, with the same argument-aware minting refusal
	// (dstSyscallFcntlArchTrap feeds dstSyscallMintingFcntl).
	return trap == SYS__LLSEEK || trap == SYS_FSTAT || trap == SYS_FSTAT64 || trap == SYS_FCNTL64
}

//go:nosplit
func dstSyscallVirtualFDArchTrap(trap uintptr) bool {
	return trap == SYS__LLSEEK || trap == SYS_FSTAT || trap == SYS_FSTAT64 || trap == SYS_FCNTL64
}

//go:nosplit
func dstSyscallFcntlArchTrap(trap uintptr) bool {
	return trap == SYS_FCNTL64
}
