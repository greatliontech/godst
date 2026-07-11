// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/simulation"
	"testing/synctest"
	"time"
	"unsafe"
)

const (
	dstTestMadvCold         = 20
	dstTestMadvPopulateRead = 22
)

func expectDSTRawSyscallPanic(t *testing.T, call func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(fmt.Sprint(r), "unsupported under deterministic simulation") {
			t.Fatalf("raw syscall panic = %v, want unsupported-under-simulation", r)
		}
	}()
	call()
}

func TestDSTRawSyscallNoErrorIdentityNotFenced(t *testing.T) {
	simulation.Run(1, func() {
		if pid := syscall.Getpid(); pid <= 0 {
			t.Fatalf("syscall.Getpid = %d, want host pid", pid)
		}
		_ = syscall.Getppid()
		_ = syscall.Gettid()
		_ = syscall.Getuid()
		_ = syscall.Getgid()
		_ = syscall.Geteuid()
		_ = syscall.Getegid()
	})
}

// TestDSTFSVirtualFDRawSyscallDispatch pins the raw boundary's two halves for a
// virtual fd. Syscall and Syscall6 fence BEFORE entersyscall, so they DISPATCH
// the settled subset a SUT reaches through golang.org/x/sys/unix (the file
// barriers, flock, close, and the mapping ops) to the file backend. RawSyscall
// and RawSyscall6 run with no P (post-entersyscall, or post-fork), where the
// dispatch's allocation could not grow the stack, so they still refuse — as does
// any operation outside the subset, on either trampoline.
func TestDSTFSVirtualFDRawSyscallDispatch(t *testing.T) {
	dispatched := []struct {
		name string
		call func(fd uintptr) syscall.Errno
	}{
		{"Syscall_Fsync", func(fd uintptr) syscall.Errno {
			_, _, e := syscall.Syscall(syscall.SYS_FSYNC, fd, 0, 0)
			return e
		}},
		{"Syscall_Fdatasync", func(fd uintptr) syscall.Errno {
			_, _, e := syscall.Syscall(syscall.SYS_FDATASYNC, fd, 0, 0)
			return e
		}},
		{"Syscall_Flock", func(fd uintptr) syscall.Errno {
			_, _, e := syscall.Syscall(syscall.SYS_FLOCK, fd, syscall.LOCK_EX, 0)
			return e
		}},
	}
	for _, tt := range dispatched {
		t.Run(tt.name, func(t *testing.T) {
			simulation.Run(1, func() {
				f, err := os.Create("/fd")
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				defer f.Close()
				if e := tt.call(f.Fd()); e != 0 {
					t.Fatalf("%s on a virtual fd = %v, want dispatch to the backend", tt.name, e)
				}
			})
		})
	}

	fenced := []struct {
		name string
		call func(fd uintptr)
	}{
		// The raw trampolines have no P for the dispatch's allocation.
		{"RawSyscall_Fdatasync", func(fd uintptr) { syscall.RawSyscall(syscall.SYS_FDATASYNC, fd, 0, 0) }},
		{"RawSyscall_Flock", func(fd uintptr) { syscall.RawSyscall(syscall.SYS_FLOCK, fd, syscall.LOCK_EX, 0) }},
		// Outside the settled subset: a minting op, and an fd-carrying op whose
		// argument shape the dispatch deliberately does not decode.
		{"Syscall6_Mmap", func(fd uintptr) {
			syscall.Syscall6(syscall.SYS_MMAP, 0, uintptr(syscall.Getpagesize()), syscall.PROT_READ, syscall.MAP_SHARED, fd, 0)
		}},
		{"Syscall_Read", func(fd uintptr) {
			var b [1]byte
			syscall.Syscall(syscall.SYS_READ, fd, uintptr(unsafe.Pointer(&b[0])), 1)
		}},
	}
	for _, tt := range fenced {
		t.Run(tt.name, func(t *testing.T) {
			simulation.Run(1, func() {
				f, err := os.Create("/fd")
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				fd := f.Fd()
				defer f.Close()
				expectDSTRawSyscallPanic(t, func() { tt.call(fd) })
			})
		})
	}
}

func TestDSTFSVirtualFDMmapReadOnlyShared(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.OpenFile("/m", os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		if _, err := f.WriteString("hello"); err != nil {
			t.Fatalf("WriteString: %v", err)
		}

		b, err := syscall.Mmap(int(f.Fd()), 0, 5, syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := string(b); got != "hello" {
			t.Fatalf("mapped bytes = %q, want hello", got)
		}

		wf, err := os.OpenFile("/m", os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("OpenFile write handle: %v", err)
		}
		defer wf.Close()
		if _, err := wf.WriteAt([]byte("Y"), 1); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if got := string(b); got != "hYllo" {
			t.Fatalf("mapped bytes after WriteAt = %q, want hYllo", got)
		}
		// VM_MAYWRITE follows the DESCRIPTOR's access mode at map time, not
		// the map-time prot: this read-only mapping is backed by an O_RDWR
		// fd, so upgrading to PROT_READ|PROT_WRITE succeeds, exactly as
		// Linux gives — and PROT_NONE is always permitted.
		if err := syscall.Mprotect(b, syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
			t.Fatalf("Mprotect(PROT_READ|PROT_WRITE) on an O_RDWR-backed mapping: %v", err)
		}
		b[0] = 'H' // writable now; the store lands in the shared page cache
		if got, err := os.ReadFile("/m"); err != nil || string(got) != "HYllo" {
			t.Fatalf("file after mapped store = %q, %v; want HYllo", got, err)
		}
		if err := syscall.Mprotect(b, syscall.PROT_NONE); err != nil {
			t.Fatalf("Mprotect(PROT_NONE): %v", err)
		}
		if err := syscall.Mprotect(b, syscall.PROT_READ); err != nil {
			t.Fatalf("Mprotect(PROT_READ): %v", err)
		}
		if got := string(b); got != "HYllo" {
			t.Fatalf("mapped bytes after protection round-trip = %q, want HYllo", got)
		}
		// The refusal that REMAINS is the descriptor's: an O_RDONLY-backed
		// mapping may never gain write.
		rf, err := os.Open("/m")
		if err != nil {
			t.Fatalf("Open read-only: %v", err)
		}
		defer rf.Close()
		rb, err := syscall.Mmap(int(rf.Fd()), 0, 5, syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap read-only fd: %v", err)
		}
		defer syscall.Munmap(rb)
		if err := syscall.Mprotect(rb, syscall.PROT_READ|syscall.PROT_WRITE); !errors.Is(err, syscall.EACCES) {
			t.Fatalf("Mprotect(RW) on an O_RDONLY-backed mapping = %v, want EACCES", err)
		}
		for _, advice := range []int{dstTestMadvPopulateRead, syscall.MADV_HUGEPAGE, dstTestMadvCold} {
			if err := syscall.Madvise(b, advice); err != nil {
				t.Fatalf("Madvise(%d): %v", advice, err)
			}
		}
		if err := syscall.Madvise(b, -1); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("Madvise(-1) = %v, want EINVAL", err)
		}
		if err := syscall.Munmap(b); err != nil {
			t.Fatalf("Munmap: %v", err)
		}
	})
}

func TestDSTFSVirtualFDMmapRangeOperations(t *testing.T) {
	simulation.Run(1, func() {
		page := syscall.Getpagesize()
		content := make([]byte, page*2)
		for i := range content {
			content[i] = byte(i)
		}
		if err := os.WriteFile("/range", content, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.Open("/range")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer f.Close()

		b, err := syscall.Mmap(int(f.Fd()), 0, len(content), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap: %v", err)
		}
		sub := b[:page:page]
		if err := syscall.Mprotect(sub, syscall.PROT_READ); err != nil {
			t.Fatalf("Mprotect subrange: %v", err)
		}
		if err := syscall.Madvise(sub, dstTestMadvPopulateRead); err != nil {
			t.Fatalf("Madvise subrange: %v", err)
		}
		if err := syscall.Munmap(b); err != nil {
			t.Fatalf("Munmap: %v", err)
		}
	})
}

func TestDSTFSVirtualFDMmapCrossProcessWriteUpdates(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/m", []byte("hello"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			mapped := make(chan struct{})
			written := make(chan struct{})
			done := make(chan error, 1)
			go simulation.Process("p1", func() {
				f, err := os.Open("/m")
				if err != nil {
					done <- err
					return
				}
				b, err := syscall.Mmap(int(f.Fd()), 0, 5, syscall.PROT_READ, syscall.MAP_SHARED)
				if closeErr := f.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					done <- err
					return
				}
				close(mapped)
				<-written
				if got := string(b); got != "hYllo" {
					done <- fmt.Errorf("mapped bytes after cross-process WriteAt = %q, want hYllo", got)
					return
				}
				done <- syscall.Munmap(b)
			})
			select {
			case err := <-done:
				t.Fatalf("p1 map setup: %v", err)
			case <-mapped:
			}
			writtenClosed := false
			defer func() {
				if !writtenClosed {
					close(written)
				}
			}()

			simulation.Process("p2", func() {
				f, err := os.OpenFile("/m", os.O_WRONLY, 0)
				if err != nil {
					t.Fatalf("p2 OpenFile: %v", err)
				}
				defer f.Close()
				if _, err := f.WriteAt([]byte("Y"), 1); err != nil {
					t.Fatalf("p2 WriteAt: %v", err)
				}
			})
			close(written)
			writtenClosed = true
			if err := <-done; err != nil {
				t.Fatalf("p1 mapped observation: %v", err)
			}
		})
	})
}

