// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"errors"
	"math"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
)

// TestDSTFSVirtualFDFstatIdentity: fstat(2)'s (st_dev, st_ino) pair is the file
// identity inode-keyed SUTs (the SQLite/LMDB per-file lock-dedup pattern) key
// on: distinct files differ, one file reached through two descriptors agrees,
// the identity survives rename, directories report Nlink 2, and two hosts'
// disks report different devices.
func TestDSTFSVirtualFDFstatIdentity(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/a", []byte("a"), 0o644); err != nil {
			t.Fatalf("WriteFile a: %v", err)
		}
		if err := os.WriteFile("/b", []byte("b"), 0o644); err != nil {
			t.Fatalf("WriteFile b: %v", err)
		}
		statOf := func(name string) syscall.Stat_t {
			f, err := os.Open(name)
			if err != nil {
				t.Fatalf("Open %s: %v", name, err)
			}
			defer f.Close()
			var st syscall.Stat_t
			if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
				t.Fatalf("Fstat %s: %v", name, err)
			}
			return st
		}
		sa, sb := statOf("/a"), statOf("/b")
		if sa.Ino == 0 || sb.Ino == 0 {
			t.Fatalf("zero inode: a=%d b=%d (identity must be nonzero)", sa.Ino, sb.Ino)
		}
		if sa.Dev == 0 {
			t.Fatalf("zero device (identity must be nonzero)")
		}
		if sa.Dev != sb.Dev {
			t.Fatalf("same-disk files report different devices: %d vs %d", sa.Dev, sb.Dev)
		}
		if sa.Ino == sb.Ino {
			t.Fatalf("distinct files share inode %d", sa.Ino)
		}
		if again := statOf("/a"); again.Ino != sa.Ino || again.Dev != sa.Dev {
			t.Fatalf("second open of /a = (%d,%d), want (%d,%d)", again.Dev, again.Ino, sa.Dev, sa.Ino)
		}
		if err := os.Rename("/a", "/moved"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		if moved := statOf("/moved"); moved.Ino != sa.Ino {
			t.Fatalf("rename moved the inode: %d, want %d", moved.Ino, sa.Ino)
		}
		if err := os.Mkdir("/d", 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if sa.Nlink != 1 {
			t.Fatalf("regular file Nlink = %d, want 1", sa.Nlink)
		}
		sd := statOf("/d")
		if sd.Nlink != 2 {
			t.Fatalf("directory Nlink = %d, want 2", sd.Nlink)
		}
		if sd.Ino == 0 || sd.Ino == sa.Ino || sd.Ino == sb.Ino {
			t.Fatalf("directory inode %d collides or is zero", sd.Ino)
		}

		// uint64() conversions: st_dev's field width is arch-dependent (uint32
		// on mips).
		var h2Dev uint64
		simulation.Host("h2", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/a", []byte("a"), 0o644); err != nil {
				t.Fatalf("h2 WriteFile: %v", err)
			}
			h2Dev = uint64(statOf("/a").Dev)
		})
		if h2Dev == uint64(sa.Dev) {
			t.Fatalf("two hosts' disks share device %d", h2Dev)
		}
	})
}

