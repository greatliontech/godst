// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package syscall_test

import (
	"bytes"
	"fmt"
	"internal/testenv"
	"os"
	"strconv"
	"strings"
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
	if realtimePanic == nil || !strings.Contains(fmt.Sprint(realtimePanic), "unsupported under deterministic simulation") {
		t.Fatalf("CLOCK_REALTIME panic = %v, want the raw-syscall fence's unsupported-under-simulation shape", realtimePanic)
	}
	if got := wallAfter - wallBefore; got != int64(step) {
		t.Fatalf("host wall delta after StepClock = %d, want %d", got, step)
	}
	if got := dstTimespecNsec(hostMonoAfter) - dstTimespecNsec(hostMonoBefore); got != 0 {
		t.Fatalf("host CLOCK_MONOTONIC delta after StepClock = %d, want 0", got)
	}
}

func TestDSTClockGettimeInvalidPointers(t *testing.T) {
	runDSTClockInvalidPointerForms(t, syscall.SYS_CLOCK_GETTIME, unsafe.Sizeof(syscall.Timespec{}))
}

func TestDSTClockGettimeCopyoutAllocationFree(t *testing.T) {
	var allocs float64
	var errno syscall.Errno
	simulation.Run(1, func() {
		allocs = testing.AllocsPerRun(10, func() {
			var ts syscall.Timespec
			_, _, errno = syscall.Syscall(syscall.SYS_CLOCK_GETTIME, dstClockMonotonic, uintptr(unsafe.Pointer(&ts)), 0)
		})
	})
	if errno != 0 {
		t.Fatalf("clock_gettime errno = %v, want 0", errno)
	}
	if allocs != 0 {
		t.Fatalf("clock_gettime copyout allocations = %v, want 0", allocs)
	}
}