func TestDSTFSVirtualFDMunmapRequiresExactMapping(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/m", []byte("hello"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.Open("/m")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer f.Close()
		b, err := syscall.Mmap(int(f.Fd()), 0, 5, syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap: %v", err)
		}
		if errno, handled := os.DSTMunmapResult(b[:1:1]); !handled || errno != syscall.EINVAL {
			t.Fatalf("dstMunmap partial slice = errno %v handled %v, want EINVAL true", errno, handled)
		}
		if err := syscall.Mprotect(b, syscall.PROT_READ); err != nil {
			t.Fatalf("Mprotect after failed partial Munmap: %v", err)
		}
		if err := syscall.Munmap(b); err != nil {
			t.Fatalf("full Munmap after partial failure: %v", err)
		}
	})
}

func TestDSTFSVirtualFDMmapWritableShared(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", make([]byte, 4), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			p1Mapped := make(chan uintptr, 1)
			p2Done := make(chan struct{})
			p1err := make(chan error, 1)
			go simulation.Process("p1", func() {
				f, err := os.OpenFile("/lock", os.O_RDWR, 0)
				if err != nil {
					p1err <- err
					return
				}
				p1Map, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if closeErr := f.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					p1err <- err
					return
				}
				atomic.StoreUint32((*uint32)(unsafe.Pointer(&p1Map[0])), 1)
				p1Mapped <- uintptr(unsafe.Pointer(&p1Map[0]))
				<-p2Done
				if got := atomic.LoadUint32((*uint32)(unsafe.Pointer(&p1Map[0]))); got != 2 {
					p1err <- fmt.Errorf("p1 atomic load after p2 CAS = %d, want 2", got)
					return
				}
				p1err <- syscall.Munmap(p1Map)
			})
			var p1Addr uintptr
			select {
			case err := <-p1err:
				t.Fatalf("p1 map setup: %v", err)
			case p1Addr = <-p1Mapped:
			}
			p2DoneClosed := false
			defer func() {
				if !p2DoneClosed {
					close(p2Done)
				}
			}()

			simulation.Process("p2", func() {
				f, err := os.OpenFile("/lock", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("p2 OpenFile: %v", err)
				}
				b, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if closeErr := f.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					t.Fatalf("p2 Mmap: %v", err)
				}
				// Each process's mmap is its own view of the shared pages, as on
				// real Linux: distinct addresses, one set of bytes. The CAS below
				// observing p1's store through p2's view pins the sharing.
				if p2Addr := uintptr(unsafe.Pointer(&b[0])); p2Addr == p1Addr {
					t.Fatalf("p2 mapping reuses p1's address %#x; real mmap returns distinct views", p1Addr)
				}
				if !atomic.CompareAndSwapUint32((*uint32)(unsafe.Pointer(&b[0])), 1, 2) {
					t.Fatalf("p2 CAS did not observe p1 store; value=%d", atomic.LoadUint32((*uint32)(unsafe.Pointer(&b[0]))))
				}
				if err := syscall.Munmap(b); err != nil {
					t.Fatalf("p2 Munmap: %v", err)
				}
			})
			close(p2Done)
			p2DoneClosed = true
			if err := <-p1err; err != nil {
				t.Fatalf("p1 mapped observation: %v", err)
			}
			got, err := os.ReadFile("/lock")
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if atomic.LoadUint32((*uint32)(unsafe.Pointer(&got[0]))) != 2 {
				t.Fatalf("file content after mapped CAS = %v, want uint32 value 2", got)
			}
		})
	})
}

func TestDSTFSVirtualFDMmapWritableSurvivesFileGrowth(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/lock", make([]byte, 4), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		b, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap: %v", err)
		}
		addr := uintptr(unsafe.Pointer(&b[0]))
		const growthOffset = 1024
		if _, err := f.WriteAt([]byte{9}, growthOffset); err != nil {
			t.Fatalf("WriteAt growth: %v", err)
		}
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&b[0])), 7)
		b2, err := syscall.Mmap(int(f.Fd()), 0, growthOffset+1, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("wider Mmap after growth: %v", err)
		}
		if got := uintptr(unsafe.Pointer(&b2[0])); got == addr {
			t.Fatalf("wider mapping reuses the first mapping's address %#x; real mmap returns distinct views", addr)
		}
		if atomic.LoadUint32((*uint32)(unsafe.Pointer(&b2[0]))) != 7 {
			t.Fatalf("wider mapping after growth did not observe mapped store")
		}
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&b2[0])), 6)
		if got := atomic.LoadUint32((*uint32)(unsafe.Pointer(&b[0]))); got != 6 {
			t.Fatalf("original mapping after wider mapped store = %d, want 6", got)
		}
		if b2[growthOffset] != 9 {
			t.Fatalf("wider mapping growth byte = %d, want 9", b2[growthOffset])
		}
		if err := syscall.Munmap(b2); err != nil {
			t.Fatalf("wider Munmap after growth: %v", err)
		}
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&b[0])), 8)
		if err := syscall.Munmap(b); err != nil {
			t.Fatalf("Munmap after growth: %v", err)
		}
		got, err := os.ReadFile("/lock")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if atomic.LoadUint32((*uint32)(unsafe.Pointer(&got[0]))) != 8 {
			t.Fatalf("file content after mapped write following growth = %v, want uint32 value 8", got[:4])
		}
		if got[growthOffset] != 9 {
			t.Fatalf("file growth byte = %d, want 9", got[growthOffset])
		}
	})
}

func TestDSTFSVirtualFDMmapOverlappingRangesShareBytes(t *testing.T) {
	simulation.Run(1, func() {
		page := syscall.Getpagesize()
		content := make([]byte, page*2)
		if err := os.WriteFile("/lock", content, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		whole, err := syscall.Mmap(int(f.Fd()), 0, len(content), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("whole Mmap: %v", err)
		}
		defer syscall.Munmap(whole)
		slot, err := syscall.Mmap(int(f.Fd()), int64(page), 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("slot Mmap: %v", err)
		}
		defer syscall.Munmap(slot)
		if got, aliased := uintptr(unsafe.Pointer(&slot[0])), uintptr(unsafe.Pointer(&whole[page])); got == aliased {
			t.Fatalf("overlapping mappings alias one address %#x; real mmap returns distinct views", got)
		}
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&slot[0])), 21)
		if got := atomic.LoadUint32((*uint32)(unsafe.Pointer(&whole[page]))); got != 21 {
			t.Fatalf("whole mapping after slot store = %d, want 21", got)
		}
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&whole[page])), 22)
		if got := atomic.LoadUint32((*uint32)(unsafe.Pointer(&slot[0]))); got != 22 {
			t.Fatalf("slot mapping after whole store = %d, want 22", got)
		}
	})
}

func TestDSTFSVirtualFDMmapWiderOverlapAfterGrowthCoheres(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/lock", make([]byte, 4), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		b, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap: %v", err)
		}
		page := syscall.Getpagesize()
		if _, err := f.WriteAt([]byte{1}, int64(page)); err != nil {
			t.Fatalf("WriteAt growth beyond backing: %v", err)
		}
		// A second, wider mapping over a grown file is ordinary on real Linux
		// (the copy-model refused it: it could not widen a shared buffer).
		wider, err := syscall.Mmap(int(f.Fd()), 0, page+1, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("wider Mmap after growth: %v", err)
		}
		defer syscall.Munmap(wider)
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&b[0])), 23)
		if got := atomic.LoadUint32((*uint32)(unsafe.Pointer(&wider[0]))); got != 23 {
			t.Fatalf("wider view reads %d, want the first view's store 23", got)
		}
		if err := syscall.Munmap(b); err != nil {
			t.Fatalf("Munmap: %v", err)
		}
		got, err := os.ReadFile("/lock")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if atomic.LoadUint32((*uint32)(unsafe.Pointer(&got[0]))) != 23 {
			t.Fatalf("file content after rejected overlap = %v, want uint32 value 23", got[:4])
		}
	})
}

func TestDSTFSVirtualFDMmapBridgeAcrossRangesCoheres(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/lock", make([]byte, 4), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		first, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("first Mmap: %v", err)
		}
		defer syscall.Munmap(first)
		page := syscall.Getpagesize()
		secondOff := page * 2
		if _, err := f.WriteAt([]byte{2}, int64(secondOff+3)); err != nil {
			t.Fatalf("WriteAt growth for second range: %v", err)
		}
		second, err := syscall.Mmap(int(f.Fd()), int64(secondOff), 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("second Mmap: %v", err)
		}
		defer syscall.Munmap(second)
		if uintptr(unsafe.Pointer(&first[0])) == uintptr(unsafe.Pointer(&second[0])) {
			t.Fatalf("disjoint mappings unexpectedly have identical start address")
		}
		bridge, err := syscall.Mmap(int(f.Fd()), 0, secondOff+4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("bridging Mmap: %v", err)
		}
		defer syscall.Munmap(bridge)
		first[0] = 31
		second[3] = 32
		if bridge[0] != 31 {
			t.Fatalf("bridge reads %d at 0, want the first window's 31", bridge[0])
		}
		if bridge[secondOff+3] != 32 {
			t.Fatalf("bridge reads %d at the second window, want 32", bridge[secondOff+3])
		}
	})
}

