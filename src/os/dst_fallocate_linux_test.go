// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"bytes"
	"errors"
	"math"
	"os"
	"runtime"
	"syscall"
	"testing"
	"testing/simulation"
)

// fallocate(2) mode-0 dispatch tests — the preallocation call WAL-shaped
// stores reach through unix.Fallocate. syscall.Fallocate and unix.Fallocate
// enter the same generic trampolines, so driving syscall.Fallocate on a
// virtual fd exercises the raw-boundary dispatch itself (dstRawFallocate),
// not a separate named path. The contract under test: mode 0 grows the file
// to offset+len with zeros (allocation IS size in the logical-bytes model),
// checked all-or-nothing against the disk capacity; a span inside the current
// size is a no-op; every other mode answers EOPNOTSUPP; the vfs gate order
// (EBADF for a read-only fd, ENODEV for a device) holds; and the growth is a
// mutation — durable only after a sync.

// falloc drives the dispatch through the real trampoline path.
func falloc(f *os.File, mode uint32, off, length int64) error {
	return syscall.Fallocate(int(f.Fd()), mode, off, length)
}

func wantErrno(t *testing.T, what string, err error, want syscall.Errno) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: error %v, want %v", what, err, want)
	}
}

// TestDSTFallocateGrow: preallocation grows the file to offset+len, reads
// return zeros in the preallocated span, existing content is untouched, and
// writes land into the span without further growth.
func TestDSTFallocateGrow(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			f, err := os.Create("/f")
			mustOK(t, "create", err)
			defer f.Close()
			_, err = f.Write([]byte("abc"))
			mustOK(t, "write", err)
			mustOK(t, "fallocate", falloc(f, 0, 0, 64<<10))
			st, err := f.Stat()
			mustOK(t, "stat", err)
			if st.Size() != 64<<10 {
				t.Fatalf("size after fallocate = %d, want %d", st.Size(), 64<<10)
			}
			b, err := os.ReadFile("/f")
			mustOK(t, "read", err)
			if !bytes.Equal(b[:3], []byte("abc")) {
				t.Fatalf("prefix clobbered: %q", b[:3])
			}
			for i, c := range b[3:] {
				if c != 0 {
					t.Fatalf("preallocated byte %d = %d, want 0", i+3, c)
				}
			}
			// A span already inside the size is a no-op.
			mustOK(t, "fallocate within size", falloc(f, 0, 8, 16))
			st, err = f.Stat()
			mustOK(t, "stat", err)
			if st.Size() != 64<<10 {
				t.Fatalf("size after in-range fallocate = %d, want unchanged %d", st.Size(), 64<<10)
			}
			// Writing into the span works and does not grow further.
			_, err = f.WriteAt([]byte("XYZ"), 32<<10)
			mustOK(t, "write into span", err)
			st, err = f.Stat()
			mustOK(t, "stat", err)
			if st.Size() != 64<<10 {
				t.Fatalf("size after in-span write = %d, want %d", st.Size(), 64<<10)
			}
			// Offset+len compose (a non-zero offset grows to the span's end).
			mustOK(t, "fallocate at offset", falloc(f, 0, 64<<10, 4<<10))
			st, err = f.Stat()
			mustOK(t, "stat", err)
			if st.Size() != 68<<10 {
				t.Fatalf("size after offset fallocate = %d, want %d", st.Size(), 68<<10)
			}
		})
	})
}

// TestDSTFallocateArgGates: vfs_fallocate's gate ORDER, not just its errnos —
// arguments (EINVAL) beat mode (EOPNOTSUPP) beat write access (EBADF), so a
// degradation ladder pairing a probe mode with a degenerate length sees the
// errno it was written against; then EFBIG for a span past the maximum file
// size, and EBADF for a proc-overlay fd (read-only — FMODE_WRITE loses before
// procfs's missing ->fallocate could answer).
func TestDSTFallocateArgGates(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "seed", os.WriteFile("/f", []byte("abc"), 0o644))
			ro, err := os.Open("/f")
			mustOK(t, "open ro", err)
			defer ro.Close()
			wantErrno(t, "read-only fd", falloc(ro, 0, 0, 4096), syscall.EBADF)
			wantErrno(t, "read-only fd, zero length (EINVAL wins)", falloc(ro, 0, 0, 0), syscall.EINVAL)
			// In-mask modes reach the access gate first (host-probed: the
			// vfs's early mode gate covers only out-of-mask bits).
			wantErrno(t, "read-only fd, KEEP_SIZE (EBADF wins)", falloc(ro, 0x1, 0, 4096), syscall.EBADF)
			wantErrno(t, "read-only fd, out-of-mask mode (EOPNOTSUPP wins)", falloc(ro, 0x1000, 0, 4096), syscall.EOPNOTSUPP)

			f, err := os.OpenFile("/f", os.O_WRONLY, 0)
			mustOK(t, "open rw", err)
			defer f.Close()
			const fallocFlKeepSize = 0x1
			wantErrno(t, "KEEP_SIZE", falloc(f, fallocFlKeepSize, 0, 4096), syscall.EOPNOTSUPP)
			wantErrno(t, "UNSHARE (in-mask, unimplemented)", falloc(f, 0x40, 0, 4096), syscall.EOPNOTSUPP)
			wantErrno(t, "out-of-mask mode", falloc(f, 0x1000, 0, 4096), syscall.EOPNOTSUPP)
			wantErrno(t, "zero length", falloc(f, 0, 0, 0), syscall.EINVAL)
			wantErrno(t, "negative length", falloc(f, 0, 0, -1), syscall.EINVAL)
			wantErrno(t, "negative offset", falloc(f, 0, -1, 4096), syscall.EINVAL)
			wantErrno(t, "bad mode, negative offset (EINVAL wins)", falloc(f, 0x1, -1, 4096), syscall.EINVAL)
			wantErrno(t, "overflowing span", falloc(f, 0, 1<<62, 1<<62), syscall.EFBIG)
			st, err := f.Stat()
			mustOK(t, "stat", err)
			if st.Size() != 3 {
				t.Fatalf("size after refused calls = %d, want 3 (a refusal allocates nothing)", st.Size())
			}

			// A proc-overlay fd: read-only, so EBADF — after the argument
			// gates, as everywhere.
			ps, err := os.Open("/proc/self/stat")
			mustOK(t, "open proc", err)
			defer ps.Close()
			wantErrno(t, "proc fd", falloc(ps, 0, 0, 4096), syscall.EBADF)
			wantErrno(t, "proc fd, zero length", falloc(ps, 0, 0, 0), syscall.EINVAL)
			wantErrno(t, "proc fd, KEEP_SIZE (EBADF wins)", falloc(ps, 0x1, 0, 4096), syscall.EBADF)
			wantErrno(t, "proc fd, out-of-mask mode", falloc(ps, 0x1000, 0, 4096), syscall.EOPNOTSUPP)
		})
	})
}