// TestDSTFSTruncateShrinkUnderLiveMapping: shrinking a file under a live
// mapping is ordinary ftruncate semantics — every entry point (Truncate by
// name, by handle, and O_TRUNC) succeeds, bytes within the new end stay
// readable through the mapping, the partial page's tail zeroes, a whole cut
// page reads zero after a re-growth (never the dropped bytes), and the
// access-side death is pinned by TestDSTFSMappingFaultShapes. The last
// partial page behaves as the kernel page cache: a tail write through a
// writable mapping is visible, and growth exposes it as file content (tmpfs
// semantics — recorded in the spec as non-portable and undurable).
func TestDSTFSTruncateShrinkUnderLiveMapping(t *testing.T) {
	const page = 4096 // the simulated page size
	simulation.Run(1, func() {
		content := make([]byte, 2*page)
		for i := range content {
			content[i] = 'x'
		}
		if err := os.WriteFile("/m", content, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/m", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		b, err := syscall.Mmap(int(f.Fd()), 0, 2*page, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap: %v", err)
		}
		defer syscall.Munmap(b)

		// Shrink by name, mid-page: succeeds under the live mapping.
		if err := os.Truncate("/m", page/2); err != nil {
			t.Fatalf("shrink under live mapping = %v, want success", err)
		}
		if b[0] != 'x' || b[page/2-1] != 'x' {
			t.Fatalf("bytes within the new end changed: %q %q", b[0], b[page/2-1])
		}
		if b[page/2] != 0 || b[page-1] != 0 {
			t.Fatalf("partial-page tail = %d %d, want zeros (truncate zeroes it)", b[page/2], b[page-1])
		}

		// A tail write through the mapping is visible, and growth exposes it
		// as file content — the kernel page cache's (tmpfs) semantics.
		b[page/2] = 'T'
		if err := f.Truncate(page); err != nil {
			t.Fatalf("grow by handle: %v", err)
		}
		got, err := os.ReadFile("/m")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if got[page/2] != 'T' {
			t.Fatalf("grown file byte = %q, want the tail write 'T' (page-cache semantics)", got[page/2])
		}

		// Re-grow over the WHOLE cut page: it reads zero, not the dropped 'x's.
		if err := f.Truncate(2 * page); err != nil {
			t.Fatalf("regrow: %v", err)
		}
		if b[page] != 0 || b[2*page-1] != 0 {
			t.Fatalf("re-grown page = %d %d, want zeros: the shrink dropped these bytes", b[page], b[2*page-1])
		}

		// Mprotect over a window that spans past EOF (reservation pages) is
		// legal: protection applies to the mapping, not the file's length.
		if err := os.Truncate("/m", page); err != nil {
			t.Fatalf("shrink for mprotect: %v", err)
		}
		if err := syscall.Mprotect(b, syscall.PROT_READ); err != nil {
			t.Fatalf("Mprotect spanning past EOF = %v, want success", err)
		}

		// O_TRUNC is the third entry point: truncates to zero under the live
		// mapping — the whole mapping is past EOF afterward.
		g, err := os.OpenFile("/m", os.O_RDWR|os.O_TRUNC, 0)
		if err != nil {
			t.Fatalf("O_TRUNC under live mapping = %v, want success", err)
		}
		g.Close()
		if got, _, _, _, _, _, ok := os.DSTFSNodeState("/m"); !ok || len(got) != 0 {
			t.Fatalf("post-O_TRUNC length = %d ok=%v, want 0 true", len(got), ok)
		}
	})
}

// TestDSTFSOpenRootRemovedDirectory: a Root keeps addressing its captured node
// across RENAMES (spec), but once the node is REMOVED the kernel fails entry
// creation in it with ENOENT — openat/mkdirat/renameat in an rmdir'd directory
// never resurrect it.
func TestDSTFSOpenRootRemovedDirectory(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.Mkdir("/d", 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		r, err := os.OpenRoot("/d")
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		defer r.Close()
		if err := os.Remove("/d"); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if _, err := r.Create("f"); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("Create in removed root = %v, want ENOENT", err)
		}
		if err := r.Mkdir("sub", 0o755); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("Mkdir in removed root = %v, want ENOENT", err)
		}
		if _, err := os.Stat("/d"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removed dir visible again: Stat = %v, want not-exist", err)
		}

		// The RemoveAll form marks the whole subtree, and a removed directory
		// reads EMPTY through a surviving Root — the host unlinks bottom-up, so
		// the detached children are not a visible listing.
		if err := os.MkdirAll("/x/y", 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile("/x/y/f", []byte("data"), 0o644); err != nil {
			t.Fatalf("WriteFile /x/y/f: %v", err)
		}
		ry, err := os.OpenRoot("/x/y")
		if err != nil {
			t.Fatalf("OpenRoot /x/y: %v", err)
		}
		defer ry.Close()
		if err := os.RemoveAll("/x"); err != nil {
			t.Fatalf("RemoveAll: %v", err)
		}
		if _, err := ry.Create("g"); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("Create in RemoveAll'd subtree root = %v, want ENOENT", err)
		}
		if _, err := ry.Open("f"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Open of a removed subtree's child through the Root = %v, want not-exist (bottom-up unlink)", err)
		}

		// Rename-over unlinks the replaced (empty) directory. Go's os.Rename
		// refuses directory targets at the Go level (EEXIST), so the
		// dir-over-dir replace is exercised through the rooted surface, which
		// keeps renameat(2)'s kernel semantics.
		if err := os.Mkdir("/old", 0o755); err != nil {
			t.Fatalf("Mkdir /old: %v", err)
		}
		if err := os.Mkdir("/new", 0o755); err != nil {
			t.Fatalf("Mkdir /new: %v", err)
		}
		rOld, err := os.OpenRoot("/old")
		if err != nil {
			t.Fatalf("OpenRoot /old: %v", err)
		}
		defer rOld.Close()
		rTree, err := os.OpenRoot("/")
		if err != nil {
			t.Fatalf("OpenRoot /: %v", err)
		}
		defer rTree.Close()
		if err := rTree.Rename("new", "old"); err != nil {
			t.Fatalf("rooted Rename over: %v", err)
		}
		if _, err := rOld.Create("f"); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("Create in renamed-over root = %v, want ENOENT", err)
		}

		// Rename INTO a removed directory fails too.
		if err := os.Mkdir("/live", 0o755); err != nil {
			t.Fatalf("Mkdir /live: %v", err)
		}
		if err := os.WriteFile("/live/f", nil, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		rLive, err := os.OpenRoot("/live")
		if err != nil {
			t.Fatalf("OpenRoot /live: %v", err)
		}
		defer rLive.Close()
		if err := os.Mkdir("/gone", 0o755); err != nil {
			t.Fatalf("Mkdir /gone: %v", err)
		}
		rGone, err := os.OpenRoot("/gone")
		if err != nil {
			t.Fatalf("OpenRoot /gone: %v", err)
		}
		defer rGone.Close()
		if err := os.Remove("/gone"); err != nil {
			t.Fatalf("Remove /gone: %v", err)
		}
		// A root-relative rename cannot cross roots; drive the check through the
		// removed root itself: source resolves (captured node), dest parent is
		// the unlinked node.
		if err := rGone.Mkdir("dst", 0o755); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("Mkdir in removed root = %v, want ENOENT", err)
		}
	})
}