// TestDSTFSVirtualFDMmapWindowsAtOffsetsCohere: a window at a nonzero offset
// and a wider window overlapping it are independent views of the file's pages;
// a store through either is a store in the other, and a disjoint window at
// offset zero is untouched.
func TestDSTFSVirtualFDMmapWindowsAtOffsetsCohere(t *testing.T) {
	simulation.Run(1, func() {
		page := syscall.Getpagesize()
		if err := os.WriteFile("/lock", make([]byte, page*2), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		first, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("first Mmap: %v", err)
		}
		defer syscall.Munmap(first)
		if _, err := f.WriteAt([]byte{1}, int64(page*2)); err != nil {
			t.Fatalf("WriteAt growth: %v", err)
		}
		wide, err := syscall.Mmap(int(f.Fd()), int64(page), page+1, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("wide Mmap: %v", err)
		}
		defer syscall.Munmap(wide)
		sub, err := syscall.Mmap(int(f.Fd()), int64(page), 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("sub Mmap: %v", err)
		}
		defer syscall.Munmap(sub)
		sub[0] = 41
		if wide[0] != 41 {
			t.Fatalf("wide view reads %d at its start, want the sub view's 41", wide[0])
		}
		wide[1] = 42
		if sub[1] != 42 {
			t.Fatalf("sub view reads %d, want the wide view's 42", sub[1])
		}
		if first[0] != 0 {
			t.Fatalf("disjoint window at offset 0 reads %d, want 0", first[0])
		}
	})
}

// TestDSTFSVirtualFDMmapWindowsCohereAfterGrowth: windows mapped at different
// offsets, before and after the file grew, all read and write the same pages —
// including one that bridges two earlier windows.
func TestDSTFSVirtualFDMmapWindowsCohereAfterGrowth(t *testing.T) {
	simulation.Run(1, func() {
		page := syscall.Getpagesize()
		if err := os.WriteFile("/lock", make([]byte, page*2), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		first, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("first Mmap: %v", err)
		}
		defer syscall.Munmap(first)
		if _, err := f.WriteAt([]byte{1}, int64(page*4-1)); err != nil {
			t.Fatalf("WriteAt growth: %v", err)
		}
		second, err := syscall.Mmap(int(f.Fd()), int64(page*3), 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("second Mmap: %v", err)
		}
		defer syscall.Munmap(second)
		middle, err := syscall.Mmap(int(f.Fd()), int64(page), 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("middle Mmap: %v", err)
		}
		defer syscall.Munmap(middle)
		bridge, err := syscall.Mmap(int(f.Fd()), int64(page), page*2+4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("bridge Mmap: %v", err)
		}
		defer syscall.Munmap(bridge)
		middle[0] = 51
		if bridge[0] != 51 {
			t.Fatalf("bridge reads %d at its start, want the middle window's 51", bridge[0])
		}
		second[0] = 52
		if bridge[page*2] != 52 {
			t.Fatalf("bridge reads %d at the second window, want 52", bridge[page*2])
		}
		bridge[1] = 53
		if middle[1] != 53 {
			t.Fatalf("middle window reads %d, want the bridge's 53", middle[1])
		}
	})
}

func TestDSTFSMprotectNonMappingFallsThrough(t *testing.T) {
	simulation.Run(1, func() {
		b := make([]byte, syscall.Getpagesize())
		expectDSTRawSyscallPanic(t, func() {
			_ = syscall.Mprotect(b, 0)
		})
	})
}

func TestDSTFSVirtualFDMmapWritableFdatasyncCommitsMappedBytes(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/lock", make([]byte, 4), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		b, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap: %v", err)
		}
		defer syscall.Munmap(b)
		atomic.StoreUint32((*uint32)(unsafe.Pointer(&b[0])), 24)
		if err := syscall.Fdatasync(int(f.Fd())); err != nil {
			t.Fatalf("Fdatasync: %v", err)
		}
		_, synced, _, _, _, _, ok := os.DSTFSNodeState("/lock")
		if !ok {
			t.Fatalf("DSTFSNodeState missing /lock")
		}
		if want := []byte{24, 0, 0, 0}; !bytes.Equal([]byte(synced), want) {
			t.Fatalf("synced mapped bytes = %v, want uint32 value 24", []byte(synced))
		}
	})
}

func TestDSTFSVirtualFDMmapReadOnlyAndWritableShareRange(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", make([]byte, 4), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			roMapped := make(chan uintptr, 1)
			writerDone := make(chan struct{})
			readerErr := make(chan error, 1)
			go simulation.Process("reader", func() {
				f, err := os.Open("/lock")
				if err != nil {
					readerErr <- err
					return
				}
				defer f.Close()
				ro, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ, syscall.MAP_SHARED)
				if err != nil {
					readerErr <- err
					return
				}
				roMapped <- uintptr(unsafe.Pointer(&ro[0]))
				<-writerDone
				if got := atomic.LoadUint32((*uint32)(unsafe.Pointer(&ro[0]))); got != 11 {
					readerErr <- fmt.Errorf("read-only mapping after writable store = %d, want 11", got)
					return
				}
				readerErr <- syscall.Munmap(ro)
			})
			var roAddr uintptr
			select {
			case err := <-readerErr:
				t.Fatalf("reader map setup: %v", err)
			case roAddr = <-roMapped:
			}
			writerDoneClosed := false
			defer func() {
				if !writerDoneClosed {
					close(writerDone)
				}
			}()
			simulation.Process("writer", func() {
				f, err := os.OpenFile("/lock", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("Open writer: %v", err)
				}
				defer f.Close()
				rw, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Fatalf("writable Mmap: %v", err)
				}
				// Two mmaps of one file are distinct views of shared pages, as on
				// real Linux: distinct addresses, one set of bytes. The store below,
				// observed through the reader's mapping, pins the sharing.
				if rwAddr := uintptr(unsafe.Pointer(&rw[0])); rwAddr == roAddr {
					t.Fatalf("writable mapping reuses the read-only mapping's address %#x; real mmap returns distinct views", roAddr)
				}
				atomic.StoreUint32((*uint32)(unsafe.Pointer(&rw[0])), 11)
				if err := syscall.Munmap(rw); err != nil {
					t.Fatalf("writable Munmap: %v", err)
				}
			})
			close(writerDone)
			writerDoneClosed = true
			if err := <-readerErr; err != nil {
				t.Fatalf("reader mapped observation: %v", err)
			}
		})
	})
}

func TestDSTFSVirtualFDMmapCrossHostCapabilityRejected(t *testing.T) {
	simulation.Run(1, func() {
		var b []byte
		mapped := make(chan struct{})
		release := make(chan struct{})
		p1err := make(chan error, 1)
		simulation.Host("h1", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", make([]byte, 4), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			go simulation.Process("p1", func() {
				f, err := os.OpenFile("/lock", os.O_RDWR, 0)
				if err != nil {
					p1err <- err
					return
				}
				defer f.Close()
				var mmapErr error
				b, mmapErr = syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if mmapErr != nil {
					p1err <- mmapErr
					return
				}
				close(mapped)
				<-release
				p1err <- syscall.Munmap(b)
			})
		})
		select {
		case err := <-p1err:
			t.Fatalf("p1 map setup: %v", err)
		case <-mapped:
		}
		releaseClosed := false
		defer func() {
			if !releaseClosed {
				close(release)
			}
		}()
		simulation.Host("h2", simulation.HostConfig{}, func() {
			simulation.Process("p2", func() {
				if err := syscall.Mprotect(b, syscall.PROT_READ); !errors.Is(err, syscall.EINVAL) {
					t.Fatalf("cross-host Mprotect = %v, want EINVAL", err)
				}
				if err := syscall.Madvise(b, syscall.MADV_HUGEPAGE); !errors.Is(err, syscall.EINVAL) {
					t.Fatalf("cross-host Madvise = %v, want EINVAL", err)
				}
			})
		})
		close(release)
		releaseClosed = true
		if err := <-p1err; err != nil {
			t.Fatalf("p1 Munmap: %v", err)
		}
	})
}

func TestDSTFSVirtualFDMmapWritablePermissionsAndRegistrations(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/lock", make([]byte, 4), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		ro, err := os.Open("/lock")
		if err != nil {
			t.Fatalf("Open read-only: %v", err)
		}
		if _, err := syscall.Mmap(int(ro.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED); !errors.Is(err, syscall.EACCES) {
			t.Fatalf("writable Mmap on read-only fd = %v, want EACCES", err)
		}
		if err := ro.Close(); err != nil {
			t.Fatalf("Close read-only: %v", err)
		}

		f, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile O_RDWR: %v", err)
		}
		defer f.Close()
		b1, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("first writable Mmap: %v", err)
		}
		b2, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("second writable Mmap: %v", err)
		}
		if uintptr(unsafe.Pointer(&b1[0])) == uintptr(unsafe.Pointer(&b2[0])) {
			t.Fatalf("duplicate mappings share an address; real mmap returns distinct views")
		}
		b1[0] = 7
		if b2[0] != 7 {
			t.Fatalf("second mapping reads %d, want the first mapping's 7: the views do not share pages", b2[0])
		}
		if err := syscall.Mprotect(b1, syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
			t.Fatalf("Mprotect writable mapping read/write: %v", err)
		}
		if err := syscall.Munmap(b1); err != nil {
			t.Fatalf("first Munmap: %v", err)
		}
		if err := syscall.Mprotect(b2, syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
			t.Fatalf("Mprotect second mapping after first Munmap: %v", err)
		}
		if err := syscall.Munmap(b2); err != nil {
			t.Fatalf("second Munmap: %v", err)
		}
	})
}

