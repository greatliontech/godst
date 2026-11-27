// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
	"unsafe"
)

const (
	testFcntlOFDGetlk  = 36
	testFcntlOFDSetlk  = 37
	testFcntlOFDSetlkw = 38
)

func testOFDFlock(f *os.File, cmd int, lk *syscall.Flock_t) error {
	return syscall.FcntlFlock(f.Fd(), cmd, lk)
}

func TestDSTOFDLockValidationMatrix(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/lock", make([]byte, 100), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		rw, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile RDWR: %v", err)
		}
		defer rw.Close()
		ro, err := os.OpenFile("/lock", os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("OpenFile RDONLY: %v", err)
		}
		defer ro.Close()
		wo, err := os.OpenFile("/lock", os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("OpenFile WRONLY: %v", err)
		}
		defer wo.Close()
		dirf, err := os.Open("/tmp")
		if err != nil {
			t.Fatalf("Open /tmp: %v", err)
		}
		defer dirf.Close()
		devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile /dev/null: %v", err)
		}
		defer devnull.Close()
		procf, err := os.Open("/proc/self/stat")
		if err != nil {
			t.Fatalf("Open /proc/self/stat: %v", err)
		}
		defer procf.Close()

		cases := []struct {
			name string
			f    *os.File
			cmd  int
			lk   syscall.Flock_t
			want error
		}{
			{"getlk-pid-nonzero", rw, testFcntlOFDGetlk, syscall.Flock_t{Type: syscall.F_RDLCK, Pid: 1}, syscall.EINVAL},
			{"setlk-pid-nonzero", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_RDLCK, Pid: -1}, syscall.EINVAL},
			{"setlkw-pid-nonzero", rw, testFcntlOFDSetlkw, syscall.Flock_t{Type: syscall.F_WRLCK, Pid: 42}, syscall.EINVAL},
			{"unlck-pid-nonzero", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Pid: 1}, syscall.EINVAL},
			{"getlk-bad-type", rw, testFcntlOFDGetlk, syscall.Flock_t{Type: 99, Len: 4}, syscall.EINVAL},
			{"setlk-bad-type", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: 99, Len: 4}, syscall.EINVAL},
			{"getlk-unlck-valid", rw, testFcntlOFDGetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Len: 4}, nil},
			{"getlk-wrlck-on-rdonly", ro, testFcntlOFDGetlk, syscall.Flock_t{Type: syscall.F_WRLCK, Len: 4}, nil},
			{"setlk-wrlck-on-rdonly", ro, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_WRLCK, Len: 4}, syscall.EBADF},
			{"setlkw-wrlck-on-rdonly", ro, testFcntlOFDSetlkw, syscall.Flock_t{Type: syscall.F_WRLCK, Len: 4}, syscall.EBADF},
			{"setlk-rdlck-on-wronly", wo, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_RDLCK, Len: 4}, syscall.EBADF},
			{"setlk-unlck-on-rdonly", ro, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Len: 4}, nil},
			{"setlk-unlck-on-wronly", wo, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Len: 4}, nil},
			{"bad-whence", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Whence: 3}, syscall.EINVAL},
			{"neg-start-seek-set", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Start: -1, Len: 1}, syscall.EINVAL},
			{"neg-start-seek-end", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Whence: 2, Start: -105, Len: 10}, syscall.EINVAL},
			{"neg-len-past-zero", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Start: 5, Len: -6}, syscall.EINVAL},
			{"neg-len-exact-zero", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Start: 5, Len: -5}, nil},
			{"neg-len-minint64", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Start: 10, Len: math.MinInt64}, syscall.EINVAL},
			{"start-minint64", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Start: math.MinInt64, Len: 1}, syscall.EINVAL},
			{"maxint64-len-1", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Start: math.MaxInt64, Len: 1}, nil},
			{"maxint64-len-2-overflow", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Start: math.MaxInt64, Len: 2}, syscall.EOVERFLOW},
			{"seek-end-overflow", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Whence: 2, Start: math.MaxInt64, Len: 0}, syscall.EOVERFLOW},
			// Multi-fault gate precedence (Linux fs/locks.c flock_to_posix_lock -> pid -> fmode):
			{"ro-wrlck-neg-start-einval", ro, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_WRLCK, Start: -1, Len: 4}, syscall.EINVAL},
			{"ro-wrlck-bad-whence-einval", ro, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 3, Len: 4}, syscall.EINVAL},
			{"ro-wrlck-overflow-eoverflow", ro, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_WRLCK, Start: math.MaxInt64, Len: 2}, syscall.EOVERFLOW},
			{"bad-type-overflow-eoverflow", rw, testFcntlOFDSetlk, syscall.Flock_t{Type: 99, Start: math.MaxInt64, Len: 2}, syscall.EOVERFLOW},
			{"setlk-pid1-overflow-eoverflow", ro, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_WRLCK, Pid: 1, Start: math.MaxInt64, Len: 2}, syscall.EOVERFLOW},
			{"getlk-pid1-overflow-eoverflow", ro, testFcntlOFDGetlk, syscall.Flock_t{Type: syscall.F_WRLCK, Pid: 1, Start: math.MaxInt64, Len: 2}, syscall.EOVERFLOW},
			{"dir-rdlck", dirf, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_RDLCK, Len: 4}, nil},
			{"dir-wrlck-ebadf", dirf, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_WRLCK, Len: 4}, syscall.EBADF},
			{"dir-unlck", dirf, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Len: 4}, nil},
			{"devnull-wrlck", devnull, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_WRLCK, Len: 4}, nil},
			{"devnull-unlck", devnull, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_UNLCK, Len: 4}, nil},
			{"proc-enotsup", procf, testFcntlOFDSetlk, syscall.Flock_t{Type: syscall.F_RDLCK, Len: 4}, syscall.ENOTSUP},
		}
		for _, tc := range cases {
			lk := tc.lk
			got := testOFDFlock(tc.f, tc.cmd, &lk)
			if !errors.Is(got, tc.want) {
				t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		}

		// Null Flock_t pointer: EFAULT on a live virtual fd, EBADF once closed.
		if err := syscall.FcntlFlock(rw.Fd(), testFcntlOFDSetlk, nil); !errors.Is(err, syscall.EFAULT) {
			t.Fatalf("nil Flock_t on live fd = %v, want EFAULT", err)
		}
		tmp, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile tmp: %v", err)
		}
		staleVFD := tmp.Fd()
		if err := tmp.Close(); err != nil {
			t.Fatalf("Close tmp: %v", err)
		}
		if err := syscall.FcntlFlock(staleVFD, testFcntlOFDSetlk, nil); !errors.Is(err, syscall.EBADF) {
			t.Fatalf("nil Flock_t on closed virtual fd = %v, want EBADF", err)
		}
	})
}

