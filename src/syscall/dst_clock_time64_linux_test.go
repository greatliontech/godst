// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (386 || arm)

package syscall_test

import (
	"fmt"
	"strings"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
	"unsafe"
)

// sysClockGettime64 is the 64-bit-time clock_gettime trap on 386/arm (the
// syscall package does not export it; mips carries the o32 4000 base and is
// covered by the same dispatch).
const sysClockGettime64 = 403

// TestDSTClockGettime64Virtual: the time64 clock_gettime trap returns the DST
// virtual base clock for CLOCK_MONOTONIC/CLOCK_BOOTTIME in __kernel_timespec
// layout (two int64s), advances with fake time, and stays fenced for other
// clock ids.
func TestDSTClockGettime64Virtual(t *testing.T) {
	type kernelTimespec struct{ Sec, Nsec int64 }
	read := func(clockid uintptr) (kernelTimespec, syscall.Errno) {
		var ts kernelTimespec
		_, _, errno := syscall.Syscall(sysClockGettime64, clockid, uintptr(unsafe.Pointer(&ts)), 0)
		return ts, errno
	}
	var mono0, mono1, boot0 kernelTimespec
	var err0, err1, errBoot syscall.Errno
	var realtimePanic any
	simulation.Run(1, func() {
		mono0, err0 = read(1) // CLOCK_MONOTONIC
		boot0, errBoot = read(7)
		time.Sleep(123 * time.Millisecond)
		mono1, err1 = read(1)
		func() {
			defer func() { realtimePanic = recover() }()
			read(0) // CLOCK_REALTIME stays fenced
		}()
	})
	if err0 != 0 || err1 != 0 || errBoot != 0 {
		t.Fatalf("clock_gettime64 errnos = %v %v %v, want 0", err0, err1, errBoot)
	}
	if mono0 != boot0 {
		t.Fatalf("boottime %v != monotonic %v (they coincide until a suspend model exists)", boot0, mono0)
	}
	d := (mono1.Sec-mono0.Sec)*1_000_000_000 + (mono1.Nsec - mono0.Nsec)
	if d != int64(123*time.Millisecond) {
		t.Fatalf("virtual monotonic advanced %dns, want 123ms exactly", d)
	}
	if realtimePanic == nil || !strings.Contains(fmt.Sprint(realtimePanic), "unsupported under deterministic simulation") {
		t.Fatalf("CLOCK_REALTIME via time64 trap panic = %v, want the fence shape", realtimePanic)
	}
}

func TestDSTClockGettime64InvalidPointers(t *testing.T) {
	type kernelTimespec struct{ Sec, Nsec int64 }
	runDSTClockInvalidPointerForms(t, sysClockGettime64, unsafe.Sizeof(kernelTimespec{}))
}

func TestDSTClockGettime32OverflowPrecedesCopyout(t *testing.T) {
	for _, pointer := range []uintptr{0, 1} {
		t.Run(fmt.Sprintf("pointer_%d", pointer), func(t *testing.T) {
			var r1, r2 uintptr
			var errno syscall.Errno
			simulation.Run(1, func() {
				time.Sleep((1 << 31) * time.Second)
				r1, r2, errno = syscall.Syscall(syscall.SYS_CLOCK_GETTIME, dstClockMonotonic, pointer, 0)
			})
			if r1 != ^uintptr(0) || r2 != 0 || errno != syscall.EOVERFLOW {
				t.Fatalf("overflowing native time32 with pointer %#x = (%#x, %#x, %v), want (%#x, 0, EOVERFLOW)", pointer, r1, r2, errno, ^uintptr(0))
			}
		})
	}
}

// TestDSTRawFcntl64Probes: these arches' fcntl is SYS_FCNTL64 — probe commands
// on an inherited host fd pass the allowlist, while the descriptor-minting
// F_DUPFD is refused, exactly like SYS_FCNTL on 64-bit arches.
func TestDSTRawFcntl64Probes(t *testing.T) {
	var probeErrno syscall.Errno
	var dupPanic any
	simulation.Run(1, func() {
		_, _, probeErrno = syscall.Syscall(syscall.SYS_FCNTL64, 1, syscall.F_GETFD, 0)
		func() {
			defer func() { dupPanic = recover() }()
			syscall.Syscall(syscall.SYS_FCNTL64, 1, syscall.F_DUPFD, 0)
		}()
	})
	if probeErrno != 0 {
		t.Fatalf("fcntl64(1, F_GETFD) = %v, want success (probe commands allowlisted)", probeErrno)
	}
	if dupPanic == nil || !strings.Contains(fmt.Sprint(dupPanic), "unsupported under deterministic simulation") {
		t.Fatalf("fcntl64(1, F_DUPFD) panic = %v, want the fence shape (minting command refused)", dupPanic)
	}
}
