// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

// The simulated /dev/null conformance suite. Every assertion below is the
// host-probed shape of the real Linux null device (drivers/char/mem.c),
// checked in the same order a host probe observes it; see the conformance
// paragraph in docs/dst/design.md ("Deterministic pipes and the stdio
// stance").

// TestDSTDevNullOpenModes: every ordinary open mode succeeds, O_TRUNC is
// ignored on a character device, and O_CREAT|O_EXCL on the existing node is
// EEXIST.
func TestDSTDevNullOpenModes(t *testing.T) {
	simulation.Run(1, func() {
		for _, tc := range []struct {
			name string
			flag int
		}{
			{"O_RDONLY", os.O_RDONLY},
			{"O_WRONLY", os.O_WRONLY},
			{"O_RDWR", os.O_RDWR},
			{"O_WRONLY|O_TRUNC", os.O_WRONLY | os.O_TRUNC},
			{"O_RDONLY|O_TRUNC", os.O_RDONLY | os.O_TRUNC},
			{"O_WRONLY|O_APPEND", os.O_WRONLY | os.O_APPEND},
			{"O_RDWR|O_CREATE", os.O_RDWR | os.O_CREATE},
		} {
			f, err := os.OpenFile(os.DevNull, tc.flag, 0o666)
			if err != nil {
				t.Fatalf("open %s: %v", tc.name, err)
			}
			f.Close()
		}
		if _, err := os.OpenFile(os.DevNull, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666); !errors.Is(err, syscall.EEXIST) {
			t.Fatalf("O_CREATE|O_EXCL = %v, want EEXIST", err)
		}
		if _, err := os.OpenFile(os.DevNull+"/", os.O_RDONLY, 0); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("trailing slash = %v, want ENOTDIR", err)
		}
	})
}

// TestDSTDevNullReadWriteLadder: reads are EOF at every offset (behind the
// wrong-direction EBADF), writes discard and report the full count at every
// offset, and the file position is pinned at 0 whatever Seek asks — including
// a negative target that would be EINVAL on a regular file.
func TestDSTDevNullReadWriteLadder(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()

		buf := make([]byte, 8)
		if n, err := f.Read(buf); n != 0 || err != io.EOF {
			t.Fatalf("Read = %d, %v; want 0, io.EOF", n, err)
		}
		if n, err := f.ReadAt(buf, 5); n != 0 || err != io.EOF {
			t.Fatalf("ReadAt(5) = %d, %v; want 0, io.EOF", n, err)
		}
		if n, err := f.Read(nil); n != 0 || err != nil {
			t.Fatalf("Read(nil) = %d, %v; want 0, nil", n, err)
		}
		if n, err := f.Write([]byte("hello")); n != 5 || err != nil {
			t.Fatalf("Write = %d, %v; want 5, nil", n, err)
		}
		if n, err := f.Write(nil); n != 0 || err != nil {
			t.Fatalf("Write(nil) = %d, %v; want 0, nil", n, err)
		}
		if n, err := f.WriteAt([]byte("x"), 100); n != 1 || err != nil {
			t.Fatalf("WriteAt(100) = %d, %v; want 1, nil", n, err)
		}
		for _, sk := range []struct {
			off    int64
			whence int
		}{{100, io.SeekStart}, {7, io.SeekEnd}, {5, io.SeekCurrent}, {-1, io.SeekStart}} {
			if off, err := f.Seek(sk.off, sk.whence); off != 0 || err != nil {
				t.Fatalf("Seek(%d,%d) = %d, %v; want 0, nil", sk.off, sk.whence, off, err)
			}
		}
		// Position stays pinned: a read after a forward seek is still EOF.
		if n, err := f.Read(buf); n != 0 || err != io.EOF {
			t.Fatalf("Read after Seek = %d, %v; want 0, io.EOF", n, err)
		}

		// Wrong direction is EBADF, ahead of the device's EOF/discard.
		ro, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("open O_RDONLY: %v", err)
		}
		defer ro.Close()
		if _, err := ro.Write([]byte("q")); !errors.Is(err, syscall.EBADF) {
			t.Fatalf("Write on O_RDONLY = %v, want EBADF", err)
		}
		wo, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open O_WRONLY: %v", err)
		}
		defer wo.Close()
		if _, err := wo.Read(buf); !errors.Is(err, syscall.EBADF) {
			t.Fatalf("Read on O_WRONLY = %v, want EBADF", err)
		}
	})
}