func TestDSTOFDLockFencesUnsupportedFcntlAndRawSyscall(t *testing.T) {
	expectFence := func(name string, fn func()) {
		t.Helper()
		var caught any
		func() {
			defer func() { caught = recover() }()
			fn()
		}()
		if caught == nil {
			t.Fatalf("%s: did not panic", name)
		}
		msg, ok := caught.(string)
		if !ok || !strings.Contains(msg, "unsupported under deterministic simulation") {
			t.Fatalf("%s: panic = %v, want unsupported-under-simulation", name, caught)
		}
	}

	simulation.Run(1, func() {
		if err := os.WriteFile("/lock", []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		vfd := f.Fd()

		// Process-scoped POSIX F_SETLK/F_GETLK/F_SETLKW stay fenced.
		expectFence("POSIX F_SETLK", func() {
			lk := syscall.Flock_t{Type: syscall.F_WRLCK, Len: 1}
			_ = syscall.FcntlFlock(vfd, syscall.F_SETLK, &lk)
		})
		expectFence("POSIX F_GETLK", func() {
			lk := syscall.Flock_t{Type: syscall.F_WRLCK, Len: 1}
			_ = syscall.FcntlFlock(vfd, syscall.F_GETLK, &lk)
		})
		// RawSyscall (nosplit/no-P entry) stays fenced even for F_OFD_SETLK on
		// the arch's fcntl64Syscall trap (221 on 32-bit 386/arm, SYS_FCNTL on 64-bit).
		fcntlTrap := uintptr(syscall.SYS_FCNTL)
		if runtime.GOARCH == "386" || runtime.GOARCH == "arm" {
			fcntlTrap = 221 // SYS_FCNTL64
		}
		expectFence("RawSyscall F_OFD_SETLK", func() {
			lk := syscall.Flock_t{Type: syscall.F_WRLCK, Len: 1}
			_, _, _ = syscall.RawSyscall(fcntlTrap, vfd, testFcntlOFDSetlk, uintptr(unsafe.Pointer(&lk)))
		})
		// Host / closed (-1) fd meets the raw fence.
		_ = f.Close()
		expectFence("closed File.Fd() (-1) F_OFD_SETLK", func() {
			lk := syscall.Flock_t{Type: syscall.F_WRLCK, Len: 1}
			_ = syscall.FcntlFlock(f.Fd(), testFcntlOFDSetlk, &lk)
		})
	})
}

func TestDSTOFDLockIntervalAlgebraAndFlockIndependence(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/lock", make([]byte, 64), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f1, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile f1: %v", err)
		}
		defer f1.Close()
		// Second open in the SAME process creates a distinct open file description.
		f2, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile f2: %v", err)
		}
		defer f2.Close()
		f3, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile f3: %v", err)
		}
		defer f3.Close()

		// BSD flock and OFD byte-range locks are independent lock tables.
		if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("Flock f1: %v", err)
		}
		if err := testOFDFlock(f2, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 0, Len: 16}); err != nil {
			t.Fatalf("OFD WRLCK on f2 while f1 holds BSD LOCK_EX: %v", err)
		}
		if err := testOFDFlock(f2, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_UNLCK, Start: 0, Len: 16}); err != nil {
			t.Fatalf("OFD UNLCK f2: %v", err)
		}
		if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatalf("Flock UN f1: %v", err)
		}

		// Adjacent same-type intervals on the same description coalesce: [0,10) + [10,20) -> [0,20).
		if err := testOFDFlock(f1, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 0, Len: 10}); err != nil {
			t.Fatalf("f1 WRLCK [0,10): %v", err)
		}
		if err := testOFDFlock(f1, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 10, Len: 10}); err != nil {
			t.Fatalf("f1 WRLCK [10,20): %v", err)
		}
		// F_OFD_GETLK with Type=F_UNLCK (Linux >= 6.6) inspects same-description
		// locks while ignoring peer descriptions' locks.
		qSelf := syscall.Flock_t{Type: syscall.F_UNLCK, Start: 2, Len: 4}
		if err := testOFDFlock(f1, testFcntlOFDGetlk, &qSelf); err != nil {
			t.Fatalf("GETLK(F_UNLCK) self: %v", err)
		}
		if qSelf.Type != syscall.F_WRLCK || qSelf.Whence != 0 || qSelf.Start != 0 || qSelf.Len != 20 || qSelf.Pid != -1 {
			t.Fatalf("GETLK(F_UNLCK) self = %+v, want Type=WRLCK Start=0 Len=20 Pid=-1", qSelf)
		}
		qPeerUn := syscall.Flock_t{Type: syscall.F_UNLCK, Whence: 2, Start: -10, Len: 4}
		if err := testOFDFlock(f2, testFcntlOFDGetlk, &qPeerUn); err != nil {
			t.Fatalf("GETLK(F_UNLCK) peer: %v", err)
		}
		if qPeerUn.Type != syscall.F_UNLCK || qPeerUn.Whence != 2 || qPeerUn.Start != -10 || qPeerUn.Len != 4 || qPeerUn.Pid != 0 {
			t.Fatalf("GETLK(F_UNLCK) peer = %+v, want Type=UNLCK Whence=2 Start=-10 Len=4 Pid=0", qPeerUn)
		}
		q := syscall.Flock_t{Type: syscall.F_RDLCK, Start: 5, Len: 10}
		if err := testOFDFlock(f2, testFcntlOFDGetlk, &q); err != nil {
			t.Fatalf("GETLK coalesced: %v", err)
		}
		if q.Type != syscall.F_WRLCK || q.Whence != 0 || q.Start != 0 || q.Len != 20 || q.Pid != -1 {
			t.Fatalf("GETLK coalesced = %+v, want Type=WRLCK Whence=0 Start=0 Len=20 Pid=-1", q)
		}

		// Middle conversion [5,15) to RDLCK splits into [0,5) WRLCK, [5,15) RDLCK, [15,20) WRLCK.
		if err := testOFDFlock(f1, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_RDLCK, Start: 5, Len: 10}); err != nil {
			t.Fatalf("f1 downgrade [5,15): %v", err)
		}
		// f2 can now acquire RDLCK on [5,15), while [4,6) and [14,16) still conflict with WRLCK!
		if err := testOFDFlock(f2, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_RDLCK, Start: 5, Len: 10}); err != nil {
			t.Fatalf("f2 RDLCK [5,15): %v", err)
		}
		if err := testOFDFlock(f2, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_RDLCK, Start: 4, Len: 2}); !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("f2 RDLCK [4,6) = %v, want EAGAIN", err)
		}
		if err := testOFDFlock(f2, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_RDLCK, Start: 14, Len: 2}); !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("f2 RDLCK [14,16) = %v, want EAGAIN", err)
		}

		// Failed non-blocking upgrade on f1 ([5,15) RDLCK -> WRLCK while f2 holds RDLCK)
		// MUST return EAGAIN AND retain f1's existing RDLCK (unlike BSD flock!).
		if err := testOFDFlock(f1, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 5, Len: 10}); !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("f1 contested upgrade = %v, want EAGAIN", err)
		}
		if err := testOFDFlock(f2, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_UNLCK, Start: 5, Len: 10}); err != nil {
			t.Fatalf("f2 UNLCK [5,15): %v", err)
		}
		if err := testOFDFlock(f3, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 5, Len: 10}); !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("f3 WRLCK [5,15) after f1 failed upgrade = %v, want EAGAIN (f1 must retain RDLCK)", err)
		}

		// Open-ended lock [0, 0) + partial unlock [0, 30) -> [30, inf) reported with Len=0.
		if err := testOFDFlock(f1, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 0, Len: 0}); err != nil {
			t.Fatalf("f1 WRLCK [0, inf): %v", err)
		}
		if err := testOFDFlock(f1, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_UNLCK, Start: 0, Len: 30}); err != nil {
			t.Fatalf("f1 UNLCK [0, 30): %v", err)
		}
		qInf := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 2, Start: -10, Len: 5}
		if err := testOFDFlock(f2, testFcntlOFDGetlk, &qInf); err != nil {
			t.Fatalf("GETLK [30, inf): %v", err)
		}
		if qInf.Type != syscall.F_WRLCK || qInf.Whence != 0 || qInf.Start != 30 || qInf.Len != 0 || qInf.Pid != -1 {
			t.Fatalf("GETLK [30, inf) = %+v, want Start=30 Len=0 Pid=-1", qInf)
		}
		// Non-conflicting GETLK sets Type=F_UNLCK and preserves Whence/Start/Len/Pid.
		qFree := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 2, Start: -60, Len: 5}
		if err := testOFDFlock(f2, testFcntlOFDGetlk, &qFree); err != nil {
			t.Fatalf("GETLK [4, 9): %v", err)
		}
		if qFree.Type != syscall.F_UNLCK || qFree.Whence != 2 || qFree.Start != -60 || qFree.Len != 5 || qFree.Pid != 0 {
			t.Fatalf("GETLK non-conflicting = %+v, want Type=UNLCK Whence=2 Start=-60 Len=5 Pid=0", qFree)
		}

		// SEEK_CUR with negative Len: seek f2 to 20, lock Len=-8 -> covers [12, 20).
		if _, err := f2.Seek(20, io.SeekStart); err != nil {
			t.Fatalf("Seek f2: %v", err)
		}
		if err := testOFDFlock(f2, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 1, Start: 0, Len: -8}); err != nil {
			t.Fatalf("f2 SEEK_CUR Len=-8: %v", err)
		}
		if err := testOFDFlock(f3, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_RDLCK, Start: 11, Len: 1}); err != nil {
			t.Fatalf("f3 [11,12) should be free: %v", err)
		}
		if err := testOFDFlock(f3, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_RDLCK, Start: 12, Len: 1}); !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("f3 [12,13) = %v, want EAGAIN", err)
		}
		if err := testOFDFlock(f3, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_RDLCK, Start: 19, Len: 1}); !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("f3 [19,20) = %v, want EAGAIN", err)
		}
		if err := testOFDFlock(f3, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_RDLCK, Start: 20, Len: 1}); err != nil {
			t.Fatalf("f3 [20,21) should be free: %v", err)
		}

		// Directory SEEK_CUR uses dirpos, os.Root.OpenFile mints independent OFD
		// descriptions, and syscall.Close(vfd) releases OFD locks.
		if err := os.WriteFile("/tmp/dentry1", []byte("1"), 0o600); err != nil {
			t.Fatalf("WriteFile dentry1: %v", err)
		}
		df1, err := os.Open("/tmp")
		if err != nil {
			t.Fatalf("Open /tmp df1: %v", err)
		}
		defer df1.Close()
		if _, err := df1.Readdirnames(1); err != nil {
			t.Fatalf("Readdirnames df1: %v", err)
		}
		if err := testOFDFlock(df1, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_RDLCK, Whence: 1, Start: 0, Len: 2}); err != nil {
			t.Fatalf("df1 SEEK_CUR RDLCK: %v", err)
		}
		qDir := syscall.Flock_t{Type: syscall.F_UNLCK, Start: 0, Len: 10}
		if err := testOFDFlock(df1, testFcntlOFDGetlk, &qDir); err != nil || qDir.Start != 1 || qDir.Len != 2 {
			t.Fatalf("df1 GETLK(UNLCK) after Readdirnames(1) = (%+v, %v), want Start=1 Len=2", qDir, err)
		}

		root, err := os.OpenRoot("/")
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		defer root.Close()
		rf1, err := root.OpenFile("lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("root.OpenFile rf1: %v", err)
		}
		rf2, err := root.OpenFile("lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("root.OpenFile rf2: %v", err)
		}
		defer rf2.Close()
		if err := testOFDFlock(rf1, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 25, Len: 4}); err != nil {
			t.Fatalf("rf1 WRLCK [25,29): %v", err)
		}
		if err := testOFDFlock(rf2, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 25, Len: 4}); !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("rf2 WRLCK [25,29) while rf1 held = %v, want EAGAIN", err)
		}
		if err := syscall.Close(int(rf1.Fd())); err != nil {
			t.Fatalf("syscall.Close(rf1.Fd()): %v", err)
		}
		if err := testOFDFlock(rf2, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 25, Len: 4}); err != nil {
			t.Fatalf("rf2 WRLCK [25,29) after syscall.Close(rf1.Fd()): %v", err)
		}
	})
}

