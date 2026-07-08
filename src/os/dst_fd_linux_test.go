// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
)

const (
	dstTestMadvCold         = 20
	dstTestMadvPopulateRead = 22
)

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
		if err := syscall.Mprotect(b, syscall.PROT_READ|syscall.PROT_WRITE); !errors.Is(err, syscall.EACCES) {
			t.Fatalf("Mprotect(PROT_READ|PROT_WRITE) = %v, want EACCES", err)
		}
		if err := syscall.Mprotect(b, syscall.PROT_READ); err != nil {
			t.Fatalf("Mprotect(PROT_READ): %v", err)
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
		if _, err := syscall.Mmap(fd, 0, 6, syscall.PROT_READ, syscall.MAP_SHARED); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("past-EOF Mmap = %v, want EINVAL", err)
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
