// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package syscall

// Stock build: the raw-syscall fence folds away (dstSimFenced is false, so the
// branch in the trampolines is dead-code-eliminated). These stubs exist only so
// that dead branch still type-checks.

func dstSyscallAllowedTrap(trap uintptr) bool { return true }

func dstSyscallVirtualFDTrap(trap, fd uintptr) bool { return false }

func dstSetVirtualFDActive(fd uintptr, active bool) {}

func dstClearVirtualFDs() {}

func dstSyscallRefuse(trap uintptr) {}
