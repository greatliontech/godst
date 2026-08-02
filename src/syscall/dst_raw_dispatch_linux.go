// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package syscall

import (
	"internal/goarch"
	"unsafe"
)

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
// address that is not a simulated mapping — falls through to the fence.

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
func dstRawDispatch(trap, a1, a2, a3, a4, a5, a6 uintptr) (r1 uintptr, err Errno, handled bool) {
	if trap == dstSysRenameat2 {
		// renameat2(olddirfd, oldpath, newdirfd, newpath, flags): only the
		// AT_FDCWD-relative form is the simulation's — a dirfd-relative form
		// names a virtual directory fd the model does not resolve renames
		// relative to; the fence decides those. The path
		// pointers convert here, in nosplit code, so the splittable helper
		// receives real pointers a stack copy adjusts.
		if int(a1) != _AT_FDCWD || int(a3) != _AT_FDCWD || a2 == 0 || a4 == 0 {
			return 0, 0, false
		}
		return dstRawRenameat2((*byte)(unsafe.Pointer(a2)), (*byte)(unsafe.Pointer(a4)), a5)
	}
	if trap == SYS_FUTEX {
		// futex(uaddr, op, val, timeout, uaddr2, val3): only the SHARED
		// FUTEX_WAIT / FUTEX_WAKE forms are the simulation's — PRIVATE and
		// every other op fall through to the fence. A nil address names no
		// mapping (the fence decides); the timespec pointer converts here,
		// in nosplit code, like every dispatched pointer.
		op := int(a2)
		if (op != 0 && op != 1) || a1 == 0 {
			return 0, 0, false
		}
		var ts *Timespec
		if a4 != 0 {
			ts = (*Timespec)(unsafe.Pointer(a4))
		}
		return dstRawFutex((*uint32)(unsafe.Pointer(a1)), op, uint32(a3), ts)
	}
	if trap == dstSysSetsockopt || trap == dstSysGetsockopt {
		// setsockopt(fd, level, optname, optval, optlen) /
		// getsockopt(fd, level, optname, optval, *optlen): only a virtual
		// SOCKET descriptor is the simulation's — a host fd falls through to
		// the fence. The pointer conversions happen here, in nosplit code, so
		// the splittable helpers receive real pointers a stack copy adjusts.
		if a1 < dstVirtualSockFDBase || a1 >= dstVirtualSockFDBase+dstVirtualSockFDCount {
			return 0, 0, false
		}
		return dstRawSockopt(trap == dstSysSetsockopt, a1, a2, a3, a4, a5)
	}
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
			return 0, 0, false // a host descriptor: the fence decides
		}
		return dstRawFD(trap, int(a1), a2)
	case SYS_FALLOCATE:
		// fallocate(fd, mode, offset, len) — the preallocation call WAL-shaped
		// stores reach through unix.Fallocate. The trampoline shapes differ by
		// word size: 64-bit arches pass (fd, mode, off, len); 32-bit split off
		// and len into lo/hi register pairs (x/sys and the named wrapper agree).
		// Register tests and arithmetic only — nosplit-safe — before the
		// splittable helper.
		if a1 < dstVirtualFDBase || a1 >= dstVirtualFDBase+dstVirtualFDCount {
			return 0, 0, false // a host descriptor: the fence decides
		}
		var off, length int64
		if goarch.PtrSize == 8 {
			off, length = int64(a3), int64(a4)
		} else {
			off = int64(uint64(a3) | uint64(a4)<<32)
			length = int64(uint64(a5) | uint64(a6)<<32)
		}
		return dstRawFallocate(int(a1), int(a2), off, length)
	}
	// Everything else on a virtual fd — read/write/pread/pwrite/lseek/fstat —
	// stays fenced at the raw boundary. Their argument shapes are not uniform
	// across the trampolines (offsets ride the 6-argument form; fstat's buffer
	// layout is arch-specific), and a SUT reaches them through os.File, whose
	// named wrappers already dispatch.
	return 0, 0, false
}