func TestDSTOFDLockMmapLifetimeAndCloseWhileBlocked(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/lock", make([]byte, 4096), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		peer, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile peer: %v", err)
		}
		defer peer.Close()

		// 1. Both BSD flock and OFD lock on a description with a live MAP_SHARED
		// mapping survive Close() and release only at the last Munmap().
		holder, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile holder: %v", err)
		}
		if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("Flock holder: %v", err)
		}
		if err := testOFDFlock(holder, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 64, Len: 16}); err != nil {
			t.Fatalf("OFD holder: %v", err)
		}
		mapped, err := syscall.Mmap(int(holder.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap holder: %v", err)
		}
		if err := holder.Close(); err != nil {
			t.Fatalf("Close holder: %v", err)
		}
		if err := syscall.Flock(int(peer.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatalf("peer Flock while mapped = %v, want EWOULDBLOCK", err)
		}
		if err := testOFDFlock(peer, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 64, Len: 16}); !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("peer OFD while mapped = %v, want EAGAIN", err)
		}
		if err := syscall.Munmap(mapped); err != nil {
			t.Fatalf("Munmap: %v", err)
		}
		if err := syscall.Flock(int(peer.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("peer Flock after Munmap: %v", err)
		}
		_ = syscall.Flock(int(peer.Fd()), syscall.LOCK_UN)
		if err := testOFDFlock(peer, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 64, Len: 16}); err != nil {
			t.Fatalf("peer OFD after Munmap: %v", err)
		}

		// 2. Close while blocked in F_OFD_SETLKW (without mmap): stays blocked,
		// wakes with nil on peer unlock, and leaves no lock behind.
		waiter, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile waiter: %v", err)
		}
		wfd := waiter.Fd()
		done := make(chan error, 1)
		go func() {
			done <- syscall.FcntlFlock(wfd, testFcntlOFDSetlkw, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 64, Len: 16})
		}()
		time.Sleep(10 * time.Millisecond)
		if err := waiter.Close(); err != nil {
			t.Fatalf("Close waiter while blocked: %v", err)
		}
		select {
		case err := <-done:
			t.Fatalf("SETLKW returned early on Close: %v", err)
		default:
		}
		if err := testOFDFlock(peer, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_UNLCK, Start: 64, Len: 16}); err != nil {
			t.Fatalf("peer unlock: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("SETLKW after close + peer unlock = %v, want nil", err)
		}
		if err := testOFDFlock(peer, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 64, Len: 16}); err != nil {
			t.Fatalf("peer re-acquire after closed waiter returned: %v", err)
		}

		// 3. Close while blocked in F_OFD_SETLKW WITH a live MAP_SHARED mapping:
		// the granted lock persists via flockPending until Munmap().
		mappedWaiter, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile mappedWaiter: %v", err)
		}
		mwFD := mappedWaiter.Fd()
		mwMap, err := syscall.Mmap(int(mwFD), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap mappedWaiter: %v", err)
		}
		mwDone := make(chan error, 1)
		go func() {
			mwDone <- syscall.FcntlFlock(mwFD, testFcntlOFDSetlkw, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 64, Len: 16})
		}()
		time.Sleep(10 * time.Millisecond)
		if err := mappedWaiter.Close(); err != nil {
			t.Fatalf("Close mappedWaiter while blocked: %v", err)
		}
		if err := testOFDFlock(peer, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_UNLCK, Start: 64, Len: 16}); err != nil {
			t.Fatalf("peer unlock for mappedWaiter: %v", err)
		}
		if err := <-mwDone; err != nil {
			t.Fatalf("mappedWaiter SETLKW = %v, want nil", err)
		}
		if err := testOFDFlock(peer, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 64, Len: 16}); !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("peer acquire while mwMap still mapped = %v, want EAGAIN", err)
		}
		if err := syscall.Munmap(mwMap); err != nil {
			t.Fatalf("Munmap mwMap: %v", err)
		}
		if err := testOFDFlock(peer, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 64, Len: 16}); err != nil {
			t.Fatalf("peer acquire after mwMap Munmap: %v", err)
		}
		_ = testOFDFlock(peer, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_UNLCK, Start: 64, Len: 16})
	})
}

