// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && amd64

package syscall

import _ "unsafe"

// rawGettimeofday is the assembly entry (vDSO with a raw-syscall fallback);
// it enters the kernel through none of the fenced trampolines.
//
//go:noescape
func rawGettimeofday(tv *Timeval) (err Errno)

// gettimeofday is the fenced wrapper over the assembly entry, and the symbol
// golang.org/x/sys/unix enters by name (its amd64 assembly jumps to
// syscall·gettimeofday): exporting the raw entry instead would hand a
// bubble goroutine an unfenced host WALL-clock read.
//
//go:linkname gettimeofday
func gettimeofday(tv *Timeval) (err Errno) {
	if dstSimFenced {
		if dstFenceActive() && !dstHostIOActive() {
			dstSyscallRefuse(SYS_GETTIMEOFDAY)
		}
	}
	return rawGettimeofday(tv)
}