// dstRawSockopt performs a sockopt on a virtual socket descriptor. Like
// dstRawFD it never falls through: the number lies in the reserved range, so
// the operation is the simulation's whatever the hook answers (no registered
// hook — net not linked — meets the fence). Argument shapes follow the
// kernel: optlen is a value for setsockopt and a pointer for getsockopt
// (socklen_t, uint32 on linux); a NULL optval with a nonzero length is
// EFAULT, a negative or absurd length EINVAL (the kernel caps sockopt
// copies), before any pointer is dereferenced. The uintptr→pointer
// conversions ride the trampoline's uintptrkeepalive exactly as
// dstRawMapping's do.
//
//go:nosplit
//go:nocheckptr
func dstRawSockopt(set bool, a1, a2, a3, a4, a5 uintptr) (r1 uintptr, err Errno, handled bool) {
	const maxOptLen = 1 << 20 // far above any real option; a larger len is not a sockopt
	fd, level, opt := int(a1), int(a2), int(a3)
	if set {
		n := int(a5)
		if n < 0 || n > maxOptLen {
			return 0, EINVAL, true
		}
		if a4 == 0 && n > 0 {
			return 0, EFAULT, true
		}
		var val []byte
		if n > 0 {
			val = unsafe.Slice((*byte)(unsafe.Pointer(a4)), n)
		}
		return dstRawSetsockopt(fd, level, opt, val)
	}
	if a5 == 0 {
		return 0, EFAULT, true
	}
	lenp := (*uint32)(unsafe.Pointer(a5))
	n := int(int32(*lenp))
	if n < 0 || n > maxOptLen {
		return 0, EINVAL, true
	}
	if a4 == 0 && n > 0 {
		return 0, EFAULT, true
	}
	var val []byte
	if n > 0 {
		val = unsafe.Slice((*byte)(unsafe.Pointer(a4)), n)
	}
	return dstRawGetsockopt(fd, level, opt, val, lenp)
}

//go:noinline
func dstRawSetsockopt(fd, level, opt int, val []byte) (r1 uintptr, err Errno, handled bool) {
	if e, ok := dstTrySetsockopt(fd, level, opt, val); ok {
		return 0, e, true
	}
	dstSyscallRefuse(dstSysSetsockopt)
	return 0, 0, true
}

//go:noinline
func dstRawGetsockopt(fd, level, opt int, val []byte, lenp *uint32) (r1 uintptr, err Errno, handled bool) {
	n, e, ok := dstTryGetsockopt(fd, level, opt, val)
	if !ok {
		dstSyscallRefuse(dstSysGetsockopt)
		return 0, 0, true
	}
	if e == 0 {
		*lenp = uint32(n)
	}
	return 0, e, true
}

// dstSysSetsockopt / dstSysGetsockopt are setsockopt(2)/getsockopt(2)'s
// numbers for the running architecture, the dstSysRenameat2 pattern. 386
// has no direct numbers (the socketcall era); its multiplexed path is
// dispatched at the fenced socketcall wrapper instead, and the sentinel
// here matches no trap.
var dstSysSetsockopt, dstSysGetsockopt = func() (uintptr, uintptr) {
	switch goarch.GOARCH {
	case "amd64":
		return 54, 55
	case "386":
		return ^uintptr(0), ^uintptr(0)
	case "arm":
		return 294, 295
	case "arm64", "riscv64":
		return 208, 209
	case "ppc64", "ppc64le":
		return 339, 340
	case "s390x":
		return 366, 365
	}
	panic("dst: sockopt numbers unknown for " + goarch.GOARCH)
}()

// dstRawMapping performs a mapping operation the simulation owns. It never
// reports handled=false: a bubble goroutine's madvise/mprotect/munmap on a range
// that is NOT a simulated mapping names host memory, which the boundary refuses,
// so refusing here issues exactly the
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