func TestDSTClockGettimeInvalidPointerChild(t *testing.T) {
	form := os.Getenv("GO_DST_CLOCK_FORM")
	if form == "" {
		t.Skip("clock pointer child")
	}
	trap64, err := strconv.ParseUint(os.Getenv("GO_DST_CLOCK_TRAP"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	size64, err := strconv.ParseUint(os.Getenv("GO_DST_CLOCK_SIZE"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	trap, size := uintptr(trap64), uintptr(size64)
	pageSize := syscall.Getpagesize()

	unmapped, err := syscall.Mmap(-1, 0, pageSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Fatal(err)
	}
	unmappedAddr := uintptr(unsafe.Pointer(&unmapped[0]))
	if err := syscall.Munmap(unmapped); err != nil {
		t.Fatal(err)
	}

	readOnly, err := syscall.Mmap(-1, 0, pageSize, syscall.PROT_READ, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(readOnly)

	partial, err := syscall.Mmap(-1, 0, 2*pageSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mprotect(partial[pageSize:], syscall.PROT_NONE); err != nil {
		t.Fatal(err)
	}
	partialWritable := partial[pageSize-int(size/2) : pageSize]
	for i := range partialWritable {
		partialWritable[i] = 0xcc
	}
	defer func() {
		syscall.Mprotect(partial[pageSize:], syscall.PROT_READ|syscall.PROT_WRITE)
		syscall.Munmap(partial)
	}()

	valid, err := syscall.Mmap(-1, 0, int(size)+2, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(valid)
	valid[0], valid[len(valid)-1] = 0xa5, 0x5a

	call := func(pointer uintptr) (uintptr, uintptr, syscall.Errno) {
		switch form {
		case "Syscall":
			return syscall.Syscall(trap, dstClockMonotonic, pointer, 0)
		case "Syscall6":
			return syscall.Syscall6(trap, dstClockMonotonic, pointer, 0, 0, 0, 0)
		case "RawSyscall":
			return syscall.RawSyscall(trap, dstClockMonotonic, pointer, 0)
		case "RawSyscall6":
			return syscall.RawSyscall6(trap, dstClockMonotonic, pointer, 0, 0, 0, 0)
		default:
			t.Fatalf("unknown form %q", form)
			return 0, 0, 0
		}
	}

	var virtualValue []byte
	invalid := []struct {
		name    string
		pointer uintptr
	}{
		{"nil", 0},
		{"low", 1},
		{"unmapped", unmappedAddr},
		{"read-only", uintptr(unsafe.Pointer(&readOnly[0]))},
		{"partial", uintptr(unsafe.Pointer(&partial[pageSize-int(size/2)]))},
		{"wrapping", ^uintptr(0) - (size - 2)},
	}
	simulation.Run(1, func() {
		r1, r2, errno := call(uintptr(unsafe.Pointer(&valid[1])))
		if r1 != 0 || r2 != 0 || errno != 0 {
			t.Fatalf("%s valid unaligned = (%#x, %#x, %v), want success", form, r1, r2, errno)
		}
		virtualValue = append(virtualValue, valid[1:1+int(size)]...)

		if unsafe.Sizeof(uintptr(0)) == 8 {
			f, err := os.OpenFile("/clock-copyout", os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatalf("OpenFile simulated mapping: %v", err)
			}
			defer f.Close()
			if _, err := f.Write(make([]byte, size)); err != nil {
				t.Fatalf("Write simulated mapping: %v", err)
			}
			simulated, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
			if err != nil {
				t.Fatalf("Mmap simulated mapping: %v", err)
			}
			defer syscall.Munmap(simulated)
			r1, r2, errno = call(uintptr(unsafe.Pointer(&simulated[0])))
			if r1 != ^uintptr(0) || r2 != 0 || errno != syscall.EFAULT {
				t.Errorf("%s simulated read-only mapping = (%#x, %#x, %v), want (%#x, 0, EFAULT)", form, r1, r2, errno, ^uintptr(0))
			}
			if err := syscall.Mprotect(simulated, syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
				t.Fatalf("Mprotect simulated mapping: %v", err)
			}
			r1, r2, errno = call(uintptr(unsafe.Pointer(&simulated[0])))
			if r1 != 0 || r2 != 0 || errno != 0 {
				t.Errorf("%s simulated writable mapping = (%#x, %#x, %v), want success", form, r1, r2, errno)
			} else if !bytes.Equal(simulated, virtualValue) {
				t.Errorf("%s simulated writable mapping = %x, want virtual value %x", form, simulated, virtualValue)
			}
		}

		for _, tc := range invalid {
			r1, r2, errno = call(tc.pointer)
			if r1 != ^uintptr(0) || r2 != 0 || errno != syscall.EFAULT {
				t.Errorf("%s %s = (%#x, %#x, %v), want (%#x, 0, EFAULT)", form, tc.name, r1, r2, errno, ^uintptr(0))
			}
			if tc.name == "partial" {
				for i, got := range partialWritable {
					if got != 0xcc && got != virtualValue[i] {
						t.Errorf("%s partial byte %d = %#x, want unchanged %#x or virtual byte %#x", form, i, got, byte(0xcc), virtualValue[i])
					}
				}
			}
		}
	})
	if valid[0] != 0xa5 || valid[len(valid)-1] != 0x5a {
		t.Fatalf("%s valid unaligned write changed canaries: first=%#x last=%#x", form, valid[0], valid[len(valid)-1])
	}
}

func runDSTClockInvalidPointerForms(t *testing.T, trap, size uintptr) {
	t.Helper()
	if testing.Short() {
		t.Skip("-short: skips invalid-pointer subprocesses")
	}
	testenv.MustHaveExec(t)
	for _, form := range []string{"Syscall", "Syscall6", "RawSyscall", "RawSyscall6"} {
		t.Run(form, func(t *testing.T) {
			cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestDSTClockGettimeInvalidPointerChild$")
			cmd = testenv.CleanCmdEnv(cmd)
			cmd.Env = append(cmd.Env,
				"GO_DST_CLOCK_FORM="+form,
				"GO_DST_CLOCK_TRAP="+strconv.FormatUint(uint64(trap), 10),
				"GO_DST_CLOCK_SIZE="+strconv.FormatUint(uint64(size), 10),
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("invalid-pointer child failed: %v\n%s", err, out)
			}
		})
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
