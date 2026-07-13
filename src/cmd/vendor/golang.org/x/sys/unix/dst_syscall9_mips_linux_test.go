// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (mips || mipsle)

package unix

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"testing/simulation"
	"unsafe"
)

func TestDSTSyscall9AssemblyEntryFenced(t *testing.T) {
	marker := fmt.Sprintf("/tmp/go-dst-xsys-syscall9-%d", os.Getpid())
	os.Remove(marker)
	defer os.Remove(marker)
	path, err := BytePtrFromString(marker)
	if err != nil {
		t.Fatal(err)
	}
	type kernelTimespec struct{ Sec, Nsec int64 }
	var hostTS kernelTimespec
	if _, _, errno := Syscall9(SYS_CLOCK_GETTIME64, 1, uintptr(unsafe.Pointer(&hostTS)), 0, 0, 0, 0, 0, 0, 0); errno != 0 || hostTS.Sec == 0 {
		t.Fatalf("x/sys Syscall9 kernel fallthrough = %v, %v; want host clock", hostTS, errno)
	}
	var ts kernelTimespec
	var clockErr Errno
	var openPanic any
	simulation.Run(1, func() {
		_, _, clockErr = Syscall9(SYS_CLOCK_GETTIME64, 1, uintptr(unsafe.Pointer(&ts)), 0, 0, 0, 0, 0, 0, 0)
		func() {
			defer func() { openPanic = recover() }()
			Syscall9(SYS_OPENAT, uintptr(^uint32(99)), uintptr(unsafe.Pointer(path)), O_CREAT|O_WRONLY|O_TRUNC, 0o600, 0, 0, 0, 0, 0)
		}()
	})
	runtime.KeepAlive(path)
	if clockErr != 0 || ts.Sec == 0 {
		t.Errorf("x/sys Syscall9(clock_gettime64) = %v, %v; want virtual clock dispatch", ts, clockErr)
	}
	if openPanic == nil || !strings.Contains(fmt.Sprint(openPanic), "unsupported under deterministic simulation") {
		t.Errorf("x/sys Syscall9(OPENAT) panic = %v, want raw-kernel fence", openPanic)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("x/sys Syscall9(OPENAT) reached host filesystem: stat err=%v", err)
	}
}
