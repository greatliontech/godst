// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// loong64 is excluded on ABI grounds, not as variant selection: its Linux
// port has no fstat trap (SYS_FSTAT does not exist; stat is statx-only), so
// this file cannot compile there. The dst build tag is refused at compile
// time on loong64 anyway (runtime/dst_arch_unsupported.go).
//go:build dst && linux && !(386 || arm || loong64)

package syscall

//go:nosplit
func dstSyscallVirtualFDArchTrap(trap uintptr) bool {
	return trap == SYS_FSTAT
}

//go:nosplit
func dstSyscallPageCacheFDArchTrap(trap, _ uintptr) bool { return trap == SYS_FSTAT }

func dstSyscallFstatTrap() uintptr { return SYS_FSTAT }
