// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/simulation"
	"testing/synctest"
	"time"
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

func TestDSTFSVirtualFDRawSelectedSyscallsFenced(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func(fd uintptr)
	}{
		{"Syscall_Fsync", func(fd uintptr) { syscall.Syscall(syscall.SYS_FSYNC, fd, 0, 0) }},
		{"RawSyscall_Fdatasync", func(fd uintptr) { syscall.RawSyscall(syscall.SYS_FDATASYNC, fd, 0, 0) }},
		{"Syscall6_Mmap", func(fd uintptr) {
			syscall.Syscall6(syscall.SYS_MMAP, 0, uintptr(syscall.Getpagesize()), syscall.PROT_READ, syscall.MAP_SHARED, fd, 0)
		}},
	} {
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

func TestDSTFSVirtualFDMmapCrossProcessWriteUpdates(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/m", []byte("hello"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			mapped := make(chan struct{})
			written := make(chan struct{})
			done := make(chan error, 1)
			simulation.Process("p1", func() {
				go func() {
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
				}()
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
			if err := syscall.Fdatasync(fd); err != nil {
				t.Fatalf("Fdatasync after HealDisk: %v", err)
			}
			if _, synced, _, _, _, _, ok := os.DSTFSNodeState("/sync-eio"); !ok || synced != "dirty" {
				t.Fatalf("synced image after heal = %q, ok=%v; want dirty", synced, ok)
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
			simulation.Process("p1", func() {
				go func() {
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
				}()
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
			done := make(chan error, 1)
			var nbErr error
			simulation.Process("p2", func() {
				f, err := os.OpenFile("/lock", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("p2 OpenFile: %v", err)
				}
				nbErr = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				go func() {
					close(attempting)
					done <- syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
					f.Close()
				}()
			})
			if !errors.Is(nbErr, syscall.EWOULDBLOCK) {
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
			simulation.Process("p1", func() {
				go func() {
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
				}()
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
			simulation.Process("p1", func() {
				go func() {
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
				}()
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
			simulation.Process("p2", func() {
				f, err := os.Open("/lock")
				if err != nil {
					t.Fatalf("p2 Open: %v", err)
				}
				go func() {
					close(attempting)
					err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
					if closeErr := f.Close(); err == nil {
						err = closeErr
					}
					done <- err
				}()
			})
			<-attempting
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
				simulation.Process(name, func() {
					go func() {
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
					}()
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
			simulation.Process("p1", func() {
				go func() {
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
				}()
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
			simulation.Process("p1", func() {
				go func() {
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
				}()
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

func TestDSTFSVirtualFDFlockRawSyscallFenced(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func(fd uintptr)
	}{
		{"Syscall", func(fd uintptr) { syscall.Syscall(syscall.SYS_FLOCK, fd, syscall.LOCK_EX, 0) }},
		{"RawSyscall", func(fd uintptr) { syscall.RawSyscall(syscall.SYS_FLOCK, fd, syscall.LOCK_EX, 0) }},
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