// TestDSTDevNullMetadataLadder: the stat shape (character device, 0666, size
// 0), one inode for every name and handle, an mtime writes never move,
// truncate and sync EINVAL, ENOTDIR for the directory surface, and the
// non-pollable ErrNoDeadline shape.
func TestDSTDevNullMetadataLadder(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()

		fi, err := f.Stat()
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if fi.Mode()&os.ModeCharDevice == 0 || fi.Mode()&os.ModeDevice == 0 {
			t.Fatalf("mode = %v, want a character device", fi.Mode())
		}
		if fi.Mode().Perm() != 0o666 || fi.Size() != 0 || fi.IsDir() {
			t.Fatalf("mode/size = %v/%d, want crw-rw-rw-/0", fi.Mode(), fi.Size())
		}
		byName, err := os.Stat(os.DevNull)
		if err != nil {
			t.Fatalf("Stat by name: %v", err)
		}
		if !os.SameFile(fi, byName) {
			t.Fatalf("fstat and stat-by-name disagree on identity")
		}

		before := fi.ModTime()
		if _, err := f.Write([]byte("bump")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		after, err := f.Stat()
		if err != nil {
			t.Fatalf("Stat after write: %v", err)
		}
		if !after.ModTime().Equal(before) {
			t.Fatalf("write moved the device mtime: %v -> %v", before, after.ModTime())
		}

		if err := f.Truncate(0); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("Ftruncate = %v, want EINVAL", err)
		}
		if err := os.Truncate(os.DevNull, 5); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("Truncate by name = %v, want EINVAL", err)
		}
		if err := f.Sync(); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("Sync = %v, want EINVAL", err)
		}
		if _, err := f.Readdirnames(1); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("Readdirnames = %v, want ENOTDIR", err)
		}
		if err := f.Chdir(); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("Chdir = %v, want ENOTDIR", err)
		}
		if err := f.SetDeadline(time.Now().Add(time.Second)); !errors.Is(err, os.ErrNoDeadline) {
			t.Fatalf("SetDeadline = %v, want ErrNoDeadline", err)
		}

		// /dev itself: a plain 0755 directory listing the device.
		devInfo, err := os.Stat("/dev")
		if err != nil || !devInfo.IsDir() || devInfo.Mode().Perm() != 0o755 {
			t.Fatalf("/dev = %v, %v; want a 0755 directory", devInfo, err)
		}
		ents, err := os.ReadDir("/dev")
		if err != nil || len(ents) != 1 || ents[0].Name() != "null" {
			t.Fatalf("ReadDir /dev = %v, %v; want [null]", ents, err)
		}
	})
}

// TestDSTDevNullRawDescriptor: the raw virtual-fd surface — fstat reports
// S_IFCHR with the real device's (1, 3) rdev, read/write/seek keep the device
// shape, fsync keeps EINVAL, flock works (any file can carry an advisory
// lock), and mmap is ENODEV.
func TestDSTDevNullRawDescriptor(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		fd := int(f.Fd())

		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			t.Fatalf("Fstat: %v", err)
		}
		if st.Mode&syscall.S_IFMT != syscall.S_IFCHR {
			t.Fatalf("Fstat mode = %#o, want S_IFCHR", st.Mode)
		}
		if st.Rdev != 1<<8|3 {
			t.Fatalf("Fstat rdev = %#x, want (1, 3)", st.Rdev)
		}
		if st.Size != 0 {
			t.Fatalf("Fstat size = %d, want 0", st.Size)
		}
		if n, err := syscall.Read(fd, make([]byte, 4)); n != 0 || err != nil {
			t.Fatalf("raw read = %d, %v; want 0, nil", n, err)
		}
		if n, err := syscall.Write(fd, []byte("abc")); n != 3 || err != nil {
			t.Fatalf("raw write = %d, %v; want 3, nil", n, err)
		}
		if off, err := syscall.Seek(fd, 42, 0); off != 0 || err != nil {
			t.Fatalf("raw seek = %d, %v; want 0, nil", off, err)
		}
		if err := syscall.Fsync(fd); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("raw fsync = %v, want EINVAL", err)
		}
		if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
			t.Fatalf("flock = %v, want nil", err)
		}
		if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
			t.Fatalf("funlock = %v, want nil", err)
		}
		if _, err := syscall.Mmap(fd, 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED); !errors.Is(err, syscall.ENODEV) {
			t.Fatalf("mmap = %v, want ENODEV", err)
		}
	})
}