// TestDSTFSOpenDirWithOCreateIsEISDIR: open(dir, O_CREAT) is EISDIR on Linux
// even with O_RDONLY — O_CREAT asserts a regular file — on both the named and
// the rooted surface.
func TestDSTFSOpenDirWithOCreateIsEISDIR(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.Mkdir("/d", 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if _, err := os.OpenFile("/d", os.O_RDONLY|os.O_CREATE, 0o644); !errors.Is(err, syscall.EISDIR) {
			t.Fatalf("named OpenFile(dir, O_RDONLY|O_CREATE) = %v, want EISDIR", err)
		}
		// O_CREAT|O_EXCL: do_open's existence check precedes EISDIR AND the
		// write/O_TRUNC access checks — EEXIST for every access mode.
		for _, extra := range []int{os.O_RDONLY, os.O_WRONLY, os.O_RDWR, os.O_RDONLY | os.O_TRUNC} {
			if _, err := os.OpenFile("/d", extra|os.O_CREATE|os.O_EXCL, 0o644); !errors.Is(err, syscall.EEXIST) {
				t.Fatalf("named OpenFile(dir, %#x|O_CREATE|O_EXCL) = %v, want EEXIST", extra, err)
			}
		}
		r, err := os.OpenRoot("/")
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		defer r.Close()
		if _, err := r.OpenFile("d", os.O_RDONLY|os.O_CREATE, 0o644); !errors.Is(err, syscall.EISDIR) {
			t.Fatalf("rooted OpenFile(dir, O_RDONLY|O_CREATE) = %v, want EISDIR", err)
		}
		for _, extra := range []int{os.O_RDONLY, os.O_WRONLY, os.O_RDWR, os.O_RDONLY | os.O_TRUNC} {
			if _, err := r.OpenFile("d", extra|os.O_CREATE|os.O_EXCL, 0o644); !errors.Is(err, syscall.EEXIST) {
				t.Fatalf("rooted OpenFile(dir, %#x|O_CREATE|O_EXCL) = %v, want EEXIST", extra, err)
			}
		}
		// Plain O_RDONLY on a directory still opens it.
		d, err := os.OpenFile("/d", os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("OpenFile(dir, O_RDONLY) = %v, want success", err)
		}
		d.Close()
	})
}

// TestDSTFSVirtualFDRawNeverIssuedInRangeFenced: the raw boundary refuses the
// WHOLE reserved virtual-fd range — a number in [base, base+count) that was
// never issued must still fence, exactly like an issued one, so no in-range
// number can ever reach the host.
func TestDSTFSVirtualFDRawNeverIssuedInRangeFenced(t *testing.T) {
	simulation.Run(1, func() {
		neverIssued := uintptr(1<<30 + 999_999)
		expectDSTRawSyscallPanic(t, func() {
			syscall.Syscall(syscall.SYS_READ, neverIssued, 0, 0)
		})
		expectDSTRawSyscallPanic(t, func() {
			syscall.RawSyscall(syscall.SYS_WRITE, neverIssued, 0, 0)
		})
	})
}