func TestDSTOFDLockProcessCrashAndSharedMmapReap(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h1", func() {
			if err := os.WriteFile("/db.lock", make([]byte, 4096), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			fInit, err := os.OpenFile("/db.lock", os.O_RDWR, 0)
			if err != nil {
				t.Fatalf("OpenFile init: %v", err)
			}
			if err := fInit.Sync(); err != nil {
				t.Fatalf("Sync init: %v", err)
			}
			_ = fInit.Close()

			ready := make(chan struct{})
			go simulation.Process("reader", func() {
				f, err := os.OpenFile("/db.lock", os.O_RDWR, 0)
				if err != nil {
					t.Errorf("reader OpenFile: %v", err)
					return
				}
				m, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Errorf("reader Mmap: %v", err)
					return
				}
				if err := testOFDFlock(f, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 64, Len: 16}); err != nil {
					t.Errorf("reader OFD lock: %v", err)
					return
				}
				// Write reader slot Seq + PID into MAP_SHARED memory under OFD WRLCK.
				binary.LittleEndian.PutUint64(m[64:72], 77)
				binary.LittleEndian.PutUint64(m[72:80], uint64(os.Getpid()))
				close(ready)
				select {}
			})
			<-ready

			// While reader is alive, writer's non-blocking reap attempt gets EAGAIN.
			writerDone := make(chan struct{})
			go simulation.Process("writer", func() {
				defer close(writerDone)
				f, err := os.OpenFile("/db.lock", os.O_RDWR, 0)
				if err != nil {
					t.Errorf("writer OpenFile: %v", err)
					return
				}
				defer f.Close()
				m, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Errorf("writer Mmap: %v", err)
					return
				}
				defer syscall.Munmap(m)

				if err := testOFDFlock(f, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 64, Len: 16}); !errors.Is(err, syscall.EAGAIN) {
					t.Errorf("writer reap while reader live = %v, want EAGAIN", err)
					return
				}

				simulation.CrashProcess("h1", "reader")

				// Reader crashed: both its fd and its MAP_SHARED mapping tore down,
				// releasing the OFD slot lock and synchronizing the shared slot memory.
				if err := testOFDFlock(f, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 64, Len: 16}); err != nil {
					t.Errorf("writer reap after CrashProcess: %v", err)
					return
				}
				if got := binary.LittleEndian.Uint64(m[64:72]); got != 77 {
					t.Errorf("slot Seq before zeroing = %d, want 77", got)
				}
				binary.LittleEndian.PutUint64(m[64:72], 0)
				binary.LittleEndian.PutUint64(m[72:80], 0)
				if err := testOFDFlock(f, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_UNLCK, Start: 64, Len: 16}); err != nil {
					t.Errorf("writer unlock reaped slot: %v", err)
				}
			})
			<-writerDone

			// Cross-process shared *os.File regression: proc "opener" opens f without
			// calling f.Fd(); proc "borrower" calls f.Fd(), acquires an OFD lock, and
			// exits (clearing its vfd from dstFDRegistry while f stays open); closing
			// f in "opener" must still release the description's OFD lock.
			sharedCh := make(chan *os.File, 1)
			releaseOpener := make(chan struct{})
			openerClosed := make(chan struct{})
			go simulation.Process("opener", func() {
				f, err := os.OpenFile("/db.lock", os.O_RDWR, 0)
				if err != nil {
					t.Errorf("opener OpenFile: %v", err)
					return
				}
				sharedCh <- f
				<-releaseOpener
				if err := f.Close(); err != nil {
					t.Errorf("opener Close: %v", err)
				}
				close(openerClosed)
			})
			sharedFile := <-sharedCh
			borrowerDone := make(chan struct{})
			go simulation.Process("borrower", func() {
				defer close(borrowerDone)
				if err := testOFDFlock(sharedFile, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 128, Len: 16}); err != nil {
					t.Errorf("borrower OFD lock: %v", err)
				}
			})
			<-borrowerDone

			probeDone := make(chan struct{})
			go simulation.Process("probe", func() {
				defer close(probeDone)
				f, err := os.OpenFile("/db.lock", os.O_RDWR, 0)
				if err != nil {
					t.Errorf("probe OpenFile: %v", err)
					return
				}
				defer f.Close()
				// Opener still holds sharedFile open: OFD lock must still be held.
				if err := testOFDFlock(f, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 128, Len: 16}); !errors.Is(err, syscall.EAGAIN) {
					t.Errorf("probe while opener open = %v, want EAGAIN", err)
					return
				}
				close(releaseOpener)
				<-openerClosed
				// Opener closed sharedFile (with zero remaining vfds in dstFDRegistry):
				// dstDropClosedNode must have released the OFD lock!
				if err := testOFDFlock(f, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 128, Len: 16}); err != nil {
					t.Errorf("probe after opener Close: %v", err)
				}
			})
			<-probeDone
		})
	})
}

