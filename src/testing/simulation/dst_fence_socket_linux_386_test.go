// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && 386

package simulation

import (
	"syscall"
	"testing"
	"unsafe"
)

const socketSubcall = 1

func rawSocketSyscall(typ int) (uintptr, syscall.Errno) {
	args := [...]uintptr{uintptr(syscall.AF_INET), uintptr(typ), 0}
	fd, _, errno := syscall.RawSyscall(syscall.SYS_SOCKETCALL, socketSubcall, uintptr(unsafe.Pointer(&args[0])), 0)
	return fd, errno
}

// xsysSocketcall pulls syscall.socketcall by linkname — the exact entry
// golang.org/x/sys/unix's 386 assembly jumps to (JMP syscall·socketcall). A
// pull reference links only against a linkname-exported symbol, so this file
// fails to BUILD if the push linkname on the fenced wrapper is ever dropped
// (which would also break every 386 binary importing x/sys/unix).
//
//go:linkname xsysSocketcall syscall.socketcall
func xsysSocketcall(call int, a0, a1, a2, a3, a4, a5 uintptr) (n int, err syscall.Errno)

// TestDSTSocketcallEntryFenced: the socketcall entry x/sys/unix's assembly
// resolves to is the FENCED wrapper — a bubble goroutine reaching it (as
// unix.Bind and friends do on 386) is refused, not handed real host I/O.
func TestDSTSocketcallEntryFenced(t *testing.T) {
	var panicked bool
	Run(1, func() {
		panicked = dstDidPanic(func() {
			// The a0..a5 slots ARE the subcall's argument array (the asm
			// passes &a0 to the kernel), matching x/sys's calling shape.
			xsysSocketcall(socketSubcall, uintptr(syscall.AF_INET), uintptr(syscall.SOCK_STREAM), 0, 0, 0, 0)
		})
	})
	if !panicked {
		t.Errorf("the socketcall entry x/sys/unix links against did not refuse in-bubble")
	}
}
