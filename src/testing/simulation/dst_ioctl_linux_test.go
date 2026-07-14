// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"fmt"
	"internal/testenv"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func TestDSTIoctlFenced(t *testing.T) {
	type entry struct {
		name string
		call func(fd, request, arg uintptr) syscall.Errno
	}
	entries := []entry{
		{"Syscall", func(fd, request, arg uintptr) syscall.Errno {
			_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, arg)
			return errno
		}},
		{"Syscall6", func(fd, request, arg uintptr) syscall.Errno {
			_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, request, arg, 0, 0, 0)
			return errno
		}},
		{"RawSyscall", func(fd, request, arg uintptr) syscall.Errno {
			_, _, errno := syscall.RawSyscall(syscall.SYS_IOCTL, fd, request, arg)
			return errno
		}},
		{"RawSyscall6", func(fd, request, arg uintptr) syscall.Errno {
			_, _, errno := syscall.RawSyscall6(syscall.SYS_IOCTL, fd, request, arg, 0, 0, 0)
			return errno
		}},
	}

	Run(1, func() {
		const invalidFD = ^uintptr(0)
		for _, e := range entries {
			for _, request := range []uintptr{syscall.TCGETS, syscall.TIOCGWINSZ, ioctlPeerRequest(), syscall.TCSETS, 0xdeadbeef} {
				got := captureIoctlPanic(func() { e.call(invalidFD, request, 0) })
				want := fmt.Sprintf("syscall: raw syscall %d unsupported under deterministic simulation", syscall.SYS_IOCTL)
				if got != want {
					t.Errorf("%s ioctl request %#x panic = %v, want %q", e.name, request, got, want)
				}
			}
		}
	})
}

func TestDSTIoctlDescriptorMintRefused(t *testing.T) {
	if os.Getenv("GO_DST_IOCTL_PEER_CHILD") == "1" {
		testDSTIoctlDescriptorMintRefused(t)
		return
	}
	if testing.Short() {
		t.Skip("-short: skips isolated host-fd census")
	}
	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestDSTIoctlDescriptorMintRefused$", "-test.v")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, "GO_DST_IOCTL_PEER_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("descriptor-mint child failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "--- SKIP: TestDSTIoctlDescriptorMintRefused") {
		t.Skipf("descriptor-mint child skipped:\n%s", out)
	}
}

func testDSTIoctlDescriptorMintRefused(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("opening /dev/ptmx: %v", err)
	}
	defer master.Close()

	var unlocked int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlocked))); errno != 0 {
		t.Fatalf("unlocking /dev/ptmx: %v", errno)
	}
	flags := uintptr(syscall.O_RDWR | syscall.O_NOCTTY | syscall.O_CLOEXEC)
	peer, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), ioctlPeerRequest(), flags)
	if errno != 0 {
		t.Fatalf("host TIOCGPTPEER support probe: %v", errno)
	}
	if err := syscall.Close(int(peer)); err != nil {
		t.Fatalf("closing support-probe peer fd: %v", err)
	}
	masterFD := master.Fd()

	startForeign := make(chan struct{})
	foreignDone := make(chan error, 1)
	go func() {
		<-startForeign
		peer, _, errno := syscall.Syscall(syscall.SYS_IOCTL, masterFD, ioctlPeerRequest(), flags)
		if errno != 0 {
			foreignDone <- errno
			return
		}
		foreignDone <- syscall.Close(int(peer))
	}()
	Run(1, func() {
		close(startForeign)
		if err := <-foreignDone; err != nil {
			t.Fatalf("non-bubble TIOCGPTPEER during active run: %v", err)
		}
	})

	before := ioctlHostFDs(t)
	peer = ^uintptr(0)
	var panicValue any
	Run(1, func() {
		panicValue = captureIoctlPanic(func() {
			peer, _, errno = syscall.Syscall(syscall.SYS_IOCTL, masterFD, ioctlPeerRequest(), flags)
		})
	})
	after := ioctlHostFDs(t)
	if peer != ^uintptr(0) {
		defer syscall.Close(int(peer))
	}

	wantPanic := fmt.Sprintf("syscall: raw syscall %d unsupported under deterministic simulation", syscall.SYS_IOCTL)
	if panicValue != wantPanic {
		t.Errorf("TIOCGPTPEER panic = %v, want %q (returned fd %#x, errno %v)", panicValue, wantPanic, peer, errno)
	}
	if !slices.Equal(before, after) {
		t.Errorf("host fd census changed across refused TIOCGPTPEER:\nbefore=%v\n after=%v", before, after)
	}
}

func ioctlPeerRequest() uintptr {
	switch runtime.GOARCH {
	case "ppc64", "ppc64le":
		return 0x20005441
	default:
		return 0x5441
	}
}

func captureIoctlPanic(f func()) (value any) {
	defer func() { value = recover() }()
	f()
	return nil
}

func ioctlHostFDs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("reading /proc/self/fd: %v", err)
	}
	fds := make([]string, 0, len(entries))
	for _, entry := range entries {
		fds = append(fds, entry.Name())
	}
	sort.Strings(fds)
	return fds
}
