// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package syscall

import _ "unsafe" // for go:linkname

const dstVirtualFDBase = 1 << 30
const dstVirtualFDCount = 1 << 20

// The virtual SOCKET descriptor range, disjoint from (and adjacent to) the
// virtual file range: the numbers net issues for simulated sockets so a
// Dialer.Control / ListenConfig.Control callback's raw setsockopt/getsockopt
// (the golang.org/x/sys/unix path) can be routed to the simulated option
// layer. net declares the same constants (net/dst_sockopt.go); the two
// packages share the range by construction, like os and syscall share the
// file range.
const dstVirtualSockFDBase = dstVirtualFDBase + dstVirtualFDCount
const dstVirtualSockFDCount = 1 << 20

// dstSyscallVirtualFDTrap reports whether an fd-carrying trap names
// a number in the reserved virtual-fd range [dstVirtualFDBase,
// dstVirtualFDBase+dstVirtualFDCount). The WHOLE range is refused at the raw
// boundary — issued or not — matching the named-wrapper side, which owns the
// range outright (an unknown in-range number is EBADF there, never a host fd).
// A pure range check needs no cross-package issued-fd state, so there is
// nothing for a racing raw syscall to read stale: the refusal is a function of
// the number alone. (A genuine host fd in this range would need fs.nr_open
// raised beyond 2^30 — the reserved range is the simulation's namespace,
// recorded in the spec.)
//
//go:nosplit
func dstSyscallVirtualFDTrap(trap, fd uintptr) bool {
	if fd < dstVirtualFDBase || fd >= dstVirtualFDBase+dstVirtualFDCount {
		return false
	}
	switch trap {
	case SYS_READ, SYS_WRITE, SYS_CLOSE, SYS_LSEEK,
		SYS_FCNTL, SYS_IOCTL, SYS_PREAD64, SYS_PWRITE64:
		return true
	}
	return dstSyscallVirtualFDArchTrap(trap)
}

// dstSyscallPageCacheFDTrap reports whether an fd-carrying trap
// names a live harness page-cache descriptor. Those fds are INVISIBLE in the
// simulated fd namespace: the caller answers EBADF — exactly what a fd the
// SUT never opened would get — never a panic, because sweeping unknown fd
// numbers with close is legal production shape (daemonize), and never host
// I/O, because a passed-through close would kill a live file's cache (fatal
// at the next resize or mmap) and a freed number reused by a later
// memfd_create would silently alias another file's bytes.
//
//go:nosplit
func dstSyscallPageCacheFDTrap(trap, fd, a3 uintptr) bool {
	if !dstPageCacheFDReserved(fd) {
		return false
	}
	switch trap {
	case SYS_READ, SYS_WRITE, SYS_CLOSE, SYS_LSEEK,
		SYS_FCNTL, SYS_IOCTL, SYS_PREAD64, SYS_PWRITE64:
		return true
	}
	return dstSyscallPageCacheFDArchTrap(trap, a3)
}

// dstSyscallHostClose reports whether a bubble goroutine is closing a real
// (non-virtual) fd number. Such a close must NEVER reach the real kernel: the
// harness and the simulated process share the one real fd table, so a close
// of a currently-free number races the harness assigning that number to a new
// fd (a page-cache memfd, or the runtime's own lazily-created netpoll epoll
// fd) between the fence check and the kernel dispatch — the closing M is
// already mid-flight when sysmon's syscall retake hands its P to the
// allocating goroutine. A bubble goroutine can never MINT a real fd (the
// interception boundary refuses open/socket/pipe/dup), so it owns no real fd
// to legitimately close; every real close is a daemonize-style sweep of a
// number outside its simulated namespace. Answered EBADF — exactly what a fd
// it never opened returns — closing the TOCTOU window for the whole host-fd
// space rather than re-homing one fd class. Virtual-range closes are handled
// by the named registry (dstFDClose); a RAW close of a virtual number is an
// escape refused separately (dstSyscallVirtualFDTrap).
//
//go:nosplit
func dstSyscallHostClose(trap, fd uintptr) bool {
	if trap != SYS_CLOSE {
		return false
	}
	return fd < dstVirtualFDBase || fd >= dstVirtualFDBase+dstVirtualFDCount
}

// dstSyscallRefuse panics with the standard unsupported-under-simulation shape,
// naming the fenced trap number so the escape is diagnosable (a raw syscall has
// no honest call site the way os.File.Fd does). It is deliberately NOT nosplit
// and NOT inlined: it is the cold refusal path, so the trampolines stay nosplit
// for the common fence-passing path, and the panic — which grows the stack —
// runs only once we have decided not to perform the syscall, where the
// trampoline's uintptr arguments are already dead. Reached only from a genuine
// bubble goroutine (never the norace post-fork child, which reads false), so
// panicking here always has a healthy stack and a P.
//
//go:noinline
func dstSyscallRefuse(trap uintptr) {
	panic("syscall: raw syscall " + dstUitoa(uint(trap)) + " unsupported under deterministic simulation")
}

// dstUitoa renders trap in decimal without pulling a formatting dependency into
// the syscall package. Cold path only.
func dstUitoa(v uint) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
