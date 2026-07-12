// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package syscall

import "unsafe"

// Split-safe raw-boundary dispatch.
//
// The named Linux wrappers (syscall.Read, .Fdatasync, .Flock, .Madvise, …)
// already route a virtual descriptor or a simulated mapping to the file backend.
// But `golang.org/x/sys/unix` — which real database code uses for
// Fdatasync/Madvise/ClockGettime — does not call those wrappers: its asm enters
// the generic trampolines directly. So `unix.Fdatasync(fd)` is
// `syscall.Syscall(SYS_FDATASYNC, fd, 0, 0)`, a different path through the
// interception boundary, which used to refuse an operation the model implements.
// This file makes the two paths one operation.
//
// TWO CONSTRAINTS SHAPE EVERY LINE BELOW, and both are about stack growth.
//
// (1) Syscall and Syscall6 are //go:uintptrkeepalive //go:nosplit, and the
// comment on them says why: "stack copying does not account for
// uintptrkeepalive, so the stack must not grow" — a uintptr argument that is a
// converted pointer is NOT adjusted when the stack moves. So a call that can
// grow the stack must never precede a FALL-THROUGH to the kernel: the address
// the trampoline then passes would be stale. dstRawDispatch is therefore nosplit
// itself, and decides "not mine" (handled=false) with cheap register tests only,
// before any call that can allocate or lock. Once it decides an operation IS the
// simulation's, it never falls through — it returns the backend's result or
// refuses. The uintptr→pointer conversions happen here, in nosplit code, so the
// splittable helpers receive REAL pointers, which a stack copy does adjust.
//
// (2) The helpers call into the os backend, which allocates and takes locks:
// they can grow the stack. That is fine before entersyscall (the goroutine has a
// P) and fatal after it. Syscall and Syscall6 fence BEFORE entersyscall, so they
// dispatch. RawSyscall and RawSyscall6 do not — Syscall reaches RawSyscall6
// after entersyscall, and RawSyscall may run post-fork with no P at all — so
// they keep refusing the virtual-fd range outright. A virtual fd handed to a raw
// trampoline still meets the fence; that is the documented shape.
//
// Anything outside the dispatched set — an unmodeled operation, a host fd, an
// address that is not a simulated mapping — is refused, or falls through to the
// existing allowlist/fence decision exactly as before.

// dstRawDispatch answers whether a raw syscall is the simulation's, and if so
// performs it. handled=false means "not mine, proceed to the kernel", and is
// reached only through tests that cannot grow the stack. It must stay nosplit
// for that guarantee to hold.
//
// //go:nocheckptr because a mapping operation's address arrives as a bare
// uintptr and must be turned back into the Go pointer it was: the trampoline's
// //go:uintptrkeepalive is what makes that provenance valid (the caller's
// pointer is alive across this call), and checkptr cannot see it. The
// conversion is the sanctioned uintptr→unsafe.Pointer(4) pattern, as in
// exec_linux.go's own dispatch of caller-supplied addresses.
//
//go:nosplit
//go:nocheckptr
func dstRawDispatch(trap, a1, a2, a3 uintptr) (r1 uintptr, err Errno, handled bool) {
	switch trap {
	case SYS_MADVISE, SYS_MPROTECT, SYS_MUNMAP:
		// A zero-length range names no mapping, so neither its address nor the
		// operation can establish simulated ownership. Match the named wrappers:
		// every empty mapping operation is EINVAL.
		if a2 == 0 {
			return 0, EINVAL, true
		}
		// A length that cannot be a Go slice is not a simulated mapping, and
		// unsafe.Slice would panic on it rather than refuse cleanly.
		if a1 == 0 || int(a2) < 0 {
			return 0, 0, false
		}
		// Convert here, in nosplit code: from now on the runtime sees a real
		// pointer and adjusts it if the stack moves under the helper.
		data := unsafe.Slice((*byte)(unsafe.Pointer(a1)), int(a2))
		return dstRawMapping(trap, data, a3)
	case SYS_FSYNC, SYS_FDATASYNC, SYS_FLOCK, SYS_CLOSE:
		if a1 < dstVirtualFDBase || a1 >= dstVirtualFDBase+dstVirtualFDCount {
			return 0, 0, false // a host descriptor: the allowlist decides
		}
		return dstRawFD(trap, int(a1), a2)
	}
	// Everything else on a virtual fd — read/write/pread/pwrite/lseek/fstat —
	// stays fenced at the raw boundary. Their argument shapes are not uniform
	// across the trampolines (offsets ride the 6-argument form; fstat's buffer
	// layout is arch-specific), and a SUT reaches them through os.File, whose
	// named wrappers already dispatch.
	return 0, 0, false
}

// dstRawMapping performs a mapping operation the simulation owns. It never
// reports handled=false: a bubble goroutine's madvise/mprotect/munmap on a range
// that is NOT a simulated mapping names host memory, which the boundary refuses
// (these traps were never on the allowlist), so refusing here issues exactly the
// fence's refusal — and issuing it here rather than falling through preserves
// the "no splittable call before a fall-through" rule.
//
//go:noinline
func dstRawMapping(trap uintptr, data []byte, a3 uintptr) (r1 uintptr, err Errno, handled bool) {
	switch trap {
	case SYS_MADVISE:
		if e, ok := dstTryMadvise(data, int(a3)); ok {
			return 0, e, true
		}
	case SYS_MPROTECT:
		if e, ok := dstTryMprotect(data, int(a3)); ok {
			return 0, e, true
		}
	case SYS_MUNMAP:
		if e, ok := dstTryMunmap(data); ok {
			return 0, e, true
		}
	}
	dstSyscallRefuse(trap)
	return 0, 0, true
}

// dstRawFD performs an operation on a virtual descriptor. Like dstRawMapping it
// never falls through: the number lies in the reserved range, so it is the
// simulation's whatever the hook answers (an unknown number answers EBADF).
//
//go:noinline
func dstRawFD(trap uintptr, fd int, a2 uintptr) (r1 uintptr, err Errno, handled bool) {
	switch trap {
	case SYS_FSYNC:
		if e, ok := dstTryFsync(fd); ok {
			return 0, e, true
		}
	case SYS_FDATASYNC:
		if e, ok := dstTryFdatasync(fd); ok {
			return 0, e, true
		}
	case SYS_FLOCK:
		if e, ok := dstTryFlock(fd, int(a2)); ok {
			return 0, e, true
		}
	case SYS_CLOSE:
		if e, ok := dstTryClose(fd); ok {
			return 0, e, true
		}
	}
	dstSyscallRefuse(trap)
	return 0, 0, true
}