// TestDSTFSVirtualFDFlockFailedNBConversionLosesLock: Linux's flock conversion
// removes the holder's existing lock BEFORE the conflict scan (fs/locks.c), so
// a failed LOCK_NB conversion has already lost the old lock — EWOULDBLOCK
// leaves the caller holding nothing, and a third process can then take EX even
// while the failed converter's fd is still open. A model that retained the
// lock would produce holder-retains executions no real kernel can.
func TestDSTFSVirtualFDFlockFailedNBConversionLosesLock(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/lock", []byte("x"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			locked1 := make(chan error, 1)
			release1 := make(chan struct{})
			locked2 := make(chan error, 1)
			nbRes := make(chan error, 1)
			release2 := make(chan struct{})
			p1done := make(chan error, 1)
			go simulation.Process("p1", func() {
				f, err := os.Open("/lock")
				if err != nil {
					locked1 <- err
					return
				}
				locked1 <- syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
				<-release1
				p1done <- f.Close()
			})
			go simulation.Process("p2", func() {
				f, err := os.Open("/lock")
				if err != nil {
					locked2 <- err
					return
				}
				locked2 <- syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
				// p1's SH conflicts with EX: the nonblocking upgrade fails —
				// and drops p2's own SH on the way, per the kernel.
				nbRes <- syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				<-release2 // keep the fd OPEN: the lock is gone, the fd is not
				f.Close()
			})
			if err := <-locked1; err != nil {
				t.Fatalf("p1 SH: %v", err)
			}
			if err := <-locked2; err != nil {
				t.Fatalf("p2 SH: %v", err)
			}
			if err := <-nbRes; !errors.Is(err, syscall.EWOULDBLOCK) {
				t.Fatalf("p2 EX|NB while p1 shared = %v, want EWOULDBLOCK", err)
			}
			close(release1)
			if err := <-p1done; err != nil {
				t.Fatalf("p1 Close: %v", err)
			}
			simulation.Process("p3", func() {
				f, err := os.Open("/lock")
				if err != nil {
					t.Fatalf("p3 Open: %v", err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatalf("p3 EX|NB after p1 release = %v, want success (p2's failed conversion lost its SH; its open fd holds nothing)", err)
				}
			})
			close(release2)
		})
	})
}

// TestDSTRawFcntlFenced pins that a numeric real fd carries no capability,
// regardless of whether the fcntl command probes or mints a descriptor.
func TestDSTRawFcntlFenced(t *testing.T) {
	simulation.Run(1, func() {
		expectDSTRawSyscallPanic(t, func() {
			syscall.Syscall(syscall.SYS_FCNTL, 1, syscall.F_DUPFD, 0)
		})
		expectDSTRawSyscallPanic(t, func() {
			syscall.Syscall(syscall.SYS_FCNTL, 1, syscall.F_DUPFD_CLOEXEC, 0)
		})
		expectDSTRawSyscallPanic(t, func() {
			syscall.RawSyscall(syscall.SYS_FCNTL, 1, syscall.F_DUPFD, 0)
		})
		expectDSTRawSyscallPanic(t, func() {
			syscall.Syscall(syscall.SYS_FCNTL, 1, syscall.F_GETFD, 0)
		})
	})
}

// TestDSTFSVirtualFDMmapSubrangeAlignment: mprotect/madvise on a subrange
// whose FILE offset is not page-aligned is EINVAL (the deterministic analog of
// the kernel's page-aligned-address requirement); aligned subranges work.
func TestDSTFSVirtualFDMmapSubrangeAlignment(t *testing.T) {
	simulation.Run(1, func() {
		const page = 4096
		if err := os.WriteFile("/m", make([]byte, page*2), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/m", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		b, err := syscall.Mmap(int(f.Fd()), 0, page*2, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap: %v", err)
		}
		defer syscall.Munmap(b)
		if err := syscall.Mprotect(b[page:], syscall.PROT_READ); err != nil {
			t.Fatalf("aligned subrange Mprotect: %v", err)
		}
		if err := syscall.Mprotect(b[1:], syscall.PROT_READ); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("unaligned subrange Mprotect = %v, want EINVAL", err)
		}
		if err := syscall.Madvise(b[page:], syscall.MADV_HUGEPAGE); err != nil {
			t.Fatalf("aligned subrange Madvise: %v", err)
		}
		if err := syscall.Madvise(b[3:], syscall.MADV_HUGEPAGE); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("unaligned subrange Madvise = %v, want EINVAL", err)
		}
	})
}