func TestDSTFSVirtualFDMmapMixedProtectionDuplicateRegistrations(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/lock", make([]byte, 4), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/lock", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		ro, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("read-only Mmap: %v", err)
		}
		rw, err := syscall.Mmap(int(f.Fd()), 0, 4, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("writable Mmap: %v", err)
		}
		if uintptr(unsafe.Pointer(&ro[0])) == uintptr(unsafe.Pointer(&rw[0])) {
			t.Fatalf("mixed-protection mappings share an address, so they cannot differ in protection")
		}
		rw[0] = 9
		if ro[0] != 9 {
			t.Fatalf("read-only view reads %d, want the writable view's 9: the views do not share pages", ro[0])
		}
		if err := syscall.Mprotect(rw, syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
			t.Fatalf("Mprotect writable duplicate: %v", err)
		}
		if err := syscall.Munmap(rw); err != nil {
			t.Fatalf("Munmap writable duplicate: %v", err)
		}
		// The surviving duplicate is still resolvable in the registry after
		// the writable one unmapped — and, backed by the same O_RDWR fd, it
		// may upgrade (VM_MAYWRITE follows the descriptor, not the map-time
		// prot).
		if err := syscall.Mprotect(ro, syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
			t.Fatalf("Mprotect read-only duplicate after writable Munmap: %v", err)
		}
		ro[1] = 7
		if b, err := os.ReadFile("/lock"); err != nil || b[1] != 7 {
			t.Fatalf("file after upgraded-duplicate store = %v, %v; want byte1==7", b, err)
		}
		if err := syscall.Munmap(ro); err != nil {
			t.Fatalf("Munmap read-only duplicate: %v", err)
		}
	})
}

func TestDSTFSVirtualFDMmapUnsupportedShapes(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.OpenFile("/m", os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		if _, err := f.WriteString("hello"); err != nil {
			t.Fatalf("WriteString: %v", err)
		}
		fd := int(f.Fd())
		if _, err := syscall.Mmap(fd, 0, 0, syscall.PROT_READ, syscall.MAP_SHARED); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("zero-length Mmap = %v, want EINVAL", err)
		}
		if _, err := syscall.Mmap(fd, 1, 1, syscall.PROT_READ, syscall.MAP_SHARED); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("unaligned Mmap = %v, want EINVAL", err)
		}
		if b, err := syscall.Mmap(fd, 0, 6, syscall.PROT_READ, syscall.MAP_SHARED); err != nil {
			t.Fatalf("past-EOF Mmap = %v, want a reservation (real mmap allows it)", err)
		} else if err := syscall.Munmap(b); err != nil {
			t.Fatalf("Munmap reservation: %v", err)
		}
		// A window whose end overflows int64 can never fit an address space:
		// deterministic ENOMEM, never a small wrapped mapping that would hand
		// back the wrong bytes.
		if _, err := syscall.Mmap(fd, math.MaxInt64&^4095, 8192, syscall.PROT_READ, syscall.MAP_SHARED); !errors.Is(err, syscall.ENOMEM) {
			t.Fatalf("overflowing Mmap = %v, want ENOMEM", err)
		}
		if _, err := syscall.Mmap(fd, 0, 5, syscall.PROT_WRITE, syscall.MAP_SHARED); !errors.Is(err, syscall.EACCES) {
			t.Fatalf("writable Mmap = %v, want EACCES", err)
		}
		if _, err := syscall.Mmap(fd, 0, 5, syscall.PROT_READ, syscall.MAP_PRIVATE); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("private Mmap = %v, want EINVAL", err)
		}

		wo, err := os.OpenFile("/write-only", os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("OpenFile write-only: %v", err)
		}
		defer wo.Close()
		if _, err := wo.WriteString("hello"); err != nil {
			t.Fatalf("WriteString write-only: %v", err)
		}
		if _, err := syscall.Mmap(int(wo.Fd()), 0, 5, syscall.PROT_READ, syscall.MAP_SHARED); !errors.Is(err, syscall.EACCES) {
			t.Fatalf("write-only Mmap = %v, want EACCES", err)
		}

		dir, err := os.Open("/tmp")
		if err != nil {
			t.Fatalf("Open /tmp: %v", err)
		}
		defer dir.Close()
		if _, err := syscall.Mmap(int(dir.Fd()), 0, 1, syscall.PROT_READ, syscall.MAP_SHARED); !errors.Is(err, syscall.ENODEV) {
			t.Fatalf("directory Mmap = %v, want ENODEV", err)
		}
	})
}

func TestDSTFSVirtualFDFdatasyncCommitsFile(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.OpenFile("/sync-file", os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		if _, err := f.WriteString("abc"); err != nil {
			t.Fatalf("WriteString: %v", err)
		}
		if _, synced, _, _, _, _, ok := os.DSTFSNodeState("/sync-file"); !ok || synced != "" {
			t.Fatalf("pre-Fdatasync synced = %q, ok=%v; want empty durable image", synced, ok)
		}
		if err := syscall.Fdatasync(int(f.Fd())); err != nil {
			t.Fatalf("Fdatasync: %v", err)
		}
		if _, synced, _, _, _, _, ok := os.DSTFSNodeState("/sync-file"); !ok || synced != "abc" {
			t.Fatalf("post-Fdatasync synced = %q, ok=%v; want abc", synced, ok)
		}
		if _, err := f.WriteAt([]byte("Z"), 0); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := os.Chmod("/sync-file", 0o600); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		if _, synced, _, _, _, _, _ := os.DSTFSNodeState("/sync-file"); synced != "abc" {
			t.Fatalf("post-write synced = %q, want abc", synced)
		}
		if err := syscall.Fsync(int(f.Fd())); err != nil {
			t.Fatalf("Fsync: %v", err)
		}
		if _, synced, _, _, _, _, ok := os.DSTFSNodeState("/sync-file"); !ok || synced != "Zbc" {
			t.Fatalf("post-Fsync synced = %q, ok=%v; want Zbc", synced, ok)
		}
		_, _, _, _, _, syncedModeBefore, _ := os.DSTFSNodeState("/sync-file")
		if err := os.Chmod("/sync-file", 0o400); err != nil {
			t.Fatalf("Chmod after Fsync: %v", err)
		}
		if _, err := f.WriteAt([]byte("Y"), 0); err != nil {
			t.Fatalf("WriteAt after Fsync: %v", err)
		}
		if err := syscall.Fdatasync(int(f.Fd())); err != nil {
			t.Fatalf("Fdatasync after Chmod: %v", err)
		}
		if _, synced, _, _, mode, syncedMode, ok := os.DSTFSNodeState("/sync-file"); !ok || synced != "Ybc" || mode != 0o400 || syncedMode != syncedModeBefore {
			t.Fatalf("post-Fdatasync state synced=%q mode=%v syncedMode=%v ok=%v; want Ybc/0400/%v", synced, mode, syncedMode, ok, syncedModeBefore)
		}
	})
}

func TestDSTFSVirtualFDFsyncCommitsDirectoryEntries(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.Mkdir("/sync-dir", 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if err := os.WriteFile("/sync-dir/one", nil, 0o644); err != nil {
			t.Fatalf("WriteFile one: %v", err)
		}
		if _, _, cur, synced, _, _, ok := os.DSTFSNodeState("/sync-dir"); !ok || len(cur) != 1 || cur[0] != "one" || len(synced) != 0 {
			t.Fatalf("pre-Fsync entries = %v/%v, ok=%v; want one/empty", cur, synced, ok)
		}

		dir, err := os.Open("/sync-dir")
		if err != nil {
			t.Fatalf("Open dir: %v", err)
		}
		defer dir.Close()
		if err := syscall.Fdatasync(int(dir.Fd())); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("Fdatasync dir = %v, want EINVAL", err)
		}
		if err := syscall.Fsync(int(dir.Fd())); err != nil {
			t.Fatalf("Fsync dir: %v", err)
		}
		if _, _, cur, synced, _, _, ok := os.DSTFSNodeState("/sync-dir"); !ok || len(cur) != 1 || cur[0] != "one" || len(synced) != 1 || synced[0] != "one" {
			t.Fatalf("post-Fsync entries = %v/%v, ok=%v; want one/one", cur, synced, ok)
		}
		if err := os.Remove("/sync-dir/one"); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if err := os.WriteFile("/sync-dir/two", nil, 0o644); err != nil {
			t.Fatalf("WriteFile two: %v", err)
		}
		if _, _, cur, synced, _, _, ok := os.DSTFSNodeState("/sync-dir"); !ok || len(cur) != 1 || cur[0] != "two" || len(synced) != 1 || synced[0] != "one" {
			t.Fatalf("post-mutation entries = %v/%v, ok=%v; want two/one", cur, synced, ok)
		}
		if err := syscall.Fsync(int(dir.Fd())); err != nil {
			t.Fatalf("Fsync dir again: %v", err)
		}
		if _, _, cur, synced, _, _, ok := os.DSTFSNodeState("/sync-dir"); !ok || len(cur) != 1 || cur[0] != "two" || len(synced) != 1 || synced[0] != "two" {
			t.Fatalf("post-second-Fsync entries = %v/%v, ok=%v; want two/two", cur, synced, ok)
		}
	})
}