func TestDSTOFDLockExploreContention(t *testing.T) {
	var p1Wins, p2Wins atomic.Bool
	simulation.ExploreTest(t, 1, simulation.ExploreOptions{
		Mode:         simulation.DPOR,
		MaxSchedules: 64,
	}, func() error {
		if err := os.WriteFile("/slot", make([]byte, 16), 0o600); err != nil {
			return fmt.Errorf("WriteFile: %w", err)
		}
		var winner atomic.Int32
		errCh := make(chan error, 2)
		contend := func(name string, id int32) {
			simulation.Process(name, func() {
				f, err := os.OpenFile("/slot", os.O_RDWR, 0)
				if err != nil {
					errCh <- fmt.Errorf("%s OpenFile: %w", name, err)
					return
				}
				defer f.Close()
				err = testOFDFlock(f, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 0, Len: 16})
				if err == nil {
					winner.CompareAndSwap(0, id)
					errCh <- nil
					return
				}
				if !errors.Is(err, syscall.EAGAIN) {
					errCh <- fmt.Errorf("%s OFDSetlk: %w", name, err)
					return
				}
				errCh <- nil
			})
		}
		go contend("p1", 1)
		go contend("p2", 2)
		for range 2 {
			if err := <-errCh; err != nil {
				return err
			}
		}
		switch winner.Load() {
		case 1:
			p1Wins.Store(true)
		case 2:
			p2Wins.Store(true)
		default:
			return fmt.Errorf("no process won OFDSetlk")
		}
		return nil
	})
	if !p1Wins.Load() || !p2Wins.Load() {
		t.Fatalf("DPOR did not explore both OFDSetlk winner orders: p1=%v p2=%v", p1Wins.Load(), p2Wins.Load())
	}
}