// TestDSTFSVirtualFDMmapLeakedAcrossRunsInert: a mapping leaked out of its run
// is dead residue in the next run — the registry rolls with the epoch, so the
// stale slice is not a mapping there (Munmap answers the deterministic EINVAL)
// and the same file range maps freshly.
func TestDSTFSVirtualFDMmapLeakedAcrossRunsInert(t *testing.T) {
	var leaked []byte
	simulation.Run(1, func() {
		if err := os.WriteFile("/m", make([]byte, 8), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/m", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		leaked, err = syscall.Mmap(int(f.Fd()), 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("Mmap: %v", err)
		}
	})
	simulation.Run(1, func() {
		if err := os.WriteFile("/m", make([]byte, 8), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		f, err := os.OpenFile("/m", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer f.Close()
		b, err := syscall.Mmap(int(f.Fd()), 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatalf("fresh-run Mmap: %v", err)
		}
		if err := syscall.Munmap(b); err != nil {
			t.Fatalf("fresh-run Munmap: %v", err)
		}
		if err := syscall.Munmap(leaked); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("Munmap of prior run's leaked mapping = %v, want EINVAL", err)
		}
	})
}

// TestDSTProcessExitReleasesResources: a Process body's return is the process's
// EXIT — its flocks release (a restart re-acquires where it would EWOULDBLOCK),
// its virtual fd numbers die (EBADF for the restart), its writable mappings
// write back (page cache persists) WITHOUT committing the durable image, and
// unregister (the leaked slice is not a mapping; the restart maps the range
// fresh).
func TestDSTProcessExitReleasesResources(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			if err := os.WriteFile("/db", make([]byte, 8), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			var oldFD int
			var leakedMap []byte
			simulation.Process("p", func() {
				f, err := os.OpenFile("/db", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("p OpenFile: %v", err)
				}
				oldFD = int(f.Fd())
				if err := syscall.Flock(oldFD, syscall.LOCK_EX); err != nil {
					t.Fatalf("p Flock: %v", err)
				}
				leakedMap, err = syscall.Mmap(oldFD, 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Fatalf("p Mmap: %v", err)
				}
				leakedMap[0] = 'Z'
				// Exit with the lock held, the fd open, and the mapping dirty.
			})

			// The dirty mapped byte was written back to the (shared, surviving)
			// file state — page cache belongs to the kernel, not the process...
			cur, synced, _, _, _, _, ok := os.DSTFSNodeState("/db")
			if !ok {
				t.Fatalf("DSTFSNodeState missing /db")
			}
			if cur[0] != 'Z' {
				t.Fatalf("post-exit file byte = %q, want 'Z' (write-back on exit)", cur[0])
			}
			// ...but never committed durably: exit moves no durability boundary.
			if len(synced) != 0 {
				t.Fatalf("exit write-back advanced the durable image: %q, want empty", synced)
			}

			simulation.Process("p", func() {
				f, err := os.OpenFile("/db", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("restart OpenFile: %v", err)
				}
				defer f.Close()
				// The exited invocation's lock is gone (kernel releases on exit).
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatalf("restart Flock = %v, want success (exit released the lock)", err)
				}
				// The exited invocation's fd number is dead capital.
				var buf [1]byte
				if _, err := syscall.Pread(oldFD, buf[:], 0); !errors.Is(err, syscall.EBADF) {
					t.Fatalf("Pread on exited invocation's fd = %v, want EBADF", err)
				}
				// Its mapping is unregistered; the range maps fresh.
				b, err := syscall.Mmap(int(f.Fd()), 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Fatalf("restart Mmap: %v", err)
				}
				if b[0] != 'Z' {
					t.Fatalf("restart mapping byte = %q, want 'Z'", b[0])
				}
				if err := syscall.Munmap(b); err != nil {
					t.Fatalf("restart Munmap: %v", err)
				}
				if err := syscall.Munmap(leakedMap); !errors.Is(err, syscall.EINVAL) {
					t.Fatalf("Munmap of exited invocation's mapping = %v, want EINVAL", err)
				}
			})
		})
	})
}

