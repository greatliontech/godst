// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package syscall

// dstSyscallAllowedTrap reports whether trap is in the I/O-on-an-existing-fd
// allowlist: the family that can only name a pre-run host handle, so it is the
// sanctioned inherited-handle stance (a simulated file never exposes an fd, so
// an fd argument can only be a real host descriptor — stdio and the like). See
// design.md "The interception boundary". Everything outside the family is
// fenced: read/write/close on inherited handles keep working, but a bubble
// goroutine minting a new host resource (open, socket, pipe, dup, mmap, execve)
// is refused. ioctl is included so isatty probes on real stdio still work.
//
// nosplit so the raw-syscall trampolines can call it without growing their
// uintptrkeepalive stack.
//
//go:nosplit
func dstSyscallAllowedTrap(trap uintptr) bool {
	switch trap {
	case SYS_READ, SYS_WRITE, SYS_CLOSE, SYS_LSEEK, SYS_FSTAT,
		SYS_FCNTL, SYS_IOCTL, SYS_PREAD64, SYS_PWRITE64:
		return true
	}
	return false
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