func TestDSTFSVirtualFDSyncFrontDoorsFailWithoutCommitting(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			f, err := os.OpenFile("/sync-eio", os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer f.Close()
			if _, err := f.WriteString("dirty"); err != nil {
				t.Fatalf("WriteString: %v", err)
			}
			fd := int(f.Fd())
			simulation.FailDisk("h")
			if err := syscall.Fdatasync(fd); !errors.Is(err, syscall.EIO) {
				t.Fatalf("Fdatasync under FailDisk = %v, want EIO", err)
			}
			if err := syscall.Fsync(fd); !errors.Is(err, syscall.EIO) {
				t.Fatalf("Fsync under FailDisk = %v, want EIO", err)
			}
			if _, synced, _, _, _, _, ok := os.DSTFSNodeState("/sync-eio"); !ok || synced != "" {
				t.Fatalf("synced image after failed syncs = %q, ok=%v; want empty", synced, ok)
			}
			simulation.HealDisk("h")
			// fsyncgate: the failed syncs dropped the dirty pages from the
			// writeback set, so the retried sync SUCCEEDS without the data
			// reaching the platter — the durable image holds the committed
			// size with never-written (zero) pages, not "dirty".
			if err := syscall.Fdatasync(fd); err != nil {
				t.Fatalf("Fdatasync after HealDisk: %v", err)
			}
			if _, synced, _, _, _, _, ok := os.DSTFSNodeState("/sync-eio"); !ok || synced != "\x00\x00\x00\x00\x00" {
				t.Fatalf("synced image after heal = %q, ok=%v; want five never-written zero bytes (the dropped pages must not reach the platter)", synced, ok)
			}
			// Rewriting the page redirties it; the next sync commits it.
			if _, err := f.WriteAt([]byte("dirty"), 0); err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if err := syscall.Fdatasync(fd); err != nil {
				t.Fatalf("Fdatasync after rewrite: %v", err)
			}
			if _, synced, _, _, _, _, ok := os.DSTFSNodeState("/sync-eio"); !ok || synced != "dirty" {
				t.Fatalf("synced image after rewrite+sync = %q, ok=%v; want dirty", synced, ok)
			}
		})
	})
}

func TestDSTFSOpenRootVirtualFDSyscalls(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.Mkdir("/root", 0o755); err != nil {
			t.Fatalf("Mkdir root: %v", err)
		}
		r, err := os.OpenRoot("/root")
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		defer r.Close()
		f, err := r.OpenFile("viafd", os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatalf("Root.OpenFile viafd: %v", err)
		}
		fd := int(f.Fd())
		if fd < 1<<30 {
			t.Fatalf("rooted Fd = %d, want virtual descriptor", fd)
		}
		if n, err := syscall.Write(fd, []byte("fd")); n != 2 || err != nil {
			t.Fatalf("syscall.Write rooted fd = %d, %v; want 2, nil", n, err)
		}
		if off, err := syscall.Seek(fd, 0, io.SeekStart); off != 0 || err != nil {
			t.Fatalf("syscall.Seek rooted fd = %d, %v; want 0, nil", off, err)
		}
		buf := make([]byte, 2)
		if n, err := syscall.Read(fd, buf); n != 2 || err != nil || string(buf) != "fd" {
			t.Fatalf("syscall.Read rooted fd = %d, %v, %q; want 2, nil, fd", n, err, buf)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("rooted fd Close: %v", err)
		}
	})
}

func TestDSTFSVirtualFDFlockExclusiveBlocksUntilClose(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			locked := make(chan struct{})
			release := make(chan struct{})
			p1err := make(chan error, 1)
			go simulation.Process("p1", func() {
				f, err := os.OpenFile("/lock", os.O_RDWR, 0)
				if err != nil {
					p1err <- err
					return
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					p1err <- err
					return
				}
				close(locked)
				<-release
				p1err <- f.Close()
			})
			select {
			case err := <-p1err:
				t.Fatalf("p1 lock setup: %v", err)
			case <-locked:
			}
			releaseClosed := false
			defer func() {
				if !releaseClosed {
					close(release)
				}
			}()

			attempting := make(chan struct{})
			nbRes := make(chan error, 1)
			done := make(chan error, 1)
			go simulation.Process("p2", func() {
				f, err := os.OpenFile("/lock", os.O_RDWR, 0)
				if err != nil {
					nbRes <- fmt.Errorf("p2 OpenFile: %w", err)
					return
				}
				nbRes <- syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				close(attempting)
				done <- syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
				f.Close()
			})
			if nbErr := <-nbRes; !errors.Is(nbErr, syscall.EWOULDBLOCK) {
				t.Fatalf("p2 LOCK_EX|LOCK_NB = %v, want EWOULDBLOCK", nbErr)
			}
			<-attempting
			synctest.Wait()
			select {
			case err := <-done:
				t.Fatalf("blocking Flock returned before release: %v", err)
			default:
			}
			close(release)
			releaseClosed = true
			if err := <-done; err != nil {
				t.Fatalf("blocking Flock after close release: %v", err)
			}
			if err := <-p1err; err != nil {
				t.Fatalf("p1 Close: %v", err)
			}
		})
	})
}

func TestDSTFSVirtualFDFlockSharedAndFDOwnership(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			locked := make(chan struct{})
			release := make(chan struct{})
			p1err := make(chan error, 1)
			go simulation.Process("p1", func() {
				f, err := os.Open("/lock")
				if err != nil {
					p1err <- err
					return
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
					p1err <- err
					return
				}
				close(locked)
				<-release
				p1err <- f.Close()
			})
			select {
			case err := <-p1err:
				t.Fatalf("p1 shared lock setup: %v", err)
			case <-locked:
			}
			releaseClosed := false
			defer func() {
				if !releaseClosed {
					close(release)
				}
			}()

			simulation.Process("p2", func() {
				f, err := os.Open("/lock")
				if err != nil {
					t.Fatalf("p2 Open: %v", err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
					t.Fatalf("p2 LOCK_SH|LOCK_NB: %v", err)
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
					t.Fatalf("p2 upgrade while p1 shared = %v, want EWOULDBLOCK", err)
				}
			})

			simulation.Process("p3", func() {
				f, err := os.Open("/lock")
				if err != nil {
					t.Fatalf("p3 Open: %v", err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
					t.Fatalf("p3 LOCK_EX|LOCK_NB while shared held = %v, want EWOULDBLOCK", err)
				}
			})

			close(release)
			releaseClosed = true
			if err := <-p1err; err != nil {
				t.Fatalf("p1 Close: %v", err)
			}
			simulation.Process("p3", func() {
				f, err := os.Open("/lock")
				if err != nil {
					t.Fatalf("p3 reopen: %v", err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatalf("p3 LOCK_EX after shared release: %v", err)
				}
			})

			simulation.Process("p4", func() {
				f1, err := os.Open("/lock")
				if err != nil {
					t.Fatalf("p4 Open f1: %v", err)
				}
				defer f1.Close()
				f2, err := os.Open("/lock")
				if err != nil {
					t.Fatalf("p4 Open f2: %v", err)
				}
				defer f2.Close()
				if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_EX); err != nil {
					t.Fatalf("p4 f1 LOCK_EX: %v", err)
				}
				if err := syscall.Flock(int(f2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
					t.Fatalf("p4 f2 LOCK_EX|LOCK_NB with f1 held = %v, want EWOULDBLOCK", err)
				}
				if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_UN); err != nil {
					t.Fatalf("p4 f1 LOCK_UN: %v", err)
				}
				if err := syscall.Flock(int(f2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatalf("p4 f2 LOCK_EX after f1 unlock: %v", err)
				}
			})
		})
	})
}

func TestDSTFSVirtualFDFlockDowngradeWakesSharedWaiter(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			locked := make(chan struct{})
			downgrade := make(chan struct{})
			downgraded := make(chan error, 1)
			release := make(chan struct{})
			p1err := make(chan error, 1)
			go simulation.Process("p1", func() {
				f, err := os.Open("/lock")
				if err != nil {
					p1err <- err
					return
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					p1err <- err
					return
				}
				close(locked)
				<-downgrade
				downgraded <- syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
				<-release
				p1err <- f.Close()
			})
			select {
			case err := <-p1err:
				t.Fatalf("p1 lock setup: %v", err)
			case <-locked:
			}
			downgradeClosed := false
			releaseClosed := false
			defer func() {
				if !downgradeClosed {
					close(downgrade)
				}
				if !releaseClosed {
					close(release)
				}
			}()

			attempting := make(chan struct{})
			done := make(chan error, 1)
			go simulation.Process("p2", func() {
				f, err := os.Open("/lock")
				if err != nil {
					done <- fmt.Errorf("p2 Open: %w", err)
					return
				}
				close(attempting)
				err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
				if closeErr := f.Close(); err == nil {
					err = closeErr
				}
				done <- err
			})
			select {
			case err := <-done:
				t.Fatalf("p2 setup: %v", err)
			case <-attempting:
			}
			synctest.Wait()
			select {
			case err := <-done:
				t.Fatalf("shared Flock returned before downgrade: %v", err)
			default:
			}

			close(downgrade)
			downgradeClosed = true
			if err := <-downgraded; err != nil {
				t.Fatalf("p1 downgrade: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("shared Flock after downgrade: %v", err)
			}
			close(release)
			releaseClosed = true
			if err := <-p1err; err != nil {
				t.Fatalf("p1 Close: %v", err)
			}
		})
	})
}

