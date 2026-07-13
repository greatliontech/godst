// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (mips || mipsle)

package syscall_test

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/simulation"
	"unsafe"
)

const sysClockGettime64 = 4403

func TestDSTSyscall9DispatchesOrRefusesBeforeKernel(t *testing.T) {
	marker := fmt.Sprintf("/tmp/go-dst-syscall9-%d", os.Getpid())
	os.Remove(marker)
	defer os.Remove(marker)
	path, err := syscall.BytePtrFromString(marker)
	if err != nil {
		t.Fatal(err)
	}
	type kernelTimespec struct{ Sec, Nsec int64 }
	var ts kernelTimespec
	var dispatchErr syscall.Errno
	var openPanic any
	simulation.Run(1, func() {
		_, _, dispatchErr = syscall.Syscall9(sysClockGettime64, 1, uintptr(unsafe.Pointer(&ts)), 0, 0, 0, 0, 0, 0, 0)
		func() {
			defer func() { openPanic = recover() }()
			syscall.Syscall9(syscall.SYS_OPENAT, uintptr(^uint32(99)), uintptr(unsafe.Pointer(path)), syscall.O_CREAT|syscall.O_WRONLY|syscall.O_TRUNC, 0o600, 0, 0, 0, 0, 0)
		}()
	})
	runtime.KeepAlive(path)
	if dispatchErr != 0 || ts.Sec == 0 {
		t.Errorf("Syscall9(clock_gettime64) = %v, %v; want virtual clock dispatch", ts, dispatchErr)
	}
	if openPanic == nil || !strings.Contains(fmt.Sprint(openPanic), "unsupported under deterministic simulation") {
		t.Errorf("Syscall9(OPENAT) panic = %v, want raw-kernel fence", openPanic)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Syscall9(OPENAT) reached host filesystem: stat err=%v", err)
	}
}
