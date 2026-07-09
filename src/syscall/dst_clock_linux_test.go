// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package syscall_test

import (
	"syscall"
	"testing"
	"testing/simulation"
	"time"
	"unsafe"
)

const (
	dstClockMonotonic = 1
	dstClockBoottime  = 7
)

func TestDSTClockGettimeVirtualMonotonic(t *testing.T) {
	const step = 5 * time.Second

	var mono0, boot0, raw0, raw60, sys60, mono1 syscall.Timespec
	var mono0Err, boot0Err, raw0Err, raw60Err, sys60Err, mono1Err syscall.Errno
	var nilErr syscall.Errno
	var realtimePanic any
	var wallBefore, wallAfter int64
	var hostMonoBefore, hostMonoAfter syscall.Timespec
	var hostMonoBeforeErr, hostMonoAfterErr syscall.Errno

	simulation.Run(1, func() {
		mono0, mono0Err = dstRawClockGettime(dstClockMonotonic)
		boot0, boot0Err = dstRawClockGettime(dstClockBoottime)
		raw0, raw0Err = dstRawClockGettimeRaw(dstClockMonotonic)
		raw60, raw60Err = dstRawClockGettimeRaw6(dstClockMonotonic)
		sys60, sys60Err = dstRawClockGettimeSyscall6(dstClockMonotonic)
		time.Sleep(123 * time.Millisecond)
		mono1, mono1Err = dstRawClockGettime(dstClockMonotonic)
		_, _, nilErr = syscall.Syscall(syscall.SYS_CLOCK_GETTIME, uintptr(dstClockMonotonic), 0, 0)
		func() {
			defer func() { realtimePanic = recover() }()
			var ts syscall.Timespec
			syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 0, uintptr(unsafe.Pointer(&ts)), 0)
		}()

		ready := make(chan struct{})
		stepped := make(chan struct{})
		done := make(chan struct{})
		go func() {
			simulation.Host("h", simulation.HostConfig{}, func() {
				wallBefore = time.Now().UnixNano()
				hostMonoBefore, hostMonoBeforeErr = dstRawClockGettime(dstClockMonotonic)
				close(ready)
				<-stepped
				wallAfter = time.Now().UnixNano()
				hostMonoAfter, hostMonoAfterErr = dstRawClockGettime(dstClockMonotonic)
			})
			close(done)
		}()
		<-ready
		simulation.StepClock("h", step)
		close(stepped)
		<-done
	})

	for name, err := range map[string]syscall.Errno{
		"CLOCK_MONOTONIC initial": mono0Err,
		"CLOCK_BOOTTIME initial":  boot0Err,
		"RawSyscall initial":      raw0Err,
		"RawSyscall6 initial":     raw60Err,
		"Syscall6 initial":        sys60Err,
		"CLOCK_MONOTONIC later":   mono1Err,
		"host monotonic before":   hostMonoBeforeErr,
		"host monotonic after":    hostMonoAfterErr,
	} {
		if err != 0 {
			t.Fatalf("%s err = %v, want 0", name, err)
		}
	}
	if got, want := dstTimespecNsec(boot0), dstTimespecNsec(mono0); got != want {
		t.Fatalf("CLOCK_BOOTTIME = %d, want CLOCK_MONOTONIC %d until suspend is modeled", got, want)
	}
	for name, ts := range map[string]syscall.Timespec{
		"RawSyscall":  raw0,
		"RawSyscall6": raw60,
		"Syscall6":    sys60,
	} {
		if got, want := dstTimespecNsec(ts), dstTimespecNsec(mono0); got != want {
			t.Fatalf("%s CLOCK_MONOTONIC = %d, want Syscall value %d", name, got, want)
		}
	}
	if got := dstTimespecNsec(mono1) - dstTimespecNsec(mono0); got != int64(123*time.Millisecond) {
		t.Fatalf("CLOCK_MONOTONIC advance = %d, want %d", got, 123*time.Millisecond)
	}
	if nilErr != syscall.EFAULT {
		t.Fatalf("CLOCK_MONOTONIC nil timespec err = %v, want EFAULT", nilErr)
	}
	if realtimePanic == nil {
		t.Fatalf("CLOCK_REALTIME did not hit the raw-syscall fence")
	}
	if got := wallAfter - wallBefore; got != int64(step) {
		t.Fatalf("host wall delta after StepClock = %d, want %d", got, step)
	}
	if got := dstTimespecNsec(hostMonoAfter) - dstTimespecNsec(hostMonoBefore); got != 0 {
		t.Fatalf("host CLOCK_MONOTONIC delta after StepClock = %d, want 0", got)
	}
}

func dstRawClockGettime(clockid uintptr) (syscall.Timespec, syscall.Errno) {
	var ts syscall.Timespec
	_, _, errno := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, clockid, uintptr(unsafe.Pointer(&ts)), 0)
	return ts, errno
}

func dstRawClockGettimeRaw(clockid uintptr) (syscall.Timespec, syscall.Errno) {
	var ts syscall.Timespec
	_, _, errno := syscall.RawSyscall(syscall.SYS_CLOCK_GETTIME, clockid, uintptr(unsafe.Pointer(&ts)), 0)
	return ts, errno
}

func dstRawClockGettimeRaw6(clockid uintptr) (syscall.Timespec, syscall.Errno) {
	var ts syscall.Timespec
	_, _, errno := syscall.RawSyscall6(syscall.SYS_CLOCK_GETTIME, clockid, uintptr(unsafe.Pointer(&ts)), 0, 0, 0, 0)
	return ts, errno
}

func dstRawClockGettimeSyscall6(clockid uintptr) (syscall.Timespec, syscall.Errno) {
	var ts syscall.Timespec
	_, _, errno := syscall.Syscall6(syscall.SYS_CLOCK_GETTIME, clockid, uintptr(unsafe.Pointer(&ts)), 0, 0, 0, 0)
	return ts, errno
}

func dstTimespecNsec(ts syscall.Timespec) int64 {
	return int64(ts.Sec)*1_000_000_000 + int64(ts.Nsec)
}