// dstRawFallocate performs a preallocation on a virtual descriptor. Like
// dstRawFD it never falls through: the number lies in the reserved range, so
// the operation is the simulation's whatever the hook answers.
//
//go:noinline
func dstRawFallocate(fd int, mode int, off, length int64) (r1 uintptr, err Errno, handled bool) {
	if e, ok := dstTryFallocate(fd, mode, off, length); ok {
		return 0, e, true
	}
	dstSyscallRefuse(SYS_FALLOCATE)
	return 0, 0, true
}

// dstRawRenameat2 performs an AT_FDCWD renameat2 the simulation owns. Like
// dstRawMapping it never falls through: the fence would refuse the operation
// anyway, and refusing here preserves the no-splittable-call-before-fall-
// through rule. The flags allowlist lives in the os backend (0 and
// RENAME_NOREPLACE modeled; RENAME_EXCHANGE / RENAME_WHITEOUT answer EINVAL,
// the kernel's own shape for a filesystem without the capability).
//
//go:noinline
func dstRawRenameat2(old, new *byte, flags uintptr) (r1 uintptr, err Errno, handled bool) {
	oldpath, ok := dstCString(old)
	if !ok {
		return 0, ENAMETOOLONG, true
	}
	newpath, ok := dstCString(new)
	if !ok {
		return 0, ENAMETOOLONG, true
	}
	if e, ok := dstTryRenameat2(oldpath, newpath, int(flags)); ok {
		return 0, e, true
	}
	dstSyscallRefuse(dstSysRenameat2)
	return 0, 0, true
}

// dstCString reads a NUL-terminated C string into a Go string, bounded the
// way the kernel's getname bounds a path copy: no NUL within PATH_MAX (4096)
// answers ok=false (the caller's ENAMETOOLONG), never an unbounded walk into
// unmapped memory a real kernel would refuse with a recoverable errno.
// Splittable — callers hold real pointers, adjusted on stack growth.
func dstCString(p *byte) (string, bool) {
	const pathMax = 4096
	n := 0
	for ; n < pathMax; n++ {
		if *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) == 0 {
			return string(unsafe.Slice(p, n)), true
		}
	}
	return "", false
}

// dstSysRenameat2 is renameat2(2)'s number for the running architecture. The
// frozen zsysnum tables predate the syscall (Linux 3.15) on several arches,
// so the dst dispatch carries it for the dst-supported set. Arches outside
// the set cannot build -tags dst at all (compile-time refusal), so the
// switch is total.
var dstSysRenameat2 = func() uintptr {
	switch goarch.GOARCH {
	case "amd64":
		return 316
	case "386":
		return 353
	case "arm":
		return 382
	case "arm64", "riscv64":
		return 276
	case "ppc64", "ppc64le":
		return 357
	case "s390x":
		return 347
	}
	panic("dst: renameat2 number unknown for " + goarch.GOARCH)
}()

// dstRawFutex performs a shared FUTEX_WAIT/FUTEX_WAKE the simulation owns.
// Like the other noinline helpers it never falls through after the arm has
// claimed the operation — an address outside the caller's simulated
// mappings, or a hook miss, issues the fence's refusal here. A WAIT's
// relative timespec converts to bubble-clock nanoseconds; malformed
// timespecs answer EINVAL, as futex(2) does.
//
//go:noinline
func dstRawFutex(addr *uint32, op int, val uint32, ts *Timespec) (r1 uintptr, err Errno, handled bool) {
	var timeoutNs int64
	hasTimeout := false
	if op == 0 && ts != nil {
		sec, nsec := int64(ts.Sec), int64(ts.Nsec)
		if sec < 0 || nsec < 0 || nsec >= 1e9 {
			return 0, EINVAL, true
		}
		const maxSec = int64(1) << 33 // caps well under int64 ns overflow
		if sec > maxSec {
			sec = maxSec
		}
		timeoutNs = sec*1e9 + nsec
		hasTimeout = true
	}
	if n, e, ok := dstTryFutex(addr, op, val, timeoutNs, hasTimeout); ok {
		return uintptr(n), e, true
	}
	dstSyscallRefuse(SYS_FUTEX)
	return 0, 0, true
}