func TestDSTFSVirtualFDFlockConcurrentSharedUpgradesMakeProgress(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			ready := make(chan struct{}, 2)
			start := make(chan struct{})
			done := make(chan error, 2)
			startUpgrader := func(name string) {
				go simulation.Process(name, func() {
					f, err := os.Open("/lock")
					if err != nil {
						done <- err
						return
					}
					if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
						done <- err
						return
					}
					ready <- struct{}{}
					<-start
					if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
						done <- err
						f.Close()
						return
					}
					done <- f.Close()
				})
			}
			startUpgrader("p1")
			startUpgrader("p2")
			startClosed := false
			defer func() {
				if !startClosed {
					close(start)
				}
			}()
			for i := 0; i < 2; i++ {
				select {
				case err := <-done:
					t.Fatalf("shared lock setup: %v", err)
				case <-ready:
				}
			}

			close(start)
			startClosed = true
			for i := 0; i < 2; i++ {
				if err := <-done; err != nil {
					t.Fatalf("upgrade %d: %v", i, err)
				}
			}
		})
	})
}

func TestDSTFSVirtualFDFlockScopesByHostAndFileNode(t *testing.T) {
	simulation.Run(1, func() {
		locked := make(chan struct{})
		release := make(chan struct{})
		p1err := make(chan error, 1)
		simulation.Host("h1", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
				t.Fatalf("h1 WriteFile: %v", err)
			}
			go simulation.Process("p1", func() {
				f, err := os.Open("/lock")
				if err != nil {
					p1err <- err
					return
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					p1err <- err
					return
				}
				close(locked)
				<-release
				p1err <- f.Close()
			})
		})
		select {
		case err := <-p1err:
			t.Fatalf("h1 lock setup: %v", err)
		case <-locked:
		}
		releaseClosed := false
		defer func() {
			if !releaseClosed {
				close(release)
			}
		}()

		simulation.Host("h2", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
				t.Fatalf("h2 WriteFile: %v", err)
			}
			simulation.Process("p2", func() {
				f, err := os.Open("/lock")
				if err != nil {
					t.Fatalf("h2 Open: %v", err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatalf("h2 same-path LOCK_EX|LOCK_NB: %v", err)
				}
			})
		})
		close(release)
		releaseClosed = true
		if err := <-p1err; err != nil {
			t.Fatalf("h1 Close: %v", err)
		}
	})
}

func TestDSTFSVirtualFDFlockFollowsFileNodeAcrossRename(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			locked := make(chan struct{})
			release := make(chan struct{})
			p1err := make(chan error, 1)
			go simulation.Process("p1", func() {
				f, err := os.Open("/lock")
				if err != nil {
					p1err <- err
					return
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					p1err <- err
					return
				}
				close(locked)
				<-release
				p1err <- f.Close()
			})
			select {
			case err := <-p1err:
				t.Fatalf("p1 lock setup: %v", err)
			case <-locked:
			}
			releaseClosed := false
			defer func() {
				if !releaseClosed {
					close(release)
				}
			}()

			if err := os.Rename("/lock", "/renamed"); err != nil {
				t.Fatalf("Rename: %v", err)
			}
			simulation.Process("p2", func() {
				f, err := os.Open("/renamed")
				if err != nil {
					t.Fatalf("p2 Open renamed: %v", err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
					t.Fatalf("LOCK_EX|LOCK_NB on renamed node = %v, want EWOULDBLOCK", err)
				}
			})
			close(release)
			releaseClosed = true
			if err := <-p1err; err != nil {
				t.Fatalf("p1 Close: %v", err)
			}
			simulation.Process("p2", func() {
				f, err := os.Open("/renamed")
				if err != nil {
					t.Fatalf("p2 reopen renamed: %v", err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatalf("LOCK_EX after original fd close: %v", err)
				}
			})
		})
	})
}

func TestDSTFSVirtualFDFlockDirectory(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.Mkdir("/dir", 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		d1, err := os.Open("/dir")
		if err != nil {
			t.Fatalf("Open d1: %v", err)
		}
		defer d1.Close()
		if err := syscall.Flock(int(d1.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatalf("d1 LOCK_EX: %v", err)
		}
		d2, err := os.Open("/dir")
		if err != nil {
			t.Fatalf("Open d2: %v", err)
		}
		defer d2.Close()
		if err := syscall.Flock(int(d2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatalf("d2 LOCK_EX|LOCK_NB while d1 held = %v, want EWOULDBLOCK", err)
		}
		if err := syscall.Flock(int(d1.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatalf("d1 LOCK_UN: %v", err)
		}
		if err := syscall.Flock(int(d2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("d2 LOCK_EX after unlock: %v", err)
		}
	})
}

func TestDSTFSVirtualFDFlockSyscallCloseReleases(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.Open("/lock")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		fd := int(f.Fd())
		if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
			t.Fatalf("LOCK_EX: %v", err)
		}
		if err := syscall.Close(fd); err != nil {
			t.Fatalf("syscall.Close: %v", err)
		}
		if err := f.Close(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("File.Close after syscall.Close = %v, want ErrClosed", err)
		}

		g, err := os.Open("/lock")
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer g.Close()
		if err := syscall.Flock(int(g.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("LOCK_EX after syscall.Close release: %v", err)
		}
	})
}

func TestDSTFSVirtualFDFlockUnlockWakesBlockedExclusive(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f1, err := os.Open("/lock")
		if err != nil {
			t.Fatalf("Open f1: %v", err)
		}
		defer f1.Close()
		if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatalf("f1 LOCK_EX: %v", err)
		}

		attempting := make(chan struct{})
		done := make(chan error, 1)
		f2, err := os.Open("/lock")
		if err != nil {
			t.Fatalf("Open f2: %v", err)
		}
		go func() {
			close(attempting)
			err := syscall.Flock(int(f2.Fd()), syscall.LOCK_EX)
			if closeErr := f2.Close(); err == nil {
				err = closeErr
			}
			done <- err
		}()
		<-attempting
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("blocking Flock returned before unlock: %v", err)
		default:
		}
		if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatalf("f1 LOCK_UN: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("blocking Flock after unlock: %v", err)
		}
	})
}

// TestDSTFSVirtualFDRawSyscallSemantics: the raw path is not merely "errno 0" —
// it performs the operation. A raw flock really excludes a peer and a raw
// LOCK_UN really releases; a raw fdatasync really commits the durable image; a
// raw close really invalidates the descriptor.
func TestDSTFSVirtualFDRawSyscallSemantics(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/db", []byte("old"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			held := make(chan struct{})
			release := make(chan struct{})
			done := make(chan error, 1)
			go simulation.Process("holder", func() {
				f, err := os.OpenFile("/db", os.O_RDWR, 0)
				if err != nil {
					done <- err
					return
				}
				// Raw LOCK_EX, the x/sys way.
				if _, _, e := syscall.Syscall(syscall.SYS_FLOCK, f.Fd(), syscall.LOCK_EX, 0); e != 0 {
					done <- e
					return
				}
				// Raw fdatasync must move the durable image.
				if _, err := f.WriteAt([]byte("new"), 0); err != nil {
					done <- err
					return
				}
				if _, _, e := syscall.Syscall(syscall.SYS_FDATASYNC, f.Fd(), 0, 0); e != 0 {
					done <- e
					return
				}
				close(held)
				<-release
				// Raw LOCK_UN really releases.
				if _, _, e := syscall.Syscall(syscall.SYS_FLOCK, f.Fd(), syscall.LOCK_UN, 0); e != 0 {
					done <- e
					return
				}
				done <- f.Close()
			})
			<-held

			simulation.Process("peer", func() {
				f, err := os.OpenFile("/db", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("peer open: %v", err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
					t.Fatalf("peer flock while a RAW flock is held = %v, want EWOULDBLOCK", err)
				}
			})
			if _, synced, _, _, _, _, ok := os.DSTFSNodeState("/db"); !ok || string(synced) != "new" {
				t.Fatalf("durable image after a RAW fdatasync = %q, want %q", synced, "new")
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("holder: %v", err)
			}

			// Raw close really invalidates the descriptor.
			g, err := os.Open("/db")
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			fd := g.Fd()
			if _, _, e := syscall.Syscall(syscall.SYS_CLOSE, fd, 0, 0); e != 0 {
				t.Fatalf("raw close: %v", e)
			}
			if _, _, e := syscall.Syscall(syscall.SYS_FSYNC, fd, 0, 0); e != syscall.EBADF {
				t.Fatalf("raw fsync after raw close = %v, want EBADF", e)
			}
		})
	})
}