// TestDSTFallocateENOSPC: the reason the call exists — preallocation fails
// ENOSPC up front on a capped disk, all-or-nothing, and frees make room.
func TestDSTFallocateENOSPC(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			f, err := os.Create("/f")
			mustOK(t, "create", err)
			defer f.Close()
			mustOK(t, "seed sibling", os.WriteFile("/g", []byte("x"), 0o644))
			simulation.LimitDisk("h", (8<<10)+1)
			mustOK(t, "fallocate under cap", falloc(f, 0, 0, 4<<10))
			wantErrno(t, "fallocate past cap", falloc(f, 0, 0, 16<<10), syscall.ENOSPC)
			// The near-MaxInt64 span: refused up front, never an overflowed
			// accounting sum reaching the grow path (a wrapped sum once
			// turned this into a runtime fatal — review-caught, this arm is
			// the reproducer). Since s_maxbytes landed the refusal is EFBIG:
			// the vfs checks the span against it before the filesystem op,
			// so EFBIG wins over the capacity's ENOSPC.
			wantErrno(t, "huge span on capped disk", falloc(f, 0, 0, math.MaxInt64), syscall.EFBIG)
			st, err := f.Stat()
			mustOK(t, "stat", err)
			if st.Size() != 4<<10 {
				t.Fatalf("size after refused fallocate = %d, want %d (all-or-nothing)", st.Size(), 4<<10)
			}
			// Exactly to capacity succeeds — the boundary is >, matching the
			// write path's room accounting.
			mustOK(t, "fallocate exactly to capacity", falloc(f, 0, 0, 8<<10))
			simulation.UnlimitDisk("h")
			mustOK(t, "fallocate after unlimit", falloc(f, 0, 0, 16<<10))
		})
	})
}

// TestDSTFallocateEIO: a failing disk fails preallocation as it fails writes.
func TestDSTFallocateEIO(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			f, err := os.Create("/f")
			mustOK(t, "create", err)
			defer f.Close()
			simulation.FailDisk("h")
			wantErrno(t, "fallocate under EIO", falloc(f, 0, 0, 4<<10), syscall.EIO)
			simulation.HealDisk("h")
			mustOK(t, "fallocate after heal", falloc(f, 0, 0, 4<<10))
		})
	})
}

// TestDSTFallocateDurability: preallocated growth is a mutation — synced, it
// survives a host crash (zeros where nothing was written, written bytes where
// they landed); unsynced, the crash discards it like any unsynced growth.
func TestDSTFallocateDurability(t *testing.T) {
	const page = 4096
	var synced, unsynced []byte
	var syncedOK, unsyncedOK bool
	simulation.RunWith(3, simulation.Options{}, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			go simulation.Process("db", func() {
				f, err := os.Create("/s")
				mustOK(t, "create /s", err)
				mustOK(t, "fallocate /s", falloc(f, 0, 0, 2*page))
				_, err = f.WriteAt(bytes.Repeat([]byte("W"), page), 0)
				mustOK(t, "write /s", err)
				mustOK(t, "sync /s", f.Sync())

				g, err := os.Create("/u")
				mustOK(t, "create /u", err)
				mustOK(t, "fallocate /u", falloc(g, 0, 0, 2*page))
				d, err := os.Open("/")
				mustOK(t, "open dir", err)
				mustOK(t, "sync dir", d.Sync())
				mustOK(t, "close dir", d.Close())
				select {}
			})
			for range 30 {
				runtime.Gosched()
			}
		})

		simulation.CrashHost("h")

		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("recover", func() {
				var err error
				synced, err = os.ReadFile("/s")
				syncedOK = err == nil
				unsynced, err = os.ReadFile("/u")
				unsyncedOK = err == nil
			})
		})
	})
	if !syncedOK {
		t.Fatal("synced preallocated file unreadable after reboot")
	}
	if len(synced) != 2*page {
		t.Fatalf("synced file = %d bytes after reboot, want %d (synced growth is durable)", len(synced), 2*page)
	}
	if !bytes.Equal(synced[:page], bytes.Repeat([]byte("W"), page)) {
		t.Fatal("written page lost from synced preallocated file")
	}
	for i, c := range synced[page:] {
		if c != 0 {
			t.Fatalf("synced preallocated byte %d = %d, want 0", page+i, c)
		}
	}
	if !unsyncedOK {
		t.Fatal("unsynced file's NAME was durable (syncDir) — the file must exist after reboot")
	}
	if len(unsynced) != 0 {
		t.Fatalf("unsynced preallocation = %d bytes after reboot, want 0 (unsynced growth is discarded)", len(unsynced))
	}
}