func TestDSTOFDLockExploreBlockingWakeupOrder(t *testing.T) {
	var p1First, p2First atomic.Bool
	simulation.ExploreTest(t, 1, simulation.ExploreOptions{
		Mode:         simulation.DPOR,
		MaxSchedules: 64,
	}, func() error {
		if err := os.WriteFile("/slotw", make([]byte, 16), 0o600); err != nil {
			return fmt.Errorf("WriteFile: %w", err)
		}
		holder, err := os.OpenFile("/slotw", os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("holder OpenFile: %w", err)
		}
		if err := testOFDFlock(holder, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 0, Len: 16}); err != nil {
			_ = holder.Close()
			return fmt.Errorf("holder SETLK: %w", err)
		}

		var firstWinner atomic.Int32
		p1Entering := make(chan struct{})
		p2Entering := make(chan struct{})
		errCh := make(chan error, 2)

		waitProc := func(name string, id int32, entering chan struct{}) {
			simulation.Process(name, func() {
				f, err := os.OpenFile("/slotw", os.O_RDWR, 0)
				if err != nil {
					close(entering)
					errCh <- fmt.Errorf("%s OpenFile: %w", name, err)
					return
				}
				defer f.Close()
				_ = f.Fd()
				close(entering)
				if err := testOFDFlock(f, testFcntlOFDSetlkw, &syscall.Flock_t{Type: syscall.F_WRLCK, Start: 0, Len: 16}); err != nil {
					errCh <- fmt.Errorf("%s SETLKW: %w", name, err)
					return
				}
				firstWinner.CompareAndSwap(0, id)
				errCh <- testOFDFlock(f, testFcntlOFDSetlk, &syscall.Flock_t{Type: syscall.F_UNLCK, Start: 0, Len: 16})
			})
		}

		go waitProc("w1", 1, p1Entering)
		<-p1Entering
		time.Sleep(5 * time.Millisecond) // w1 parks on <-wait strictly before w2 starts
		go waitProc("w2", 2, p2Entering)
		<-p2Entering
		time.Sleep(5 * time.Millisecond) // w2 also parks on <-wait before holder unlocks

		if err := holder.Close(); err != nil {
			return fmt.Errorf("holder Close: %w", err)
		}
		for range 2 {
			if err := <-errCh; err != nil {
				return err
			}
		}
		switch firstWinner.Load() {
		case 1:
			p1First.Store(true)
		case 2:
			p2First.Store(true)
		default:
			return fmt.Errorf("no SETLKW winner recorded")
		}
		return nil
	})
	if !p1First.Load() || !p2First.Load() {
		t.Fatalf("DPOR did not explore both F_OFD_SETLKW wakeup winner orders: w1=%v w2=%v", p1First.Load(), p2First.Load())
	}
}