// TestDSTDevNullDiskFaultDecoupled: the device is not on the disk — a full
// disk, a failed disk, and a per-file EIO aimed at the device's own path all
// leave it reading EOF and discarding writes, exactly as on a real machine
// whose disk died.
func TestDSTDevNullDiskFaultDecoupled(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.LimitDisk("h", 0)
			simulation.FailDisk("h")
			simulation.FailFile("h", os.DevNull)
			f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
			if err != nil {
				t.Fatalf("open under full+failed disk: %v", err)
			}
			defer f.Close()
			if n, err := f.Write([]byte("payload")); n != 7 || err != nil {
				t.Fatalf("Write under full+failed disk = %d, %v; want 7, nil", n, err)
			}
			if n, err := f.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("Read under failed disk = %d, %v; want 0, io.EOF", n, err)
			}
			// The disk faults still hold for a regular file on the same host.
			if _, err := os.OpenFile("/spill", os.O_RDWR|os.O_CREATE, 0o644); !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("regular create on full disk = %v, want ENOSPC", err)
			}
		})
	})
}

// TestDSTDevNullSurvivesHostCrash: /dev/null is part of the mkfs image — a
// host crash (and a torn one) reboots with the device intact, so a
// post-recovery logger's /dev/null open cannot fail with an ENOENT no real
// reboot produces.
func TestDSTDevNullSurvivesHostCrash(t *testing.T) {
	for _, tear := range []bool{false, true} {
		t.Run(map[bool]string{false: "untorn", true: "torn"}[tear], func(t *testing.T) {
			simulation.RunWith(1, simulation.Options{CrashTear: tear}, func() {
				simulation.Host("h", simulation.HostConfig{}, func() {
					f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
					if err != nil {
						t.Fatalf("pre-crash open: %v", err)
					}
					f.Write([]byte("pre-crash"))
					f.Close()
				})
				simulation.CrashHost("h")
				simulation.Host("h", simulation.HostConfig{}, func() {
					f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
					if err != nil {
						t.Fatalf("post-reboot open: %v", err)
					}
					defer f.Close()
					if n, err := f.Write([]byte("post-crash")); n != 10 || err != nil {
						t.Fatalf("post-reboot write = %d, %v", n, err)
					}
					fi, err := f.Stat()
					if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
						t.Fatalf("post-reboot stat = %v, %v; want a character device", fi, err)
					}
				})
			})
		})
	}
}

// TestDSTDevNullUnlinkAndRecreate: removing the device is an ordinary
// namespace operation; a subsequent O_CREAT mints a plain REGULAR file at the
// path (as on a real system after rm /dev/null), while a handle opened before
// the removal keeps the device semantics.
func TestDSTDevNullUnlinkAndRecreate(t *testing.T) {
	simulation.Run(1, func() {
		held, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer held.Close()
		if err := os.Remove(os.DevNull); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if _, err := os.Stat(os.DevNull); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Stat after Remove = %v, want ErrNotExist", err)
		}
		// The held handle keeps discarding.
		if n, err := held.Write([]byte("still")); n != 5 || err != nil {
			t.Fatalf("held Write = %d, %v", n, err)
		}
		// Recreation mints a regular file, not a device.
		f, err := os.OpenFile(os.DevNull, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatalf("recreate: %v", err)
		}
		defer f.Close()
		if n, err := f.Write([]byte("kept")); n != 4 || err != nil {
			t.Fatalf("regular write = %d, %v", n, err)
		}
		fi, err := f.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice != 0 || fi.Size() != 4 {
			t.Fatalf("recreated file = %v (size %d), %v; want a 4-byte regular file", fi.Mode(), fi.Size(), err)
		}
	})
}

// TestDSTDevNullPerHost: each host's /dev/null is its own node — a chmod on
// one host's device is invisible on another (DST-NODE-ISOLATION), like every
// other per-host tree fact.
func TestDSTDevNullPerHost(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("hA", simulation.HostConfig{}, func() {
			if err := os.Chmod(os.DevNull, 0o600); err != nil {
				t.Fatalf("chmod on hA: %v", err)
			}
		})
		simulation.Host("hB", simulation.HostConfig{}, func() {
			fi, err := os.Stat(os.DevNull)
			if err != nil {
				t.Fatalf("stat on hB: %v", err)
			}
			if fi.Mode().Perm() != 0o666 {
				t.Fatalf("hB device perm = %v, want 0666 (hA's chmod leaked)", fi.Mode().Perm())
			}
			if fi.Mode()&os.ModeCharDevice == 0 {
				t.Fatalf("hB device mode = %v, want a character device", fi.Mode())
			}
		})
	})
}
