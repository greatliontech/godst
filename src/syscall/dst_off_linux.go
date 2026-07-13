// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package syscall

// Stock build: the raw-syscall fence folds away (dstSimFenced is false, so the
// branch in the trampolines is dead-code-eliminated). These stubs exist only so
// that dead branch still type-checks.

func dstSyscallVirtualFDTrap(trap, fd uintptr) bool { return false }

func dstSyscallPageCacheFDTrap(trap, fd, a3 uintptr) bool { return false }

func dstSyscallHostClose(trap, fd uintptr) bool { return false }

func dstSyscallFstatTrap() uintptr { return 0 }

func dstHostIOActive() bool { return false }

func dstSyscallRefuse(trap uintptr) {}

func dstRawDispatch(trap, a1, a2, a3 uintptr) (r1 uintptr, err Errno, handled bool) {
	return 0, 0, false
}