// TestDSTRawSyscallHostFdSurvivesBubbleClose: a bubble goroutine's close of a
// pre-run host handle is answered EBADF at the trampolines and NEVER
// dispatched — the host-close fence (syscall's dstSyscallHostClose): a
// dispatched close of a then-free number races the harness assigning that
// number to a newborn host fd, so bubble-originated destruction of host fds
// is refused for the whole real-fd space. The handle surviving the run is the
// recorded accepted divergence of the inherited-handle stance (design.md
// "The interception boundary"); the non-close allowlist family still reaches
// the host (pinned elsewhere — the post-run fcntl below runs outside the
// fence and proves only that the fd survived).
func TestDSTRawSyscallHostFdSurvivesBubbleClose(t *testing.T) {
	// A real descriptor of our own, opened BEFORE the run (the inherited-handle
	// stance) and owned by nothing else: dup a descriptor rather than borrowing
	// one the test harness tracks.
	dup, err := syscall.Dup(0)
	if err != nil {
		t.Skipf("dup(0): %v", err)
	}
	defer syscall.Close(dup)
	fd := uintptr(dup)
	simulation.Run(1, func() {
		if _, _, e := syscall.Syscall(syscall.SYS_CLOSE, fd, 0, 0); e != syscall.EBADF {
			t.Fatalf("bubble close of a pre-run host fd = %v, want EBADF (the host-close fence must refuse dispatch)", e)
		}
	})
	// The descriptor survives: the close never reached the kernel.
	if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, fd, uintptr(syscall.F_GETFD), 0); e != 0 {
		t.Fatalf("fcntl on the surviving host fd = %v, want success (the bubble close must never reach the host)", e)
	}
}

func TestDSTFSVirtualFDFlockRawTrampolinesFenced(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func(fd uintptr)
	}{
		// Syscall dispatches flock now (see TestDSTFSVirtualFDRawSyscallDispatch);
		// the raw trampolines, which have no P for the dispatch, still refuse.
		{"RawSyscall", func(fd uintptr) { syscall.RawSyscall(syscall.SYS_FLOCK, fd, syscall.LOCK_EX, 0) }},
		{"RawSyscall6", func(fd uintptr) { syscall.RawSyscall6(syscall.SYS_FLOCK, fd, syscall.LOCK_EX, 0, 0, 0, 0) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			simulation.Run(1, func() {
				f, err := os.Create("/lock")
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				fd := f.Fd()
				defer f.Close()

				defer func() {
					r := recover()
					if r == nil || !strings.Contains(fmt.Sprint(r), "unsupported under deterministic simulation") {
						t.Fatalf("raw flock panic = %v, want unsupported-under-simulation", r)
					}
				}()
				tt.call(fd)
			})
		})
	}
}

func TestDSTFSVirtualFDFlockSlowDiskNoDelay(t *testing.T) {
	const lat = 50 * time.Millisecond
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			f, err := os.Open("/lock")
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer f.Close()

			simulation.SlowDisk("h", lat)
			t0 := time.Now()
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatalf("LOCK_EX: %v", err)
			}
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatalf("LOCK_UN: %v", err)
			}
			if d := time.Since(t0); d != 0 {
				t.Fatalf("Flock under SlowDisk took %v, want 0", d)
			}
		})
	})
}

func TestDSTFSVirtualFDFlockErrors(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.OpenFile("/lock", os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		fd := int(f.Fd())
		if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
			t.Fatalf("Flock(LOCK_UN without holder): %v", err)
		}
		if err := syscall.Flock(fd, syscall.LOCK_NB); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("Flock(LOCK_NB only) = %v, want EINVAL", err)
		}
		if err := syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_EX); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("Flock(LOCK_SH|LOCK_EX) = %v, want EINVAL", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := syscall.Flock(fd, syscall.LOCK_UN); !errors.Is(err, syscall.EBADF) {
			t.Fatalf("Flock after close = %v, want EBADF", err)
		}
	})
}

