// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package syscall_test

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
	"unsafe"
)

func TestDSTRawEmptyMappingOperations(t *testing.T) {
	pageSize := syscall.Getpagesize()
	host, err := syscall.Mmap(-1, 0, pageSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(host)

	simulation.Run(1, func() {
		var mapped []byte
		if unsafe.Sizeof(uintptr(0)) == 8 {
			f, err := os.OpenFile("/empty-mapping-operations", os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			defer f.Close()
			if _, err := f.Write(make([]byte, pageSize)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			mapped, err = syscall.Mmap(int(f.Fd()), 0, pageSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
			if err != nil {
				t.Fatalf("Mmap: %v", err)
			}
			defer syscall.Munmap(mapped)
			for _, call := range []struct {
				name string
				fn   func() (uintptr, uintptr, syscall.Errno)
			}{
				{name: "Syscall", fn: func() (uintptr, uintptr, syscall.Errno) {
					return syscall.Syscall(syscall.SYS_MPROTECT, uintptr(unsafe.Pointer(&mapped[0])), uintptr(len(mapped)), syscall.PROT_READ|syscall.PROT_WRITE)
				}},
				{name: "Syscall6", fn: func() (uintptr, uintptr, syscall.Errno) {
					return syscall.Syscall6(syscall.SYS_MPROTECT, uintptr(unsafe.Pointer(&mapped[0])), uintptr(len(mapped)), syscall.PROT_READ|syscall.PROT_WRITE, 0, 0, 0)
				}},
			} {
				r1, r2, errno := call.fn()
				if r1 != 0 || r2 != 0 || errno != 0 {
					t.Errorf("successful %s mprotect = (%#x, %#x, %v), want (0, 0, 0)", call.name, r1, r2, errno)
				}
			}
		}

		type address struct {
			name  string
			empty []byte
			ptr   uintptr
		}
		addresses := []address{
			{name: "nil"},
			{name: "host-aligned", empty: host[:0], ptr: uintptr(unsafe.Pointer(&host[0]))},
			{name: "host-unaligned", empty: host[1:1], ptr: uintptr(unsafe.Pointer(&host[1]))},
		}
		if mapped != nil {
			addresses = append(addresses,
				address{name: "mapping-aligned", empty: mapped[:0], ptr: uintptr(unsafe.Pointer(&mapped[0]))},
				address{name: "mapping-unaligned", empty: mapped[1:1], ptr: uintptr(unsafe.Pointer(&mapped[1]))},
			)
		}

		operations := []struct {
			name  string
			trap  uintptr
			arg   uintptr
			named func([]byte) error
		}{
			{name: "madvise", trap: syscall.SYS_MADVISE, arg: syscall.MADV_HUGEPAGE, named: func(b []byte) error { return syscall.Madvise(b, syscall.MADV_HUGEPAGE) }},
			{name: "mprotect", trap: syscall.SYS_MPROTECT, arg: syscall.PROT_READ, named: func(b []byte) error { return syscall.Mprotect(b, syscall.PROT_READ) }},
			{name: "munmap", trap: syscall.SYS_MUNMAP, named: syscall.Munmap},
		}

		for _, addr := range addresses {
			for _, op := range operations {
				t.Run(addr.name+"/"+op.name, func(t *testing.T) {
					if err := op.named(addr.empty); !errors.Is(err, syscall.EINVAL) {
						t.Errorf("named operation error = %v, want EINVAL", err)
					}
					for _, call := range []struct {
						name string
						fn   func() (uintptr, uintptr, syscall.Errno)
					}{
						{name: "Syscall", fn: func() (uintptr, uintptr, syscall.Errno) { return syscall.Syscall(op.trap, addr.ptr, 0, op.arg) }},
						{name: "Syscall6", fn: func() (uintptr, uintptr, syscall.Errno) {
							return syscall.Syscall6(op.trap, addr.ptr, 0, op.arg, 0, 0, 0)
						}},
					} {
						r1, r2, errno := call.fn()
						if r1 != ^uintptr(0) || r2 != 0 || errno != syscall.EINVAL {
							t.Errorf("%s = (%#x, %#x, %v), want (%#x, 0, EINVAL)", call.name, r1, r2, errno, ^uintptr(0))
						}
					}
				})
			}
		}
	})
}