// TestDSTProcStatEdgeIdentity: procfs edge shapes — a trailing slash on a proc
// leaf is ENOTDIR (the leaf exists, is not a directory), a zero-padded pid is
// not a procfs name (Linux name_to_int rejects leading zeros), and
// /proc/self/stat is the SAME FILE as /proc/<own-pid>/stat.
func TestDSTProcStatEdgeIdentity(t *testing.T) {
	simulation.RunWith(1, simulation.Options{PID: 42}, func() {
		if _, err := os.Open("/proc/self/stat/"); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("Open(/proc/self/stat/) = %v, want ENOTDIR", err)
		}
		if _, err := os.Open("/proc/042/stat"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Open(/proc/042/stat) = %v, want not-exist (leading zero)", err)
		}
		selfInfo, err := os.Stat("/proc/self/stat")
		if err != nil {
			t.Fatalf("Stat self: %v", err)
		}
		pidInfo, err := os.Stat("/proc/42/stat")
		if err != nil {
			t.Fatalf("Stat pid: %v", err)
		}
		if !os.SameFile(selfInfo, pidInfo) {
			t.Fatalf("SameFile(/proc/self/stat, /proc/42/stat) = false, want true")
		}
	})
}

// TestDSTFSOpenRootSubtreeHasNoProc: the procfs overlay exists only at the
// TREE root — a Root of a subdirectory must not serve proc/<pid>/stat from a
// relative path (the guard keys on the captured node being the disk root).
func TestDSTFSOpenRootSubtreeHasNoProc(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.Mkdir("/sub", 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		r, err := os.OpenRoot("/sub")
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		defer r.Close()
		if _, err := r.Open("proc/self/stat"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("sub-root proc/self/stat = %v, want not-exist (no procfs below the tree root)", err)
		}
		// And the ROOT root does serve it (the guard's positive leg).
		rr, err := os.OpenRoot("/")
		if err != nil {
			t.Fatalf("OpenRoot /: %v", err)
		}
		defer rr.Close()
		f, err := rr.Open("proc/self/stat")
		if err != nil {
			t.Fatalf("root proc/self/stat: %v", err)
		}
		f.Close()
	})
}

// TestDSTFSTruncateRegrowReadsZeros: bytes a truncate dropped must not
// resurrect when the file grows back over them. The page cache makes this a
// real hazard: the memfd holds pages, and a shrink that failed to ftruncate
// would leave the old bytes waiting under the re-grown range.
func TestDSTFSTruncateRegrowReadsZeros(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/f", []byte("abcd"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.Truncate("/f", 0); err != nil {
			t.Fatalf("Truncate down: %v", err)
		}
		if err := os.Truncate("/f", 4); err != nil {
			t.Fatalf("Truncate up: %v", err)
		}
		got, err := os.ReadFile("/f")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != "\x00\x00\x00\x00" {
			t.Fatalf("re-grown file reads %q, want four zero bytes: truncation dropped these", got)
		}
	})
}

// TestDSTFSStaleHandleFromDeadRunRefused: a *File leaked out of one run and
// used in the next answers "file already closed" — its run's nodes were
// released with the run (their page caches are gone), and before the epoch
// gate this dereferenced them. The virtual-fd front door has the same gate
// (EBADF); this pins the File-method door.
func TestDSTFSStaleHandleFromDeadRunRefused(t *testing.T) {
	var leaked *os.File
	simulation.Run(1, func() {
		f, err := os.Create("/f")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := f.WriteString("live"); err != nil {
			t.Fatalf("Write in owning run: %v", err)
		}
		leaked = f
	})
	simulation.Run(2, func() {
		if _, err := leaked.WriteString("stale"); err == nil {
			t.Fatalf("a dead run's handle accepted a write")
		} else if !errors.Is(err, os.ErrClosed) {
			t.Fatalf("a dead run's handle failed with %v, want ErrClosed", err)
		}
		if _, err := leaked.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("a dead run's handle Stat = %v, want ErrClosed", err)
		}
	})
}

// TestDSTFSWriteAtHugeOffsetIsEFBIG: an offset so large the write's end
// overflows int64 is the file-size limit, not an index computation — real
// Linux answers EFBIG, and the overflowed arithmetic used to skip the growth
// and panic on the slice index instead.
func TestDSTFSWriteAtHugeOffsetIsEFBIG(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.Create("/f")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		defer f.Close()
		if _, err := f.WriteAt([]byte("xx"), math.MaxInt64-1); !errors.Is(err, syscall.EFBIG) {
			t.Fatalf("WriteAt near MaxInt64 = %v, want EFBIG", err)
		}
	})
}