// TestDSTFSMmapAddressDeterministicAcrossRuns pins the run boundary's half of
// address determinism at the os level: the filesystem roll must reset the
// mapping region (dstNodeReleaseRunLocked), or the second run's identical
// schedule would carve at a bump offset the first run advanced — same seed,
// different address, and a pointer-keyed map in the system under test iterates
// differently on replay.
func TestDSTFSMmapAddressDeterministicAcrossRuns(t *testing.T) {
	one := func() uintptr {
		var addr uintptr
		simulation.Run(1, func() {
			if err := os.WriteFile("/lock", make([]byte, 8), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			f, err := os.OpenFile("/lock", os.O_RDWR, 0)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer f.Close()
			b, err := syscall.Mmap(int(f.Fd()), 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
			if err != nil {
				t.Fatalf("Mmap: %v", err)
			}
			defer syscall.Munmap(b)
			addr = uintptr(unsafe.Pointer(&b[0]))
		})
		return addr
	}
	a1 := one()
	a2 := one()
	if a1 != a2 {
		t.Fatalf("one seed, two runs, two mapping addresses: %#x vs %#x", a1, a2)
	}
}

// TestDSTFSMprotectDowngradeIsEnforcedByHardware pins the half of Mprotect a
// return code cannot: after Mprotect(PROT_READ) on a writable mapping, a store
// through it is a protection fault, and the fault is the simulated process's
// death — unswallowable, leaving its peer and the harness running. Before the
// page cache, Mprotect was bookkeeping and this store silently succeeded.
func TestDSTFSMprotectDowngradeIsEnforcedByHardware(t *testing.T) {
	var recovered, pastStore, peerRan bool
	var victimPID int
	var killErr error
	simulation.Test(t, 1, func(t *testing.T) {
		simulation.Host("h", simulation.HostConfig{}, func() {
			downgraded := make(chan []byte, 1)
			pid := make(chan int, 1)
			go simulation.Process("victim", func() {
				defer func() {
					if r := recover(); r != nil {
						recovered = true
					}
				}()
				f, err := os.OpenFile("/m", os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					t.Errorf("OpenFile: %v", err)
					return
				}
				if _, err := f.Write(make([]byte, 8)); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
				b, err := syscall.Mmap(int(f.Fd()), 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				f.Close()
				if err != nil {
					t.Errorf("Mmap: %v", err)
					return
				}
				b[0] = 1 // writable: fine
				if err := syscall.Mprotect(b, syscall.PROT_READ); err != nil {
					t.Errorf("Mprotect: %v", err)
					return
				}
				pid <- os.Getpid()
				downgraded <- b
				b[1] = 2 // downgraded: production SIGSEGVs, so must we
				pastStore = true
			})
			b := <-downgraded
			victimPID = <-pid
			_ = b
			simulation.Process("peer", func() {
				killErr = syscall.Kill(victimPID, 0)
				peerRan = true
			})
		})
	})
	if pastStore {
		t.Errorf("the victim ran past a store through a read-only-downgraded mapping")
	}
	if recovered {
		t.Errorf("the victim recovered from a protection fault production delivers as SIGSEGV")
	}
	if !peerRan {
		t.Errorf("the peer did not run: the fault took down more than its process")
	}
	if killErr != syscall.ESRCH {
		t.Errorf("Kill(victim, 0) = %v, want ESRCH", killErr)
	}
}

// faultShapeInProcess runs touch inside a simulated process (with a recover
// and SetPanicOnFault armed — the strongest survival attempt the language
// affords) and reports whether the process died unswallowably while a peer
// outlived it.
func faultShapeInProcess(t *testing.T, setup func(t *testing.T) func(), check func(died, recovered, peerRan bool, killErr error)) {
	t.Helper()
	var recovered, past, peer bool
	var victimPID int
	var killErr error
	simulation.Test(t, 1, func(t *testing.T) {
		simulation.Host("h", simulation.HostConfig{}, func() {
			touch := setup(t)
			ready := make(chan int, 1)
			go simulation.Process("victim", func() {
				defer func() {
					if r := recover(); r != nil {
						recovered = true
					}
				}()
				debug.SetPanicOnFault(true)
				ready <- os.Getpid()
				touch()
				past = true
			})
			victimPID = <-ready
			simulation.Process("peer", func() {
				killErr = syscall.Kill(victimPID, 0)
				peer = true
			})
		})
	})
	check(!past && killErr == syscall.ESRCH, recovered, peer, killErr)
}

// TestDSTFSMappingFaultShapes: the two shapes gmdb's pager performs on its hot
// path, now behaving as the kernel behaves. A RESERVATION maps more than the
// file holds — reads inside the file and inside its last partial page are
// ordinary, a read from a page wholly past EOF is the process's death, and
// growing the file makes the page readable with no remap. A SHRINK under the
// live mapping cuts pages out from under it — the truncate succeeds, and it
// is the later ACCESS that dies.
func TestDSTFSMappingFaultShapes(t *testing.T) {
	const page = 4096 // the SIMULATED page size; a coarser host is refused before any boundary matters
	t.Run("reservation past EOF", func(t *testing.T) {
		faultShapeInProcess(t,
			func(t *testing.T) func() {
				if err := os.WriteFile("/db", []byte("hdr"), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				f, err := os.OpenFile("/db", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("OpenFile: %v", err)
				}
				b, err := syscall.Mmap(int(f.Fd()), 0, 4*page, syscall.PROT_READ, syscall.MAP_SHARED)
				f.Close()
				if err != nil {
					t.Fatalf("reservation Mmap: %v", err)
				}
				if b[0] != 'h' || b[3] != 0 || b[page-1] != 0 {
					t.Fatalf("file bytes and partial-page tail = %q %d %d, want 'h' 0 0", b[0], b[3], b[page-1])
				}
				// Grow the file over page 1: the page becomes readable through
				// the SAME mapping — the reservation extends, no remap.
				g, err := os.OpenFile("/db", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("reopen: %v", err)
				}
				if _, err := g.WriteAt([]byte{7}, int64(page)); err != nil {
					t.Fatalf("growth WriteAt: %v", err)
				}
				g.Close()
				if b[page] != 7 {
					t.Fatalf("grown page reads %d through the reservation, want 7", b[page])
				}
				return func() {
					pcSinkOS.Store(uint32(b[2*page])) // wholly past EOF: SIGBUS
				}
			},
			func(died, recovered, peerRan bool, killErr error) {
				if !died {
					t.Errorf("the process survived a read from a reservation page past EOF")
				}
				if recovered {
					t.Errorf("the process recovered from a fault production delivers as SIGBUS")
				}
				if !peerRan {
					t.Errorf("the peer did not run")
				}
			})
	})
	t.Run("munmap then touch", func(t *testing.T) {
		faultShapeInProcess(t,
			func(t *testing.T) func() {
				if err := os.WriteFile("/db", make([]byte, page), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				f, err := os.OpenFile("/db", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("OpenFile: %v", err)
				}
				b, err := syscall.Mmap(int(f.Fd()), 0, page, syscall.PROT_READ, syscall.MAP_SHARED)
				f.Close()
				if err != nil {
					t.Fatalf("Mmap: %v", err)
				}
				pcSinkOS.Store(uint32(b[0])) // mapped: fine
				if err := syscall.Munmap(b); err != nil {
					t.Fatalf("Munmap: %v", err)
				}
				return func() {
					pcSinkOS.Store(uint32(b[0])) // unmapped: the toucher's SIGSEGV
				}
			},
			func(died, recovered, peerRan bool, killErr error) {
				if !died {
					t.Errorf("the process survived touching memory it unmapped")
				}
				if recovered {
					t.Errorf("the process recovered from a fault production delivers as SIGSEGV")
				}
				if !peerRan {
					t.Errorf("the peer did not run")
				}
			})
	})
	t.Run("shrink under live mapping", func(t *testing.T) {
		faultShapeInProcess(t,
			func(t *testing.T) func() {
				if err := os.WriteFile("/db", make([]byte, 2*page), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				f, err := os.OpenFile("/db", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("OpenFile: %v", err)
				}
				b, err := syscall.Mmap(int(f.Fd()), 0, 2*page, syscall.PROT_READ, syscall.MAP_SHARED)
				f.Close()
				if err != nil {
					t.Fatalf("Mmap: %v", err)
				}
				if err := os.Truncate("/db", int64(page)); err != nil {
					t.Fatalf("shrink under live mapping: %v", err)
				}
				pcSinkOS.Store(uint32(b[0])) // within the file: fine
				return func() {
					pcSinkOS.Store(uint32(b[page])) // cut away: SIGBUS
				}
			},
			func(died, recovered, peerRan bool, killErr error) {
				if !died {
					t.Errorf("the process survived a read from a page the truncate cut away")
				}
				if recovered {
					t.Errorf("the process recovered from a fault production delivers as SIGBUS")
				}
				if !peerRan {
					t.Errorf("the peer did not run")
				}
			})
	})
}

// pcSinkOS keeps a load from being optimized away: an elided load is an
// elided fault, and the test would pass for the wrong reason.
var pcSinkOS atomic.Uint32

// TestDSTFSMprotectNoneIsEnforcedByHardware: PROT_NONE is applied to the
// real page tables, not just acknowledged — a read through a NONE-protected
// mapping faults, killing exactly the touching simulated process, as
// production SIGSEGV does.
func TestDSTFSMprotectNoneIsEnforcedByHardware(t *testing.T) {
	faultShapeInProcess(t,
		func(t *testing.T) func() {
			f, err := os.OpenFile("/none", os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer f.Close()
			if _, err := f.Write(make([]byte, 8)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			b, err := syscall.Mmap(int(f.Fd()), 0, 8, syscall.PROT_READ, syscall.MAP_SHARED)
			if err != nil {
				t.Fatalf("Mmap: %v", err)
			}
			if err := syscall.Mprotect(b, syscall.PROT_NONE); err != nil {
				t.Fatalf("Mprotect(PROT_NONE): %v", err)
			}
			return func() { // a read through PROT_NONE must fault
				if b[0] == 42 { // the compare forces the load
					t.Log("unreachable")
				}
			}
		},
		func(died, recovered, peerRan bool, killErr error) {
			if !died {
				t.Errorf("the victim survived a read through a PROT_NONE mapping")
			}
			if recovered {
				t.Errorf("the victim recovered from a fault production delivers as SIGSEGV")
			}
			if !peerRan {
				t.Errorf("the peer did not run: the fault took down more than its process")
			}
			if killErr != syscall.ESRCH {
				t.Errorf("Kill(victim, 0) = %v, want ESRCH", killErr)
			}
		})
}

// TestDSTFSVirtualFDFlockCloseWhileBlocked: a blocked flock waiter whose fd
// is closed elsewhere is NOT woken with EBADF — Linux's in-flight flock holds
// a reference to the open file description, so the wait survives the close
// and the call GRANTS once the lock becomes available; the description's last
// reference (the in-flight call itself) drops at return, so the grant records
// nothing and a fresh lock succeeds immediately.
func TestDSTFSVirtualFDFlockCloseWhileBlocked(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			f1, err := os.OpenFile("/lock", os.O_RDWR, 0)
			if err != nil {
				t.Fatalf("open f1: %v", err)
			}
			defer f1.Close()
			if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatalf("f1 lock: %v", err)
			}
			f2, err := os.OpenFile("/lock", os.O_RDWR, 0)
			if err != nil {
				t.Fatalf("open f2: %v", err)
			}
			fd2 := int(f2.Fd())
			res := make(chan error, 1)
			go func() {
				res <- syscall.Flock(fd2, syscall.LOCK_EX) // blocks: f1 holds
			}()
			time.Sleep(time.Millisecond) // let the waiter block
			if err := f2.Close(); err != nil {
				t.Fatalf("close f2 while its flock blocks: %v", err)
			}
			time.Sleep(time.Millisecond)
			select {
			case err := <-res:
				t.Fatalf("blocked flock returned early after the close: %v", err)
			default:
			}
			if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatalf("f1 unlock: %v", err)
			}
			if err := <-res; err != nil {
				t.Fatalf("flock woken after close-while-blocked = %v, want nil (Linux grants on the pinned description)", err)
			}
			// The phantom grant recorded nothing: a fresh nonblocking lock
			// succeeds at once.
			if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("fresh lock after the phantom grant = %v, want nil (the grant must record nothing)", err)
			}

			// Second cycle: the woken phantom waiter finds the lock ALREADY
			// retaken and must keep waiting (the wait-again path), granting
			// only at the next unlock. The retake happens in straight-line
			// code after the unlock — no yield point between the two calls
			// at P=1 — so the waiter deterministically loses the race.
			f3, err := os.OpenFile("/lock", os.O_RDWR, 0)
			if err != nil {
				t.Fatalf("open f3: %v", err)
			}
			fd3 := int(f3.Fd())
			res2 := make(chan error, 1)
			go func() {
				res2 <- syscall.Flock(fd3, syscall.LOCK_EX)
			}()
			time.Sleep(time.Millisecond)
			if err := f3.Close(); err != nil {
				t.Fatalf("close f3 while its flock blocks: %v", err)
			}
			time.Sleep(time.Millisecond)
			if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatalf("unlock before retake: %v", err)
			}
			if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("straight-line retake = %v, want nil", err)
			}
			time.Sleep(time.Millisecond) // the woken waiter re-checks, finds f1 holding, waits again
			select {
			case err := <-res2:
				t.Fatalf("phantom waiter granted %v while the lock was held (must wait again)", err)
			default:
			}
			if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatalf("final unlock: %v", err)
			}
			if err := <-res2; err != nil {
				t.Fatalf("phantom waiter after wait-again = %v, want nil", err)
			}
		})
	})
}

// TestDSTFSVirtualFDPwriteAppendHandle: Linux pwrite(2) (BUGS section)
// appends to a file opened O_APPEND regardless of the offset — the raw
// syscall surface must reproduce that shape (os.File.WriteAt never reaches
// it: the os layer refuses append handles).
func TestDSTFSVirtualFDPwriteAppendHandle(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/log", []byte("base"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/log", os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		if n, err := syscall.Pwrite(int(f.Fd()), []byte("XY"), 0); n != 2 || err != nil {
			t.Fatalf("Pwrite on O_APPEND = %d, %v; want 2, nil", n, err)
		}
		got, err := os.ReadFile("/log")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != "baseXY" {
			t.Fatalf("file after Pwrite(off=0) on O_APPEND = %q, want %q (Linux appends, ignoring the offset)", got, "baseXY")
		}
	})
}

// TestDSTProcOverlayFDIdentity: the spec's proc-fd identity contract is
// reachable and holds — Fd() on a proc-overlay file mints a virtual fd (it
// must not panic), fstat over it reports zero (st_dev, st_ino) (synthetic
// procfs identity: no SUT keys file identity on procfs stats), and reads
// through the fd work.
func TestDSTProcOverlayFDIdentity(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.Open("/proc/self/stat")
		if err != nil {
			t.Fatalf("open /proc/self/stat: %v", err)
		}
		defer f.Close()
		fd := int(f.Fd()) // must mint, not panic
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			t.Fatalf("Fstat(proc fd): %v", err)
		}
		if st.Dev != 0 || st.Ino != 0 {
			t.Fatalf("proc-overlay identity = (dev %d, ino %d), want (0, 0)", st.Dev, st.Ino)
		}
		buf := make([]byte, 8)
		if n, err := syscall.Read(fd, buf); n <= 0 || err != nil {
			t.Fatalf("read through the proc fd = %d, %v; want >0, nil", n, err)
		}
	})
}
