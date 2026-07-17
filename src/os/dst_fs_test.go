// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
	"unsafe"
)

func isDSTUnsupportedFS(err error) bool {
	return err != nil && strings.Contains(err.Error(),
		"filesystem operation unsupported under deterministic simulation")
}

// TestDSTFSBasic exercises the chunk-1 file surface end to end on the
// simulated tree: create, write, seek, read, pread/pwrite, truncate, stat,
// sync, close.
func TestDSTFSBasic(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.Create("/a.txt")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if n, err := f.Write([]byte("hello world")); n != 11 || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
		if off, err := f.Seek(0, io.SeekStart); off != 0 || err != nil {
			t.Fatalf("Seek = %d, %v", off, err)
		}
		buf := make([]byte, 5)
		if n, err := f.Read(buf); n != 5 || err != nil || string(buf) != "hello" {
			t.Fatalf("Read = %d, %v, %q", n, err, buf)
		}
		if n, err := f.WriteAt([]byte("WORLD"), 6); n != 5 || err != nil {
			t.Fatalf("WriteAt = %d, %v", n, err)
		}
		got := make([]byte, 5)
		if n, err := f.ReadAt(got, 6); n != 5 || err != nil || string(got) != "WORLD" {
			t.Fatalf("ReadAt = %d, %v, %q", n, err, got)
		}
		if err := f.Truncate(5); err != nil {
			t.Fatalf("Truncate: %v", err)
		}
		fi, err := f.Stat()
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if fi.Size() != 5 || fi.Name() != "a.txt" || fi.IsDir() {
			t.Fatalf("Stat = name %q size %d dir %v", fi.Name(), fi.Size(), fi.IsDir())
		}
		if err := f.Sync(); err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// Reopen: content persisted in the tree; O_APPEND appends.
		f2, err := os.OpenFile("/a.txt", os.O_RDWR|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if _, err := f2.Write([]byte("!!")); err != nil {
			t.Fatalf("append: %v", err)
		}
		if _, err := f2.Seek(0, io.SeekStart); err != nil {
			t.Fatalf("rewind: %v", err)
		}
		all, err := io.ReadAll(f2)
		if err != nil || string(all) != "hello!!" {
			t.Fatalf("content = %q, %v (want %q)", all, err, "hello!!")
		}
		f2.Close()
	})
}

// TestDSTFSErrorIdentity asserts production-shaped errors across the chunk-1
// surface: *PathError wrapping the exact errno, errors.Is identities.
func TestDSTFSErrorIdentity(t *testing.T) {
	simulation.Run(1, func() {
		check := func(what string, err error, want error) {
			var pe *os.PathError
			if !errors.As(err, &pe) {
				t.Fatalf("%s: error %v (%T) is not *PathError", what, err, err)
			}
			if !errors.Is(err, want) {
				t.Fatalf("%s: error %v, want errors.Is %v", what, err, want)
			}
		}

		_, err := os.Open("/missing")
		check("open missing", err, syscall.ENOENT)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("open missing: %v not ErrNotExist", err)
		}

		f, err := os.Create("/f")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = os.OpenFile("/f", os.O_CREATE|os.O_EXCL, 0o644)
		check("O_EXCL exists", err, syscall.EEXIST)
		if !errors.Is(err, os.ErrExist) {
			t.Fatalf("O_EXCL: %v not ErrExist", err)
		}

		_, err = os.Open("/f/child")
		check("file as dir", err, syscall.ENOTDIR)

		_, err = os.OpenFile("/", os.O_WRONLY, 0)
		check("write dir", err, syscall.EISDIR)

		_, err = os.Open("/nosuch/child")
		check("missing intermediate", err, syscall.ENOENT)

		// Access-mode enforcement.
		ro, _ := os.Open("/f")
		_, err = ro.Write([]byte("x"))
		check("write to O_RDONLY", err, syscall.EBADF)
		ro.Close()
		wo, _ := os.OpenFile("/f", os.O_WRONLY, 0)
		_, err = wo.Read(make([]byte, 1))
		check("read from O_WRONLY", err, syscall.EBADF)
		wo.Close()

		// Close semantics.
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := f.Close(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("double Close = %v, want ErrClosed", err)
		}
		if _, err := f.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("read after close = %v, want ErrClosed", err)
		}

		// Seek validation.
		g, _ := os.Create("/g")
		if _, err := g.Seek(-1, io.SeekStart); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("negative seek = %v, want EINVAL", err)
		}
		// EOF identity.
		if _, err := g.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("read at EOF = %v, want io.EOF", err)
		}
		if _, err := g.ReadAt(make([]byte, 1), 100); err != io.EOF {
			t.Fatalf("ReadAt past EOF = %v, want io.EOF", err)
		}
		g.Close()
	})
}

// TestDSTFSFences asserts every not-yet-modeled named operation fails with
// the deterministic unsupported error under a run instead of reaching the
// host filesystem (the host-isolation invariant's fence half).
func TestDSTFSFences(t *testing.T) {
	simulation.Run(1, func() {
		pathOps := []struct {
			name string
			call func() error
		}{
			{"chown", func() error { return os.Chown("/x", 0, 0) }},
			{"lchown", func() error { return os.Lchown("/x", 0, 0) }},
			{"readlink", func() error { _, err := os.Readlink("/x"); return err }},
		}
		for _, op := range pathOps {
			err := op.call()
			if !isDSTUnsupportedFS(err) {
				t.Fatalf("%s: error %v, want unsupported-under-simulation", op.name, err)
			}
			var pe *os.PathError
			if !errors.As(err, &pe) {
				t.Fatalf("%s: error %v (%T) does not wrap *PathError", op.name, err, err)
			}
		}

		linkOps := []struct {
			name string
			call func() error
		}{
			{"link", func() error { return os.Link("/a", "/b") }},
			{"symlink", func() error { return os.Symlink("/a", "/b") }},
		}
		for _, op := range linkOps {
			err := op.call()
			if !isDSTUnsupportedFS(err) {
				t.Fatalf("%s: error %v, want unsupported-under-simulation", op.name, err)
			}
			var le *os.LinkError
			if !errors.As(err, &le) {
				t.Fatalf("%s: error %v (%T) does not wrap *LinkError", op.name, err, err)
			}
		}
	})
}

// TestDSTFSCopyAndIdentity covers the round-2 review fixes: io.Copy between
// simulated files takes the generic loop (the zero-copy fast paths bail), a
// zero-length read at EOF is (0, nil), O_RDONLY|O_TRUNC truncates as Linux
// does, SameFile keys on node identity, empty paths are ENOENT, the
// metadata round-trips (fake-clock ModTime, stored perm), and the remaining
// unmodeled handle methods carry the fence shape.
func TestDSTFSCopyAndIdentity(t *testing.T) {
	simulation.Run(1, func() {
		// io.Copy sim -> sim (upstream zero-copy would EBADF without the bail).
		src, _ := os.Create("/src")
		src.WriteString("payload-123")
		src.Seek(0, io.SeekStart)
		dst, _ := os.Create("/dst")
		if n, err := io.Copy(dst, src); n != 11 || err != nil {
			t.Fatalf("io.Copy = %d, %v", n, err)
		}
		dst.Seek(0, io.SeekStart)
		got, _ := io.ReadAll(dst)
		if string(got) != "payload-123" {
			t.Fatalf("copied content = %q", got)
		}

		// Zero-length read at EOF: (0, nil), as upstream poll.FD.Read.
		if n, err := src.Read(nil); n != 0 || err != nil {
			t.Fatalf("Read(nil) at EOF = %d, %v, want 0, nil", n, err)
		}

		// O_RDONLY|O_TRUNC truncates (Linux shape).
		rt, err := os.OpenFile("/src", os.O_RDONLY|os.O_TRUNC, 0)
		if err != nil {
			t.Fatalf("O_RDONLY|O_TRUNC: %v", err)
		}
		if fi, _ := rt.Stat(); fi.Size() != 0 {
			t.Fatalf("O_RDONLY|O_TRUNC size = %d, want 0", fi.Size())
		}
		rt.Close()

		// SameFile: same node via two handles true; distinct nodes false;
		// sim/host mix false.
		h1, _ := os.Open("/dst")
		h2, _ := os.Open("/dst")
		fi1, _ := h1.Stat()
		fi2, _ := h2.Stat()
		if !os.SameFile(fi1, fi2) {
			t.Fatalf("SameFile(same node) = false")
		}
		other, _ := os.Create("/other")
		fi3, _ := other.Stat()
		if os.SameFile(fi1, fi3) {
			t.Fatalf("SameFile(distinct nodes) = true")
		}
		h1.Close()
		h2.Close()
		other.Close()

		// Empty path: ENOENT, like the host.
		if _, err := os.Open(""); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf(`Open("") = %v, want ENOENT`, err)
		}

		// Metadata: ModTime is the bubble's fake clock; perm is stored.
		mf, _ := os.OpenFile("/meta", os.O_CREATE|os.O_WRONLY, 0o640)
		mf.WriteString("x")
		fi, _ := mf.Stat()
		if !fi.ModTime().Equal(time.Now()) {
			t.Fatalf("ModTime = %v, want the fake clock now %v", fi.ModTime(), time.Now())
		}
		if fi.Mode() != 0o640 {
			t.Fatalf("Mode = %v, want 0640", fi.Mode())
		}

		// Zero-length read on a write-only file: (0, nil) — upstream's
		// check order (empty buffer before access mode).
		woz, _ := os.OpenFile("/dst", os.O_WRONLY, 0)
		if n, err := woz.Read(nil); n != 0 || err != nil {
			t.Fatalf("Read(nil) on O_WRONLY = %d, %v, want 0, nil", n, err)
		}
		woz.Close()

		// os.Pipe is simulated (design.md "Deterministic pipes and the
		// stdio stance"): an in-memory pair, no host descriptor. Full
		// pipe coverage lives in dst_pipe_test.go.
		if pr, pw, err := os.Pipe(); err != nil {
			t.Fatalf("Pipe = %v, want simulated pair", err)
		} else {
			pr.Close()
			pw.Close()
		}

		// Fd on a tree file is virtual; SyscallConn is still fenced.
		if fd := int(dst.Fd()); fd < 1<<30 {
			t.Fatalf("Fd = %d, want virtual descriptor", fd)
		}
		if _, err := dst.SyscallConn(); !isDSTUnsupportedFS(err) {
			t.Fatalf("SyscallConn = %v, want unsupported", err)
		}

		// Readdir on a regular file: the production ENOTDIR shape (the
		// funnel is implemented as of the namespace chunk); the remaining
		// unmodeled handle methods carry the fence shape.
		if _, err := mf.Readdirnames(0); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("Readdirnames on file = %v, want ENOTDIR", err)
		}
		if err := mf.Chmod(0o600); err != nil {
			t.Fatalf("File.Chmod = %v (implemented as of the metadata chunk)", err)
		}
		if fi2, _ := mf.Stat(); fi2.Mode() != 0o600 {
			t.Fatalf("File.Chmod mode = %v, want 0600", fi2.Mode())
		}
		if err := mf.Chown(0, 0); !isDSTUnsupportedFS(err) {
			t.Fatalf("File.Chown = %v, want unsupported", err)
		}
		if err := mf.Chdir(); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("File.Chdir on file = %v, want ENOTDIR", err)
		}
		mf.Close()
	})
}

func TestDSTFSVirtualFDSyscalls(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.OpenFile("/fd", os.O_CREATE|os.O_RDWR, 0o640)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		fd := int(f.Fd())
		if fd < 1<<30 {
			t.Fatalf("Fd = %d, want virtual descriptor", fd)
		}
		if fd2 := int(f.Fd()); fd2 != fd {
			t.Fatalf("second Fd = %d, want stable %d", fd2, fd)
		}

		if n, err := syscall.Write(fd, []byte("hello")); n != 5 || err != nil {
			t.Fatalf("syscall.Write = %d, %v", n, err)
		}
		if off, err := syscall.Seek(fd, 0, io.SeekStart); off != 0 || err != nil {
			t.Fatalf("syscall.Seek = %d, %v", off, err)
		}
		buf := make([]byte, 5)
		if n, err := syscall.Read(fd, buf); n != 5 || err != nil || string(buf) != "hello" {
			t.Fatalf("syscall.Read = %d, %v, %q", n, err, buf)
		}

		buf = make([]byte, 3)
		if n, err := syscall.Pread(fd, buf, 1); n != 3 || err != nil || string(buf) != "ell" {
			t.Fatalf("syscall.Pread = %d, %v, %q", n, err, buf)
		}
		if n, err := syscall.Pread(fd, make([]byte, 1), -1); n != -1 || !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("negative syscall.Pread = %d, %v, want -1, EINVAL", n, err)
		}
		if n, err := syscall.Pread(fd, nil, -1); n != -1 || !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("zero-length negative syscall.Pread = %d, %v, want -1, EINVAL", n, err)
		}
		if n, err := syscall.Pwrite(fd, []byte("Y"), 1); n != 1 || err != nil {
			t.Fatalf("syscall.Pwrite = %d, %v", n, err)
		}
		if n, err := syscall.Pwrite(fd, nil, -1); n != -1 || !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("zero-length negative syscall.Pwrite = %d, %v, want -1, EINVAL", n, err)
		}
		if n, err := syscall.Pwrite(fd, nil, 1<<20); n != 0 || err != nil {
			t.Fatalf("zero-length syscall.Pwrite = %d, %v; want no-op", n, err)
		}
		var stNoop syscall.Stat_t
		if err := syscall.Fstat(fd, &stNoop); err != nil || stNoop.Size != 5 {
			t.Fatalf("Fstat after zero-length pwrite = size %d, %v; want size 5", stNoop.Size, err)
		}
		buf = make([]byte, 5)
		if n, err := syscall.Pread(fd, buf, 0); n != 5 || err != nil || string(buf) != "hYllo" {
			t.Fatalf("second syscall.Pread = %d, %v, %q; want hYllo", n, err, buf)
		}

		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			t.Fatalf("syscall.Fstat: %v", err)
		}
		if st.Size != 5 || st.Mode&syscall.S_IFREG == 0 || st.Mode&0o777 != 0o640 {
			t.Fatalf("Fstat = size %d mode %#o, want size 5 regular 0640", st.Size, st.Mode)
		}
		tmp, err := os.Open("/tmp")
		if err != nil {
			t.Fatalf("Open /tmp: %v", err)
		}
		var tmpSt syscall.Stat_t
		if err := syscall.Fstat(int(tmp.Fd()), &tmpSt); err != nil {
			t.Fatalf("Fstat /tmp: %v", err)
		}
		if tmpSt.Mode&syscall.S_ISVTX == 0 {
			t.Fatalf("Fstat /tmp mode %#o, want sticky bit", tmpSt.Mode)
		}
		tmp.Close()
		if err := syscall.Close(fd); err != nil {
			t.Fatalf("syscall.Close: %v", err)
		}
		if _, err := syscall.Read(fd, make([]byte, 1)); !errors.Is(err, syscall.EBADF) {
			t.Fatalf("read after raw close = %v, want EBADF", err)
		}
		if got := f.Fd(); got != ^uintptr(0) {
			t.Fatalf("Fd after raw close = %#x, want invalid", got)
		}
		if err := f.Close(); err == nil {
			t.Fatalf("File.Close after raw close = nil, want closed error")
		}
		if err := syscall.Close(-1); !errors.Is(err, syscall.EBADF) {
			t.Fatalf("syscall.Close(-1) = %v, want EBADF", err)
		}

		wo, err := os.OpenFile("/write-only", os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("OpenFile write-only: %v", err)
		}
		wofd := int(wo.Fd())
		if n, err := syscall.Read(wofd, nil); n != -1 || !errors.Is(err, syscall.EBADF) {
			t.Fatalf("zero-length syscall.Read on write-only fd = %d, %v; want -1, EBADF", n, err)
		}
		if n, err := syscall.Pread(wofd, nil, 0); n != -1 || !errors.Is(err, syscall.EBADF) {
			t.Fatalf("zero-length syscall.Pread on write-only fd = %d, %v; want -1, EBADF", n, err)
		}
		wo.Close()

		ro, err := os.Open("/fd")
		if err != nil {
			t.Fatalf("Open read-only: %v", err)
		}
		rofd := int(ro.Fd())
		if n, err := syscall.Write(rofd, nil); n != -1 || !errors.Is(err, syscall.EBADF) {
			t.Fatalf("zero-length syscall.Write on read-only fd = %d, %v; want -1, EBADF", n, err)
		}
		if n, err := syscall.Pwrite(rofd, nil, 0); n != -1 || !errors.Is(err, syscall.EBADF) {
			t.Fatalf("zero-length syscall.Pwrite on read-only fd = %d, %v; want -1, EBADF", n, err)
		}
		ro.Close()
	})
}

func TestDSTFSVirtualFDWritePartialENOSPC(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.LimitDisk("h", 5)
			f, err := os.Create("/fd")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			fd := int(f.Fd())
			if n, err := syscall.Write(fd, []byte("abcdef")); n != 5 || err != nil {
				t.Fatalf("partial syscall.Write = %d, %v; want 5, nil", n, err)
			}
			if n, err := syscall.Write(fd, []byte("z")); n != -1 || !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("full syscall.Write = %d, %v; want -1, ENOSPC", n, err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			b, err := os.ReadFile("/fd")
			if err != nil || string(b) != "abcde" {
				t.Fatalf("content after partial syscall.Write = %q, %v; want abcde", b, err)
			}
		})
	})
}

func TestDSTFSVirtualFDClosedFileDoesNotMint(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.Create("/fd")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if fd := f.Fd(); fd == ^uintptr(0) {
			t.Fatalf("open Fd = invalid")
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if fd := f.Fd(); fd != ^uintptr(0) {
			t.Fatalf("closed Fd = %#x, want invalid", fd)
		}
	})
}

func TestDSTFSVirtualFDStaleEpochDoesNotReleaseLiveFD(t *testing.T) {
	var leaked *os.File
	simulation.Run(1, func() {
		var err error
		leaked, err = os.Create("/old")
		if err != nil {
			t.Fatalf("Create old: %v", err)
		}
		_ = leaked.Fd()
	})
	simulation.Run(1, func() {
		live, err := os.Create("/new")
		if err != nil {
			t.Fatalf("Create new: %v", err)
		}
		fd := int(live.Fd())
		if err := leaked.Close(); err != nil {
			t.Fatalf("close leaked old file: %v", err)
		}
		if n, err := syscall.Write(fd, []byte("x")); n != 1 || err != nil {
			t.Fatalf("write live fd after leaked close = %d, %v; want live fd intact", n, err)
		}
		live.Close()
	})
}

func TestDSTFSVirtualFDRawSyscallFenced(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func(fd uintptr)
	}{
		{"Syscall", func(fd uintptr) { syscall.Syscall(syscall.SYS_READ, fd, 0, 0) }},
		{"RawSyscall", func(fd uintptr) { syscall.RawSyscall(syscall.SYS_READ, fd, 0, 0) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			simulation.Run(1, func() {
				f, err := os.Create("/fd")
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				fd := f.Fd()
				defer f.Close()

				defer func() {
					r := recover()
					if r == nil || !strings.Contains(fmt.Sprint(r), "unsupported under deterministic simulation") {
						t.Fatalf("raw syscall panic = %v, want unsupported-under-simulation", r)
					}
				}()
				tt.call(fd)
			})
		})
	}
}

func TestDSTFSVirtualFDRawSyscallStaleFDFenced(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.Create("/fd")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		fd := f.Fd()
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		defer func() {
			r := recover()
			if r == nil || !strings.Contains(fmt.Sprint(r), "unsupported under deterministic simulation") {
				t.Fatalf("stale raw syscall panic = %v, want unsupported-under-simulation", r)
			}
		}()
		syscall.RawSyscall(syscall.SYS_READ, fd, 0, 0)
	})
}

func TestDSTFSVirtualFDProcessIsolation(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			opened := make(chan int, 1)
			release := make(chan struct{})
			p1err := make(chan error, 1)
			go simulation.Process("p1", func() {
				f, err := os.Create("/fd")
				if err != nil {
					p1err <- err
					return
				}
				opened <- int(f.Fd())
				<-release
				p1err <- f.Close()
			})
			var fd int
			select {
			case err := <-p1err:
				t.Fatalf("Create: %v", err)
			case fd = <-opened:
			}
			releaseClosed := false
			defer func() {
				if !releaseClosed {
					close(release)
				}
			}()
			simulation.Process("p2", func() {
				if _, err := syscall.Read(fd, make([]byte, 1)); !errors.Is(err, syscall.EBADF) {
					t.Fatalf("cross-process read = %v, want EBADF", err)
				}
			})
			close(release)
			releaseClosed = true
			if err := <-p1err; err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	})
}

// TestDSTFSNamespace covers the chunk-2 named operations end to end:
// Mkdir/MkdirAll/Remove/RemoveAll/Rename/Stat/Lstat with production error
// identity, rename atomicity rules, and unlinked-but-open semantics.
func TestDSTFSNamespace(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.Mkdir("/a", 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		if err := os.Mkdir("/a", 0o755); !errors.Is(err, syscall.EEXIST) {
			t.Fatalf("Mkdir exists = %v, want EEXIST", err)
		}
		if err := os.MkdirAll("/a/b/c", 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		fi, err := os.Stat("/a/b/c")
		if err != nil || !fi.IsDir() || fi.Name() != "c" {
			t.Fatalf("Stat dir = %v, %v", fi, err)
		}
		if _, err := os.Stat("/a/missing"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Stat missing = %v", err)
		}
		if fi2, err := os.Lstat("/a/b/c"); err != nil || !os.SameFile(fi, fi2) {
			t.Fatalf("Lstat/SameFile = %v, %v", fi2, err)
		}

		// Remove: non-empty dir refuses; empty dir and files go.
		if err := os.Remove("/a"); !errors.Is(err, syscall.ENOTEMPTY) {
			t.Fatalf("Remove non-empty = %v, want ENOTEMPTY", err)
		}
		if err := os.Remove("/a/b/c"); err != nil {
			t.Fatalf("Remove empty dir: %v", err)
		}
		if err := os.WriteFile("/a/f", []byte("data"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.RemoveAll("/a"); err != nil {
			t.Fatalf("RemoveAll: %v", err)
		}
		if _, err := os.Stat("/a"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("post-RemoveAll Stat = %v", err)
		}
		if err := os.RemoveAll("/nosuch"); err != nil {
			t.Fatalf("RemoveAll missing = %v, want nil", err)
		}

		// Rename: move, replace-file target, missing source, into-self.
		os.WriteFile("/r1", []byte("one"), 0o644)
		os.WriteFile("/r2", []byte("two"), 0o644)
		if err := os.Rename("/r1", "/r3"); err != nil {
			t.Fatalf("Rename move: %v", err)
		}
		if err := os.Rename("/r3", "/r2"); err != nil {
			t.Fatalf("Rename replace: %v", err)
		}
		got, _ := os.ReadFile("/r2")
		if string(got) != "one" {
			t.Fatalf("rename-replace content = %q", got)
		}
		if _, err := os.Stat("/r3"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rename left the old name: %v", err)
		}
		if err := os.Rename("/missing", "/x"); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("Rename missing = %v", err)
		}
		var le *os.LinkError
		if err := os.Rename("/missing", "/x"); !errors.As(err, &le) {
			t.Fatalf("Rename error type = %T", err)
		}
		os.Mkdir("/d1", 0o755)
		if err := os.Rename("/d1", "/d1/sub"); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("Rename into self = %v, want EINVAL", err)
		}

		// Unlinked-but-open: content survives the name.
		f, _ := os.Create("/wal")
		f.WriteString("entry1")
		if err := os.Remove("/wal"); err != nil {
			t.Fatalf("Remove open file: %v", err)
		}
		if _, err := os.Stat("/wal"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("name survived remove: %v", err)
		}
		f.WriteString("|entry2")
		buf := make([]byte, 64)
		n, _ := f.ReadAt(buf, 0)
		if string(buf[:n]) != "entry1|entry2" {
			t.Fatalf("unlinked-open content = %q", buf[:n])
		}
		f.Close()
	})
}

// TestDSTFSNamespaceEdgeIdentity pins the host shapes for the degenerate
// names (the M1 review family — Remove("") must NOT delete the cwd), the
// rename dir-target EEXIST precheck, root removal, and relative two-name ops
// against a non-root cwd (the M2 review case: relative Open + File.Chdir).
func TestDSTFSNamespaceEdgeIdentity(t *testing.T) {
	simulation.Run(1, func() {
		os.Mkdir("/work", 0o755)
		if err := os.Chdir("/work"); err != nil {
			t.Fatalf("Chdir: %v", err)
		}

		// Degenerate names: host shapes, and the cwd must survive.
		if err := os.Remove(""); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf(`Remove("") = %v, want ENOENT`, err)
		}
		if err := os.Remove("."); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf(`Remove(".") = %v, want EINVAL`, err)
		}
		if err := os.RemoveAll("."); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf(`RemoveAll(".") = %v, want EINVAL`, err)
		}
		if err := os.Rename("", "/x"); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf(`Rename("",x) = %v, want ENOENT`, err)
		}
		if err := os.Rename("/work", ""); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf(`Rename(x,"") = %v, want ENOENT`, err)
		}
		if err := os.Chdir(""); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf(`Chdir("") = %v, want ENOENT`, err)
		}
		if _, err := os.Stat("/work"); err != nil {
			t.Fatalf("cwd was harmed by degenerate names: %v", err)
		}

		// Relative two-name ops resolve both names against the cwd.
		os.WriteFile("relsrc", []byte("rel"), 0o644)
		if err := os.Rename("relsrc", "reldst"); err != nil {
			t.Fatalf("relative Rename: %v", err)
		}
		if got, err := os.ReadFile("/work/reldst"); err != nil || string(got) != "rel" {
			t.Fatalf("relative rename target = %q, %v", got, err)
		}
		if err := os.Remove("reldst"); err != nil {
			t.Fatalf("relative Remove: %v", err)
		}

		// Relative dir open + File.Chdir (the M2 case).
		os.Mkdir("sub", 0o755)
		dh, err := os.Open("sub")
		if err != nil {
			t.Fatalf("relative Open dir: %v", err)
		}
		if err := dh.Chdir(); err != nil {
			t.Fatalf("File.Chdir: %v", err)
		}
		dh.Close()
		if wd, _ := os.Getwd(); wd != "/work/sub" {
			t.Fatalf("Getwd after relative File.Chdir = %q, want /work/sub", wd)
		}
		os.Chdir("/")

		// Rename onto an existing directory target: the wrapper's EEXIST
		// shape (Go's deliberate cross-platform unification), via the
		// sim-aware Lstat precheck.
		os.WriteFile("/pf", []byte("x"), 0o644)
		os.Mkdir("/pd", 0o755)
		if err := os.Rename("/pf", "/pd"); !errors.Is(err, syscall.EEXIST) {
			t.Fatalf("Rename file->dir = %v, want EEXIST", err)
		}

		// Root removal: EBUSY, like the host's error-priority pick.
		if err := os.Remove("/"); !errors.Is(err, syscall.EBUSY) {
			t.Fatalf(`Remove("/") = %v, want EBUSY`, err)
		}

		// Empty-directory listing: non-nil empty result.
		os.Mkdir("/empty", 0o755)
		eh, _ := os.Open("/empty")
		names, err := eh.Readdirnames(0)
		if err != nil || names == nil || len(names) != 0 {
			t.Fatalf("empty Readdirnames = %v (nil=%v), %v", names, names == nil, err)
		}
		eh.Close()
	})
}

// TestDSTFSReadDirAndCwd covers the sorted deterministic listing (one-shot
// and chunked with the stable cursor), os.ReadDir, Readdirnames, and the
// per-bubble working directory (Getwd/Chdir/File.Chdir + relative paths).
func TestDSTFSReadDirAndCwd(t *testing.T) {
	simulation.Run(1, func() {
		for _, n := range []string{"zz", "aa", "mm", "bb"} {
			if err := os.WriteFile("/"+n, []byte(n), 0o644); err != nil {
				t.Fatalf("WriteFile %s: %v", n, err)
			}
		}
		os.Mkdir("/sub", 0o755)

		ents, err := os.ReadDir("/")
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		want := "aa,bb,dev,mm,sub,tmp,zz" // dev and tmp are the pre-seeded tree
		if got := strings.Join(names, ","); got != want {
			t.Fatalf("ReadDir order = %s, want %s", got, want)
		}
		if !ents[4].IsDir() || ents[4].Type()&os.ModeDir == 0 {
			t.Fatalf("DirEntry sub not a dir: %v %v", ents[4].IsDir(), ents[4].Type())
		}

		// Chunked reads: stable cursor, io.EOF at exhaustion.
		dh, err := os.Open("/")
		if err != nil {
			t.Fatalf("Open dir: %v", err)
		}
		n1, err1 := dh.Readdirnames(2)
		n2, err2 := dh.Readdirnames(2)
		n3, err3 := dh.Readdirnames(3)
		_, err4 := dh.Readdirnames(2)
		if err1 != nil || err2 != nil || err3 != nil {
			t.Fatalf("chunked errs: %v %v %v", err1, err2, err3)
		}
		if err4 != io.EOF {
			t.Fatalf("exhausted Readdirnames err = %v, want io.EOF", err4)
		}
		if got := strings.Join(append(append(n1, n2...), n3...), ","); got != want {
			t.Fatalf("chunked order = %s, want %s", got, want)
		}
		// Read(2) on a directory handle: EISDIR.
		if _, err := dh.Read(make([]byte, 4)); !errors.Is(err, syscall.EISDIR) {
			t.Fatalf("Read on dir = %v, want EISDIR", err)
		}
		dh.Close()

		// cwd: starts at /, Chdir is per-bubble, relative paths resolve.
		if wd, err := os.Getwd(); wd != "/" || err != nil {
			t.Fatalf("Getwd = %q, %v", wd, err)
		}
		if err := os.Chdir("/sub"); err != nil {
			t.Fatalf("Chdir: %v", err)
		}
		if wd, _ := os.Getwd(); wd != "/sub" {
			t.Fatalf("Getwd after Chdir = %q", wd)
		}
		if err := os.WriteFile("rel.txt", []byte("relative"), 0o644); err != nil {
			t.Fatalf("relative WriteFile: %v", err)
		}
		if got, err := os.ReadFile("/sub/rel.txt"); err != nil || string(got) != "relative" {
			t.Fatalf("relative file = %q, %v", got, err)
		}
		if err := os.Chdir("/sub/rel.txt"); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("Chdir to file = %v, want ENOTDIR", err)
		}
		root, _ := os.Open("/")
		if err := root.Chdir(); err != nil {
			t.Fatalf("File.Chdir: %v", err)
		}
		root.Close()
		if wd, _ := os.Getwd(); wd != "/" {
			t.Fatalf("Getwd after File.Chdir = %q", wd)
		}
	})

	// cwd resets across runs with the rest of the tree — proven by a run
	// that ENDS away from root (a run ending at "/" would mask a stale cwd).
	simulation.Run(1, func() {
		os.Mkdir("/leftover", 0o755)
		if err := os.Chdir("/leftover"); err != nil {
			t.Fatalf("Chdir: %v", err)
		}
	})
	simulation.Run(1, func() {
		if wd, _ := os.Getwd(); wd != "/" {
			t.Fatalf("fresh run Getwd = %q, want / (cwd leaked across runs)", wd)
		}
	})
}

func TestDSTFSOpenRoot(t *testing.T) {
	simulation.Run(1, func() {
		mustErrIs := func(what string, err, target error) {
			t.Helper()
			if !errors.Is(err, target) {
				t.Fatalf("%s = %v, want %v", what, err, target)
			}
		}
		mustNotExist := func(name string) {
			t.Helper()
			if _, err := os.Stat(name); !errors.Is(err, syscall.ENOENT) {
				t.Fatalf("Stat(%q) = %v, want ENOENT", name, err)
			}
		}

		if err := os.Mkdir("/root", 0o755); err != nil {
			t.Fatalf("Mkdir root: %v", err)
		}
		if err := os.WriteFile("/outside", []byte("outside"), 0o644); err != nil {
			t.Fatalf("WriteFile outside: %v", err)
		}
		r, err := os.OpenRoot("/root")
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		defer r.Close()

		if err := r.MkdirAll("sub/dir", 0o755); err != nil {
			t.Fatalf("Root.MkdirAll: %v", err)
		}
		if err := r.WriteFile("sub/dir/file", []byte("in-root"), 0o640); err != nil {
			t.Fatalf("Root.WriteFile: %v", err)
		}
		if got, err := os.ReadFile("/root/sub/dir/file"); err != nil || string(got) != "in-root" {
			t.Fatalf("ReadFile rooted result = %q, %v", got, err)
		}
		if err := r.Chmod("sub/dir/file", 0o600); err != nil {
			t.Fatalf("Root.Chmod: %v", err)
		}
		mtime := time.Unix(123, 0)
		if err := r.Chtimes("sub/dir/file", time.Time{}, mtime); err != nil {
			t.Fatalf("Root.Chtimes: %v", err)
		}
		fi, err := r.Stat("sub/dir/file")
		if err != nil {
			t.Fatalf("Root.Stat: %v", err)
		}
		if fi.Name() != "file" || fi.Mode().Perm() != 0o600 || !fi.ModTime().Equal(mtime) {
			t.Fatalf("Root.Stat = name %q mode %v mtime %v", fi.Name(), fi.Mode().Perm(), fi.ModTime())
		}
		_, err = r.OpenFile("sub/dir/file", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		mustErrIs("Root.OpenFile O_EXCL existing", err, syscall.EEXIST)
		// ENOENT, not the plain open(2)'s EISDIR: openat2 (os.Root's
		// resolver) rejects the slash-asserted missing final component
		// first (host-probed; TestDSTFSTrailingSlashCreateLegs).
		_, err = r.OpenFile("new/", os.O_WRONLY|os.O_CREATE, 0o600)
		mustErrIs("Root.OpenFile create trailing slash", err, syscall.ENOENT)
		_, err = r.OpenFile("sub/dir", os.O_RDONLY|os.O_TRUNC, 0)
		mustErrIs("Root.OpenFile truncate dir", err, syscall.EISDIR)
		if err := r.Chown("sub/dir/file", 1, 1); !isDSTUnsupportedFS(err) {
			t.Fatalf("Root.Chown = %v, want unsupported-under-simulation", err)
		}
		if err := r.Lchown("sub/dir/file", 1, 1); !isDSTUnsupportedFS(err) {
			t.Fatalf("Root.Lchown = %v, want unsupported-under-simulation", err)
		}
		if _, err := r.Readlink("sub/dir/file"); !isDSTUnsupportedFS(err) {
			t.Fatalf("Root.Readlink = %v, want unsupported-under-simulation", err)
		}
		if err := r.Link("sub/dir/file", "sub/dir/link"); !isDSTUnsupportedFS(err) {
			t.Fatalf("Root.Link = %v, want unsupported-under-simulation", err)
		}
		if err := r.Symlink("sub/dir/file", "sub/dir/symlink"); !isDSTUnsupportedFS(err) {
			t.Fatalf("Root.Symlink = %v, want unsupported-under-simulation", err)
		}
		if err := r.MkdirAll("dot/child", 0o755); err != nil {
			t.Fatalf("Root.MkdirAll dot: %v", err)
		}
		fi, err = r.Stat("dot/.")
		if err != nil {
			t.Fatalf(`Root.Stat("dot/."): %v`, err)
		}
		if fi.Name() != "dot" {
			t.Fatalf(`Root.Stat("dot/.") name = %q, want dot`, fi.Name())
		}
		if err := r.Rename("dot/child/..", "dot-renamed"); err != nil {
			t.Fatalf(`Root.Rename("dot/child/..", "dot-renamed"): %v`, err)
		}
		mustNotExist("/root/dot")
		if _, err := r.Stat("dot-renamed/child"); err != nil {
			t.Fatalf("Root.Stat renamed terminal dotdot child: %v", err)
		}

		if _, err := r.Open("../outside"); err == nil {
			t.Fatalf("Root.Open escape succeeded")
		}
		if err := r.Mkdir("../escape", 0o755); err == nil {
			t.Fatalf("Root.Mkdir escape succeeded")
		}
		if err := r.MkdirAll("../escape", 0o755); err == nil {
			t.Fatalf("Root.MkdirAll escape succeeded")
		}
		if err := r.WriteFile("../outside", []byte("escaped"), 0o644); err == nil {
			t.Fatalf("Root.WriteFile escape succeeded")
		}
		if err := r.Remove("../outside"); err == nil {
			t.Fatalf("Root.Remove escape succeeded")
		}
		if err := r.RemoveAll("../outside"); err == nil {
			t.Fatalf("Root.RemoveAll escape succeeded")
		}
		if got, _ := os.ReadFile("/outside"); string(got) != "outside" {
			t.Fatalf("outside file = %q, want unchanged", got)
		}
		if _, err := r.Open("/outside"); err == nil {
			t.Fatalf("Root.Open absolute path succeeded")
		}

		sub, err := r.OpenRoot("sub")
		if err != nil {
			t.Fatalf("Root.OpenRoot: %v", err)
		}
		defer sub.Close()
		if got, err := sub.ReadFile("dir/file"); err != nil || string(got) != "in-root" {
			t.Fatalf("nested Root.ReadFile = %q, %v", got, err)
		}
		if err := sub.Rename("dir/file", "dir/renamed"); err != nil {
			t.Fatalf("nested Root.Rename: %v", err)
		}
		if got, err := r.ReadFile("sub/dir/renamed"); err != nil || string(got) != "in-root" {
			t.Fatalf("Root.ReadFile renamed = %q, %v", got, err)
		}
		if err := r.Rename("sub", "sub/dir/child"); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("Root.Rename ancestor = %v, want EINVAL", err)
		}
		if err := r.Rename("sub/dir/renamed", "missing/"); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf(`Root.Rename("sub/dir/renamed", "missing/") = %v, want ENOTDIR`, err)
		}
		mustNotExist("/root/missing")
		// A DIRECTORY source renames onto a trailing-slash missing newpath
		// (renameat shares rename(2)'s source-type rule; host-probed
		// through os.Root.Rename).
		if err := r.Mkdir("dmv", 0o755); err != nil {
			t.Fatalf("Root.Mkdir dmv: %v", err)
		}
		if err := r.Rename("dmv", "dmoved/"); err != nil {
			t.Fatalf(`Root.Rename("dmv", "dmoved/") = %v, want success (dir source)`, err)
		}
		if fi, err := r.Stat("dmoved"); err != nil || !fi.IsDir() {
			t.Fatalf("Root.Stat renamed dir = %v, %v; want a directory", fi, err)
		}
		if err := r.Remove("dmoved"); err != nil {
			t.Fatalf("Root.Remove dmoved: %v", err)
		}
		if err := r.Rename("sub/dir/renamed", "../outside"); err == nil {
			t.Fatalf("Root.Rename escape succeeded")
		}
		if got, err := r.ReadFile("sub/dir/renamed"); err != nil || string(got) != "in-root" {
			t.Fatalf("Root.ReadFile after failed rename = %q, %v", got, err)
		}

		if err := r.Mkdir("empty", 0o755); err != nil {
			t.Fatalf("Root.Mkdir: %v", err)
		}
		if err := r.Remove("empty"); err != nil {
			t.Fatalf("Root.Remove empty: %v", err)
		}
		if err := r.MkdirAll("trash/a/b", 0o755); err != nil {
			t.Fatalf("Root.MkdirAll trash: %v", err)
		}
		if err := r.WriteFile("trash/a/b/file", []byte("trash"), 0o644); err != nil {
			t.Fatalf("Root.WriteFile trash: %v", err)
		}
		if err := r.RemoveAll("trash"); err != nil {
			t.Fatalf("Root.RemoveAll trash: %v", err)
		}
		mustNotExist("/root/trash")
		mustErrIs(`Root.RemoveAll(".")`, r.RemoveAll("."), syscall.EINVAL)

		if err := os.Rename("/root", "/moved"); err != nil {
			t.Fatalf("Rename root: %v", err)
		}
		if err := os.Mkdir("/root", 0o755); err != nil {
			t.Fatalf("recreate root path: %v", err)
		}
		if err := r.WriteFile("after-rename", []byte("still-rooted"), 0o644); err != nil {
			t.Fatalf("Root.WriteFile after rename: %v", err)
		}
		if got, err := os.ReadFile("/moved/after-rename"); err != nil || string(got) != "still-rooted" {
			t.Fatalf("ReadFile after root rename = %q, %v", got, err)
		}
		mustNotExist("/root/after-rename")
		if got, err := sub.ReadFile("dir/renamed"); err != nil || string(got) != "in-root" {
			t.Fatalf("nested Root.ReadFile after parent rename = %q, %v", got, err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("Root.Close: %v", err)
		}
		var pe *os.PathError
		if _, err := r.Open("sub/dir/renamed"); !errors.Is(err, os.ErrClosed) || !errors.As(err, &pe) || pe.Op != "openat" {
			t.Fatalf("Root.Open after Close = %v, want openat PathError wrapping ErrClosed", err)
		}
		pe = nil
		if err := r.Chown("sub/dir/renamed", 1, 1); !errors.Is(err, os.ErrClosed) || !errors.As(err, &pe) || pe.Op != "chownat" {
			t.Fatalf("Root.Chown after Close = %v, want chownat PathError wrapping ErrClosed", err)
		}
		var le *os.LinkError
		if err := r.Link("sub/dir/renamed", "linked"); !errors.Is(err, os.ErrClosed) || !errors.As(err, &le) || le.Op != "linkat" {
			t.Fatalf("Root.Link after Close = %v, want linkat LinkError wrapping ErrClosed", err)
		}
	})
}

// TestDSTFSOpenRootRenameHostMatrix pins the simulated rooted-rename surface
// to the host os.Root.Rename matrix, row for row (host-probed on ext4 and
// tmpfs, which agree; the host implementation is Go's own openat walk with
// the portable existing-directory-newname EEXIST preamble, NOT raw
// renameat(2) — see design.md, the os.OpenRoot paragraph). Population per
// row: files f1,f2; empty dirs de,de2; nonempty dirs dn,dn2 (a child file
// inside); nested sub/inside.
func TestDSTFSOpenRootRenameHostMatrix(t *testing.T) {
	type row struct {
		old, new string
		want     error // nil = success; errEscape = path-escape refusal
	}
	errEscape := errors.New("path escapes")
	rows := []row{
		// file sources
		{"f1", "missing", nil}, {"f1", "missing/", syscall.ENOTDIR},
		{"f1", "f2", nil}, {"f1", "f2/", syscall.ENOTDIR},
		{"f1", "de", syscall.EEXIST}, {"f1", "de/", syscall.ENOTDIR},
		{"f1", "dn", syscall.EEXIST}, {"f1", "dn/", syscall.ENOTDIR},
		{"f1", "f1", nil}, {"f1", "f1/", syscall.ENOTDIR}, {"f1", "./f1", nil},
		// dir sources (empty)
		{"de", "missing", nil}, {"de", "missing/", nil},
		{"de", "f2", syscall.ENOTDIR}, {"de", "f2/", syscall.ENOTDIR},
		{"de", "de2", syscall.EEXIST}, {"de", "de2/", syscall.EEXIST},
		{"de", "dn", syscall.EEXIST}, {"de", "dn/", syscall.EEXIST},
		{"de", "de", syscall.EEXIST}, {"de", "de/", syscall.EEXIST},
		{"de", "./de", syscall.EEXIST}, {"./de", "de", syscall.EEXIST},
		{"de/", "missing", nil}, {"de/", "de2", syscall.EEXIST},
		// dir sources (nonempty): the preamble's EEXIST, never raw
		// rename(2)'s ENOTEMPTY, through a Root
		{"dn", "missing", nil}, {"dn", "missing/", nil},
		{"dn", "de", syscall.EEXIST}, {"dn", "de/", syscall.EEXIST},
		{"dn", "dn2", syscall.EEXIST}, {"dn", "dn2/", syscall.EEXIST},
		{"dn", "dn", syscall.EEXIST}, {"dn", "dn/", syscall.EEXIST},
		// missing sources
		{"missing", "missing2", syscall.ENOENT}, {"missing", "missing2/", syscall.ENOENT},
		{"missing", "f2", syscall.ENOENT},
		// the rooted ordering: the new walk's slash check on a file
		// answers ENOTDIR before the old final's existence (the un-rooted
		// surface answers ENOENT here)
		{"missing", "f2/", syscall.ENOTDIR},
		{"missing", "de", syscall.ENOENT}, {"missing", "de/", syscall.ENOENT},
		{"missing", "dn", syscall.ENOENT}, {"missing", "dn/", syscall.ENOENT},
		{"missing/", "missing2", syscall.ENOENT}, {"missing/", "f2", syscall.ENOENT},
		{"missing/", "de", syscall.ENOENT},
		// old trailing slash on a file: the old walk's slash assertion
		{"f1/", "missing", syscall.ENOTDIR}, {"f1/", "de", syscall.ENOTDIR},
		{"f1/", "f2", syscall.ENOTDIR},
		// dot finals
		{".", "de", syscall.EEXIST}, {".", "missing", syscall.EBUSY},
		{"de", ".", syscall.EEXIST}, {"de", "./", syscall.EEXIST},
		{"f1", ".", syscall.EEXIST},
		{"de/.", "missing", syscall.EBUSY}, {"de", "de2/.", syscall.EEXIST},
		// same node with a dot-final on exactly one side: the preamble's
		// equal-names/SameFile escape falls through to renameat's EBUSY;
		// dot on both sides keeps equal finals and stays EEXIST
		{"de", "de/.", syscall.EBUSY}, {"de/.", "de", syscall.EBUSY},
		{"de/.", "de/.", syscall.EEXIST}, {"de", "./de/.", syscall.EBUSY},
		{"sub/inside/..", "sub", syscall.EEXIST},
		{"sub/inside/..", "renamed-sub", nil},
		{".", "f2/", syscall.ENOTDIR}, {".", "missing/", syscall.EBUSY},
		{".", "f2", syscall.EBUSY}, {".", "missing/x", syscall.ENOENT},
		{"de/.", "f2/", syscall.ENOTDIR}, {"de/.", "missing/", syscall.EBUSY},
		{"de/.", "de2", syscall.EEXIST},
		{"de/..", "missing", syscall.EBUSY}, {"de/..", "de2", syscall.EEXIST},
		{"missing", "de/.", syscall.ENOENT}, {"missing", ".", syscall.ENOENT},
		{"de", "de2/..", syscall.EEXIST}, {"f1", "de/../f2", nil},
		{"f1", "de/..", syscall.EEXIST},
		// containment: the preamble answers an existing-dir target first,
		// EINVAL only for a missing target inside the source
		{"sub", "sub/inside/x", syscall.EINVAL}, {"sub", "sub/x", syscall.EINVAL},
		{"sub", "sub/inside", syscall.EEXIST}, {"sub", "sub/inside/", syscall.EEXIST},
		{"sub/inside", "sub/inside/", syscall.EEXIST}, {"sub/inside", "sub", syscall.EEXIST},
		// intermediates through files / missing components
		{"f1", "f2/x", syscall.ENOTDIR}, {"f2/x", "f1", syscall.ENOTDIR},
		{"f1", "missing/x", syscall.ENOENT}, {"missing/x", "f1", syscall.ENOENT},
		// escapes, both sides, lexical and walked
		{"f1", "../x", errEscape}, {"../x", "f1", errEscape},
		{"f1", "/abs", errEscape}, {"/abs", "f1", errEscape},
		{"sub/../../x", "f1", errEscape}, {"f1", "sub/../../x", errEscape},
		{".", "../x", errEscape},
	}
	simulation.Run(1, func() {
		for i, row := range rows {
			dir := fmt.Sprintf("/matrix-%d", i)
			if err := os.MkdirAll(dir+"/sub/inside", 0o755); err != nil {
				t.Fatalf("row %d: population: %v", i, err)
			}
			for _, f := range []string{"f1", "f2", "dn/child", "dn2/child"} {
				if err := os.MkdirAll(filepath.Dir(dir+"/"+f), 0o755); err != nil {
					t.Fatalf("row %d: population: %v", i, err)
				}
				if err := os.WriteFile(dir+"/"+f, []byte(f), 0o644); err != nil {
					t.Fatalf("row %d: population: %v", i, err)
				}
			}
			for _, d := range []string{"de", "de2"} {
				if err := os.Mkdir(dir+"/"+d, 0o755); err != nil {
					t.Fatalf("row %d: population: %v", i, err)
				}
			}
			r, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatalf("row %d: OpenRoot: %v", i, err)
			}
			err = r.Rename(row.old, row.new)
			switch want := row.want; {
			case want == nil:
				if err != nil {
					t.Errorf("Root.Rename(%q, %q) = %v, want success (host-probed)", row.old, row.new, err)
				}
			case want == errEscape:
				if err == nil || !strings.Contains(err.Error(), "path escapes") {
					t.Errorf("Root.Rename(%q, %q) = %v, want path-escape refusal (host-probed)", row.old, row.new, err)
				}
			default:
				if !errors.Is(err, want) {
					t.Errorf("Root.Rename(%q, %q) = %v, want %v (host-probed)", row.old, row.new, err, want)
				}
			}
			r.Close()
		}
	})
}

func TestDSTFSOpenRootAdditionalEdges(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.Mkdir("/root", 0o755); err != nil {
			t.Fatalf("Mkdir root: %v", err)
		}
		if err := os.WriteFile("/root/file", []byte("data"), 0o644); err != nil {
			t.Fatalf("WriteFile file: %v", err)
		}
		r, err := os.OpenRoot("/root")
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		defer r.Close()

		if _, err := r.OpenRoot("file"); err == nil {
			t.Fatalf("Root.OpenRoot on file succeeded")
		}
		if _, err := r.OpenRoot("missing"); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("Root.OpenRoot missing = %v, want ENOENT", err)
		}
		if _, err := r.OpenRoot("/root"); err == nil {
			t.Fatalf("Root.OpenRoot absolute path succeeded")
		}

		fi, err := r.Stat("file")
		if err != nil {
			t.Fatalf("Root.Stat file: %v", err)
		}
		lfi, err := r.Lstat("file")
		if err != nil {
			t.Fatalf("Root.Lstat file: %v", err)
		}
		if !os.SameFile(fi, lfi) {
			t.Fatalf("Root.Stat/Lstat SameFile = false, want true")
		}

		if err := r.WriteFile("append", []byte("a"), 0o644); err != nil {
			t.Fatalf("Root.WriteFile append: %v", err)
		}
		f, err := r.OpenFile("append", os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("Root.OpenFile append: %v", err)
		}
		if _, err := f.Write([]byte("b")); err != nil {
			t.Fatalf("append Write: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("append Close: %v", err)
		}
		if got, err := r.ReadFile("append"); err != nil || string(got) != "ab" {
			t.Fatalf("Root.ReadFile append = %q, %v; want ab", got, err)
		}
		f, err = r.OpenFile("append", os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			t.Fatalf("Root.OpenFile truncate: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("truncate Close: %v", err)
		}
		if got, err := r.ReadFile("append"); err != nil || len(got) != 0 {
			t.Fatalf("Root.ReadFile after truncate = %q, %v; want empty", got, err)
		}

	})
}

func TestDSTFSOpenRootDurability(t *testing.T) {
	simulation.Run(1, func() {
		must := func(what string, err error) {
			t.Helper()
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
		state := func(name string) (curData, syncedData, curEntries, syncedEntries string, mode, syncedMode os.FileMode) {
			t.Helper()
			cur, synced, ce, se, m, sm, ok := os.DSTFSNodeState(name)
			if !ok {
				t.Fatalf("DSTFSNodeState(%q) not ok", name)
			}
			return cur, synced, strings.Join(ce, ","), strings.Join(se, ","), m, sm
		}
		syncRoot := func(r *os.Root, name string) {
			t.Helper()
			f, err := r.Open(name)
			must("Root.Open "+name, err)
			must("File.Sync "+name, f.Sync())
			must("File.Close "+name, f.Close())
		}

		must("Mkdir root", os.Mkdir("/root", 0o755))
		r, err := os.OpenRoot("/root")
		must("OpenRoot", err)
		defer r.Close()

		must("Root.Mkdir durability", r.Mkdir("durability", 0o755))
		syncRoot(r, ".")
		_, _, rootCur, rootSynced, _, _ := state("/root")
		if rootCur != "durability" || rootSynced != "durability" {
			t.Fatalf("/root entries = current %q synced %q, want durability/durability", rootCur, rootSynced)
		}
		must("Root.Mkdir mkbase", r.Mkdir("mkbase", 0o755))
		syncRoot(r, ".")
		syncRoot(r, "mkbase")
		must("Root.MkdirAll mkbase/a/b", r.MkdirAll("mkbase/a/b", 0o755))
		_, _, mkCur, mkSynced, _, _ := state("/root/mkbase")
		if mkCur != "a" || mkSynced != "" {
			t.Fatalf("mkbase entries after MkdirAll = current %q synced %q, want a/empty", mkCur, mkSynced)
		}
		must("Root.RemoveAll mkbase", r.RemoveAll("mkbase"))
		_, _, rootCur, rootSynced, _, _ = state("/root")
		if rootCur != "durability" || rootSynced != "durability,mkbase" {
			t.Fatalf("/root entries after mkbase RemoveAll = current %q synced %q, want durability/durability,mkbase", rootCur, rootSynced)
		}
		syncRoot(r, "durability")

		must("Root.WriteFile durability/file", r.WriteFile("durability/file", []byte("data"), 0o644))
		cur, synced, _, _, _, _ := state("/root/durability/file")
		if cur != "data" || synced != "" {
			t.Fatalf("file data = current %q synced %q, want data/empty", cur, synced)
		}
		_, _, dirCur, dirSynced, _, _ := state("/root/durability")
		if dirCur != "file" || dirSynced != "" {
			t.Fatalf("durability entries after create = current %q synced %q, want file/empty", dirCur, dirSynced)
		}

		f, err := r.OpenFile("durability/file", os.O_RDWR, 0)
		must("Root.OpenFile durability/file", err)
		must("file Sync", f.Sync())
		must("file Close", f.Close())
		must("Root.WriteFile durability/trunc", r.WriteFile("durability/trunc", []byte("keep"), 0o644))
		f, err = r.OpenFile("durability/trunc", os.O_RDWR, 0)
		must("Root.OpenFile durability/trunc", err)
		must("trunc Sync", f.Sync())
		must("trunc Close", f.Close())
		f, err = r.OpenFile("durability/trunc", os.O_WRONLY|os.O_TRUNC, 0)
		must("Root.OpenFile durability/trunc O_TRUNC", err)
		must("trunc Close after O_TRUNC", f.Close())
		cur, synced, _, _, _, _ = state("/root/durability/trunc")
		if cur != "" || synced != "keep" {
			t.Fatalf("trunc data = current %q synced %q, want empty/keep", cur, synced)
		}
		must("Root.WriteFile durability/timefile", r.WriteFile("durability/timefile", []byte("time"), 0o644))
		f, err = r.OpenFile("durability/timefile", os.O_RDWR, 0)
		must("Root.OpenFile durability/timefile", err)
		must("timefile Sync", f.Sync())
		must("timefile Close", f.Close())
		_, syncedModTime0, ok := os.DSTFSNodeTimes("/root/durability/timefile")
		if !ok {
			t.Fatalf("DSTFSNodeTimes(timefile) not ok")
		}
		mtime := time.Unix(456, 0)
		must("Root.Chtimes durability/timefile", r.Chtimes("durability/timefile", time.Time{}, mtime))
		modTime1, syncedModTime1, ok := os.DSTFSNodeTimes("/root/durability/timefile")
		if !ok {
			t.Fatalf("DSTFSNodeTimes(timefile after Chtimes) not ok")
		}
		if modTime1 != mtime.UnixNano() || syncedModTime1 != syncedModTime0 {
			t.Fatalf("timefile times after Chtimes = mod %d synced %d, want %d/%d", modTime1, syncedModTime1, mtime.UnixNano(), syncedModTime0)
		}
		syncRoot(r, "durability")
		must("Root.Chmod durability/file", r.Chmod("durability/file", 0o600))
		_, synced, _, _, mode, syncedMode := state("/root/durability/file")
		if synced != "data" || mode.Perm() != 0o600 || syncedMode.Perm() != 0o644 {
			t.Fatalf("after chmod = synced data %q mode %v syncedMode %v, want data 0600 0644", synced, mode.Perm(), syncedMode.Perm())
		}

		must("Root.Rename durability/file", r.Rename("durability/file", "durability/renamed"))
		_, _, dirCur, dirSynced, _, _ = state("/root/durability")
		if dirCur != "renamed,timefile,trunc" || dirSynced != "file,timefile,trunc" {
			t.Fatalf("durability entries after rename = current %q synced %q, want renamed,timefile,trunc/file,timefile,trunc", dirCur, dirSynced)
		}
		must("Root.Remove durability/renamed", r.Remove("durability/renamed"))
		_, _, dirCur, dirSynced, _, _ = state("/root/durability")
		if dirCur != "timefile,trunc" || dirSynced != "file,timefile,trunc" {
			t.Fatalf("durability entries after remove = current %q synced %q, want timefile,trunc/file,timefile,trunc", dirCur, dirSynced)
		}
		must("Root.RemoveAll durability", r.RemoveAll("durability"))
		_, _, rootCur, rootSynced, _, _ = state("/root")
		if rootCur != "" || rootSynced != "durability,mkbase" {
			t.Fatalf("/root entries after RemoveAll = current %q synced %q, want empty/durability,mkbase", rootCur, rootSynced)
		}
	})
}

// TestDSTFSDurabilityMonotonicity is the enforcement of the spec's
// durability-monotonicity invariant, promoted from spec tier at this chunk:
// every mutation enters the tree as unsynced (the durable image is
// untouched), and sync alone advances the durable boundary — for file
// content, file metadata, and directory entry sets. A future simulated crash
// restores exactly the durable image, so a mutation that advanced it
// directly would later let torn-write faults tear "durable" state — a
// soundness violation. The white-box inspector (os.DSTFSNodeState) is the
// only observer until the fault feature lands.
func TestDSTFSDurabilityMonotonicity(t *testing.T) {
	simulation.Run(1, func() {
		state := func(name string) (cur, synced string, ce, se []string) {
			cur, synced, ce, se, _, _, ok := os.DSTFSNodeState(name)
			if !ok {
				t.Fatalf("DSTFSNodeState(%q) not ok", name)
			}
			return cur, synced, ce, se
		}
		names := func(l []string) string { return strings.Join(l, ",") }
		must := func(what string, err error) {
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}

		// File content: write -> unsynced; Sync -> committed; further
		// mutation (write, truncate, O_TRUNC reopen) leaves the image.
		f, err := os.Create("/f")
		must("Create", err)
		_, err = f.WriteString("abc")
		must("WriteString", err)
		if cur, synced, _, _ := state("/f"); cur != "abc" || synced != "" {
			t.Fatalf("post-write state = %q/%q, want abc/empty (mutation advanced the durable image)", cur, synced)
		}
		must("Sync", f.Sync())
		if _, synced, _, _ := state("/f"); synced != "abc" {
			t.Fatalf("post-sync image = %q, want abc", synced)
		}
		// In-place overwrite of the synced range: the image must be a COPY,
		// not an alias of the live buffer (the aliasing-mutant kill).
		_, err = f.WriteAt([]byte("XYZ"), 0)
		must("WriteAt", err)
		if cur, synced, _, _ := state("/f"); cur != "XYZ" || synced != "abc" {
			t.Fatalf("post-overwrite = %q/%q (durable image aliases the live buffer)", cur, synced)
		}
		_, err = f.WriteString("def") // appends at offset 3 (WriteAt does not move it)
		must("WriteString-2", err)
		if cur, synced, _, _ := state("/f"); cur != "XYZdef" || synced != "abc" {
			t.Fatalf("post-write-2 = %q/%q", cur, synced)
		}
		must("Truncate", f.Truncate(1))
		if cur, synced, _, _ := state("/f"); cur != "X" || synced != "abc" {
			t.Fatalf("post-truncate = %q/%q", cur, synced)
		}
		f.Close()
		ft, err := os.OpenFile("/f", os.O_WRONLY|os.O_TRUNC, 0)
		must("O_TRUNC reopen", err)
		if cur, synced, _, _ := state("/f"); cur != "" || synced != "abc" {
			t.Fatalf("post-O_TRUNC = %q/%q", cur, synced)
		}
		ft.Close()

		// Truncate-down then extend: the gap is zeros, never resurrected
		// bytes (the stale-byte regression: backing arrays are reused).
		g, err := os.Create("/g")
		must("Create g", err)
		_, err = g.WriteString("abcdefgh")
		must("write g", err)
		must("truncate g", g.Truncate(2))
		_, err = g.WriteAt([]byte("Z"), 4)
		must("extend g", err)
		if cur, _, _, _ := state("/g"); cur != "ab\x00\x00Z" {
			t.Fatalf("truncate-extend content = %q, want ab\\x00\\x00Z (stale bytes resurrected)", cur)
		}
		g.Close()

		// Directory entries: create/remove/rename are unsynced until the
		// DIRECTORY is synced; the synced set is a snapshot by NAME, not an
		// alias of the live map.
		must("Mkdir", os.Mkdir("/d", 0o755))
		must("WriteFile one", os.WriteFile("/d/one", nil, 0o644))
		must("WriteFile two", os.WriteFile("/d/two", nil, 0o644))
		if _, _, ce, se := state("/d"); names(ce) != "one,two" || len(se) != 0 {
			t.Fatalf("dir pre-sync = %v/%v", ce, se)
		}
		dh, err := os.Open("/d")
		must("open dir", err)
		must("dir Sync", dh.Sync())
		if _, _, ce, se := state("/d"); names(ce) != "one,two" || names(se) != "one,two" {
			t.Fatalf("dir post-sync = %v/%v", ce, se)
		}
		must("Remove", os.Remove("/d/one"))
		if _, _, ce, se := state("/d"); names(ce) != "two" || names(se) != "one,two" {
			t.Fatalf("dir post-remove = %v/%v (synced set aliases the live map)", ce, se)
		}
		must("WriteFile three", os.WriteFile("/d/three", nil, 0o644))
		if _, _, ce, se := state("/d"); names(ce) != "three,two" || names(se) != "one,two" {
			t.Fatalf("dir post-create = %v/%v", ce, se)
		}
		// Rename mutates the live namespace only.
		must("Rename", os.Rename("/d/two", "/d/renamed"))
		if _, _, ce, se := state("/d"); names(ce) != "renamed,three" || names(se) != "one,two" {
			t.Fatalf("dir post-rename = %v/%v (rename advanced the durable set)", ce, se)
		}
		dh.Close()

		// Metadata image: committed by sync alone (commitLocked's mode copy).
		_, _, _, _, mode, syncedMode, _ := os.DSTFSNodeState("/f")
		if mode == 0 || syncedMode != mode {
			t.Fatalf("syncedMode = %v, mode = %v (sync did not commit metadata)", syncedMode, mode)
		}

		// O_SYNC: a WRITE THAT WROTE (n > 0) commits through the single
		// commit point (a zero-length or fully-refused write commits
		// nothing — TestDSTDiskOSyncZeroWriteDoesNotCommit); ftruncate is NOT
		// covered (POSIX synchronized-I/O is for writes — O_SYNC-truncate
		// durability would be finer than real disks grant).
		sf, err := os.OpenFile("/s", os.O_CREATE|os.O_WRONLY|os.O_SYNC, 0o644)
		must("O_SYNC open", err)
		_, err = sf.WriteString("123")
		must("O_SYNC write", err)
		if cur, synced, _, _ := state("/s"); cur != "123" || synced != "123" {
			t.Fatalf("O_SYNC write = %q/%q, want 123/123", cur, synced)
		}
		// The pwrite path (WriteAt) commits too — "per write" means both
		// write funnels through the single commit point.
		_, err = sf.WriteAt([]byte("9"), 0)
		must("O_SYNC WriteAt", err)
		if cur, synced, _, _ := state("/s"); cur != "923" || synced != "923" {
			t.Fatalf("O_SYNC WriteAt = %q/%q, want 923/923", cur, synced)
		}
		must("O_SYNC truncate", sf.Truncate(1))
		if cur, synced, _, _ := state("/s"); cur != "9" || synced != "923" {
			t.Fatalf("O_SYNC truncate = %q/%q, want 9/923 (truncate must not commit)", cur, synced)
		}
		sf.Close()
	})
}

// TestDSTFSMetadata covers the chunk-4 named metadata ops: Truncate(name)
// with truncate(2) shapes, Chmod (named and handle) changing exactly the
// changeable bits, Chtimes with the zero-time leave-unchanged contract, and
// the monotonicity extension — metadata mutations never advance the durable
// metadata image.
func TestDSTFSMetadata(t *testing.T) {
	simulation.Run(1, func() {
		must := func(what string, err error) {
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
		must("WriteFile", os.WriteFile("/m", []byte("0123456789"), 0o644))

		// Named truncate.
		must("Truncate", os.Truncate("/m", 4))
		if fi, _ := os.Stat("/m"); fi.Size() != 4 {
			t.Fatalf("post-truncate size = %d", fi.Size())
		}
		if err := os.Truncate("/m", -1); !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("negative truncate = %v, want EINVAL", err)
		}
		if err := os.Truncate("/missing", 0); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("truncate missing = %v, want ENOENT", err)
		}
		os.Mkdir("/md", 0o755)
		if err := os.Truncate("/md", 0); !errors.Is(err, syscall.EISDIR) {
			t.Fatalf("truncate dir = %v, want EISDIR", err)
		}
		// Truncate-up zero-extends.
		must("Truncate up", os.Truncate("/m", 8))
		got, _ := os.ReadFile("/m")
		if string(got) != "0123\x00\x00\x00\x00" {
			t.Fatalf("truncate-up content = %q", got)
		}

		// Chmod: changeable bits only; type bits preserved (dir stays dir).
		must("Chmod", os.Chmod("/m", 0o600|os.ModeSetuid))
		if fi, _ := os.Stat("/m"); fi.Mode() != 0o600|os.ModeSetuid {
			t.Fatalf("Chmod mode = %v", fi.Mode())
		}
		must("Chmod dir", os.Chmod("/md", 0o700))
		if fi, _ := os.Stat("/md"); fi.Mode() != os.ModeDir|0o700 || !fi.IsDir() {
			t.Fatalf("dir Chmod mode = %v", fi.Mode())
		}
		if err := os.Chmod("/missing", 0o644); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("Chmod missing = %v", err)
		}

		// Chtimes: explicit mtime sets; zero mtime leaves unchanged.
		stamp := time.Now().Add(-3 * time.Hour)
		must("Chtimes", os.Chtimes("/m", time.Time{}, stamp))
		if fi, _ := os.Stat("/m"); !fi.ModTime().Equal(stamp) {
			t.Fatalf("Chtimes mtime = %v, want %v", fi.ModTime(), stamp)
		}
		must("Chtimes zero", os.Chtimes("/m", time.Time{}, time.Time{}))
		if fi, _ := os.Stat("/m"); !fi.ModTime().Equal(stamp) {
			t.Fatalf("zero Chtimes changed mtime: %v", fi.ModTime())
		}

		// Monotonicity: metadata mutations enter unsynced.
		f, _ := os.Open("/m")
		must("Sync", f.Sync())
		f.Close()
		_, _, _, _, mode0, syncedMode0, _ := os.DSTFSNodeState("/m")
		if syncedMode0 != mode0 {
			t.Fatalf("post-sync metadata image = %v vs %v", syncedMode0, mode0)
		}
		must("Chmod after sync", os.Chmod("/m", 0o400))
		_, _, _, _, mode1, syncedMode1, _ := os.DSTFSNodeState("/m")
		if mode1 != 0o400 || syncedMode1 != syncedMode0 {
			t.Fatalf("metadata mutation advanced the durable image: %v/%v", mode1, syncedMode1)
		}
		// Chtimes after sync: modTime moves, the durable stamp does not.
		_, smt0, _ := os.DSTFSNodeTimes("/m")
		must("Chtimes after sync", os.Chtimes("/m", time.Time{}, stamp.Add(time.Hour)))
		mt1, smt1, _ := os.DSTFSNodeTimes("/m")
		if mt1 == smt1 || smt1 != smt0 {
			t.Fatalf("Chtimes advanced the durable stamp: mt=%d smt=%d smt0=%d", mt1, smt1, smt0)
		}
		// Named Truncate after sync: content image untouched (the handle
		// path is pinned in the durability test; this pins the wrapper).
		_, synced0, _, _, _, _, _ := os.DSTFSNodeState("/m")
		must("named Truncate after sync", os.Truncate("/m", 2))
		cur1, synced1, _, _, _, _, _ := os.DSTFSNodeState("/m")
		if len(cur1) != 2 || synced1 != synced0 {
			t.Fatalf("named truncate advanced the durable image: %q vs %q", synced1, synced0)
		}

		// Chmod does not touch mtime (chmod(2) updates ctime only). The
		// virtual sleep advances the bubble clock first — without it a
		// mutant stamping time.Now() writes the same frozen instant and
		// the probe is vacuous.
		time.Sleep(time.Second)
		mtPre, _, _ := os.DSTFSNodeTimes("/m")
		time.Sleep(time.Second)
		must("Chmod mtime probe", os.Chmod("/m", 0o644))
		mtPost, _, _ := os.DSTFSNodeTimes("/m")
		if mtPre != mtPost {
			t.Fatalf("Chmod moved mtime: %d -> %d", mtPre, mtPost)
		}
	})
}

// TestDSTFSTempDir: os.TempDir reports the fixed simulated /tmp during a
// run (never the host's machine-dependent $TMPDIR string), /tmp is
// pre-seeded (mode 1777), CreateTemp/MkdirTemp work unmodified, and their
// seeded random names replay identically across same-seed runs.
func TestDSTFSTempDir(t *testing.T) {
	// Pin TMPDIR to a non-/tmp host path so the in-run assertion has teeth:
	// with TMPDIR unset the host default is also "/tmp" and a missing
	// TempDir gate would pass vacuously.
	t.Setenv("TMPDIR", t.TempDir())
	hostTmp := os.TempDir()
	if hostTmp == "/tmp" {
		t.Fatalf("test harness: host TempDir still /tmp despite TMPDIR pin")
	}
	name := func(seed uint64) (got string) {
		simulation.Run(seed, func() {
			if td := os.TempDir(); td != "/tmp" {
				t.Fatalf("TempDir under run = %q, want /tmp", td)
			}
			fi, err := os.Stat("/tmp")
			if err != nil || !fi.IsDir() || fi.Mode() != os.ModeDir|os.ModeSticky|0o777 {
				t.Fatalf("/tmp = %v %v, %v", fi.Mode(), fi.IsDir(), err)
			}
			f, err := os.CreateTemp("", "dst-*")
			if err != nil {
				t.Fatalf("CreateTemp: %v", err)
			}
			if !strings.HasPrefix(f.Name(), "/tmp/dst-") {
				t.Fatalf("CreateTemp name = %q", f.Name())
			}
			got = f.Name()
			f.Close()
			d, err := os.MkdirTemp("", "dstdir-*")
			if err != nil {
				t.Fatalf("MkdirTemp: %v", err)
			}
			if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
				t.Fatalf("MkdirTemp result: %v, %v", fi, err)
			}
		})
		return got
	}
	a, b := name(11), name(11)
	if a != b {
		t.Fatalf("CreateTemp names differ across same-seed runs: %q vs %q", a, b)
	}
	if os.TempDir() != hostTmp {
		t.Fatalf("host TempDir changed outside run: %q vs %q", os.TempDir(), hostTmp)
	}
}

// TestDSTFSMixedHandleCopy: io.Copy pairing a simulated source with an
// explicitly inherited host destination takes the generic loop (the mixed-handle
// rule); the zero-copy fast paths must bail on the simulated side seen
// through wrappers (fileWithoutWriteTo, LimitedReader), not only bare *File.
func TestDSTFSMixedHandleCopy(t *testing.T) {
	host, err := os.CreateTemp(t.TempDir(), "dst-mixed-*")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	simulation.Run(1, func() {
		dst, err := simulation.InheritFile(host)
		if err != nil {
			t.Fatalf("InheritFile: %v", err)
		}
		defer dst.Close()
		src, _ := os.Create("/payload")
		src.WriteString("across-the-boundary")
		src.Seek(0, io.SeekStart)
		if n, err := io.Copy(dst, src); n != 19 || err != nil {
			t.Fatalf("io.Copy(host, sim) = %d, %v (zero-copy fast path leaked through?)", n, err)
		}
		src.Seek(0, io.SeekStart)
		if n, err := io.CopyN(dst, src, 6); n != 6 || err != nil {
			t.Fatalf("io.CopyN(host, sim, 6) = %d, %v", n, err)
		}
		src.Close()
	})
	got, err := os.ReadFile(host.Name())
	if err != nil || string(got) != "across-the-boundaryacross" {
		t.Fatalf("host file = %q, %v", got, err)
	}
}

// TestDSTFSHostIsolation: under a run, path resolution happens in the
// simulated tree, never on the host. A path whose parent directory EXISTS on
// the host (t.TempDir()) must fail ENOENT inside the run — the simulated tree
// is empty by contract — and nothing may appear on the host afterwards. A
// root-level file created in-sim likewise never lands on the host.
func TestDSTFSHostIsolation(t *testing.T) {
	hostDir := t.TempDir()
	probe := filepath.Join(hostDir, "dst-host-isolation-probe")
	simulation.Run(1, func() {
		// The host parent exists; the simulated tree has no such directory.
		// ENOENT here proves resolution is in-sim.
		if _, err := os.Create(probe); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("Create(%q) under run = %v, want ENOENT against the empty simulated tree", probe, err)
		}
		f, err := os.Create("/dst-host-isolation-probe")
		if err != nil {
			t.Fatalf("Create(/...) under run: %v", err)
		}
		if _, err := f.Write([]byte("simulated")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		f.Close()
	})
	if _, err := os.Stat(probe); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host Stat(%q) = %v, want not-exist: the simulated write leaked to the host", probe, err)
	}
	if _, err := os.Stat("/dst-host-isolation-probe"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host root probe exists: the simulated write leaked to the host")
	}
}

// TestDSTFSEpochReset: each run starts with a fresh empty tree.
func TestDSTFSEpochReset(t *testing.T) {
	simulation.Run(1, func() {
		f, err := os.Create("/persist")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		f.Close()
	})
	simulation.Run(1, func() {
		if _, err := os.Open("/persist"); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("second run Open = %v, want ENOENT (stale tree leaked across runs)", err)
		}
	})
}

// TestDSTFSReplay: the same seed produces the identical observation sequence
// from concurrent file users — content interleaving included.
func TestDSTFSReplay(t *testing.T) {
	transcript := func(seed uint64) string {
		var out string
		simulation.Run(seed, func() {
			f, err := os.OpenFile("/log", os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			var wg sync.WaitGroup
			for g := 0; g < 4; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := 0; i < 4; i++ {
						fmt.Fprintf(f, "[g%d:%d]", g, i)
					}
				}(g)
			}
			wg.Wait()
			buf := make([]byte, 1024)
			n, err := f.ReadAt(buf, 0)
			if err != nil && err != io.EOF {
				t.Fatalf("ReadAt: %v", err)
			}
			out = string(buf[:n])
			f.Close()
		})
		return out
	}

	a, b := transcript(7), transcript(7)
	if a != b {
		t.Fatalf("same seed, different transcripts:\n  %q\n  %q", a, b)
	}
	if len(a) != 4*4*len("[g0:0]") {
		t.Fatalf("transcript length %d, want %d (lost or duplicated writes)", len(a), 4*4*len("[g0:0]"))
	}
}

// TestDSTRootLeakedAcrossRuns: a *Root leaked across a run boundary — and any
// file minted through it — is refused deterministically, like a leaked *File:
// never a nil deref, never a read of the prior run's tree, on BOTH sides of
// the next run's first filesystem op (which is when the prior run's nodes are
// released). Under the bug, an op ordered before that release silently read
// run 1's un-released tree, and one ordered after it SIGSEGV'd on the
// released page cache — run-2 observable state depending on op ordering.
func TestDSTRootLeakedAcrossRuns(t *testing.T) {
	var leakedRoot *os.Root
	var leakedFile *os.File
	simulation.Run(1, func() {
		if err := os.Mkdir("/d", 0o755); err != nil {
			t.Errorf("mkdir: %v", err)
			return
		}
		if err := os.WriteFile("/d/f", []byte("run1"), 0o644); err != nil {
			t.Errorf("write: %v", err)
			return
		}
		r, err := os.OpenRoot("/d")
		if err != nil {
			t.Errorf("OpenRoot: %v", err)
			return
		}
		leakedRoot = r
		f, err := r.OpenFile("f", os.O_RDWR, 0)
		if err != nil {
			t.Errorf("root OpenFile: %v", err)
			return
		}
		leakedFile = f
	})
	if t.Failed() {
		return
	}
	// BETWEEN runs (no run active, epoch reads 0): already refused — the tree
	// is not yet physically released, so a pass-through here would silently
	// read run 1's tree with no run active.
	if _, err := leakedRoot.Stat("f"); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("leaked Root between runs = %v, want ErrClosed", err)
	}
	simulation.Run(2, func() {
		// Before the roll (no run-2 fs op yet): refused, not a stale read —
		// with the closed-handle identity, like a leaked *File.
		if _, err := leakedRoot.Stat("f"); !errors.Is(err, os.ErrClosed) {
			t.Errorf("leaked Root before the roll = %v, want ErrClosed", err)
		}
		if _, err := leakedRoot.OpenFile("f", os.O_RDWR, 0); err == nil {
			t.Error("leaked Root minted a file over the prior run's tree")
		}
		// Roll: the first run-2 fs op releases run 1's nodes.
		if _, err := os.Stat("/tmp"); err != nil {
			t.Errorf("stat /tmp: %v", err)
		}
		// After the roll: still refused, and the leaked root-minted file is
		// refused too (a write here SIGSEGV'd on the released page cache).
		if _, err := leakedRoot.Stat("f"); err == nil {
			t.Error("leaked Root usable after the roll")
		}
		if _, err := leakedFile.Write([]byte("x")); err == nil {
			t.Error("leaked root-minted File wrote into a dead run")
		}
		if _, err := leakedFile.Read(make([]byte, 4)); err == nil {
			t.Error("leaked root-minted File read from a dead run")
		}
	})
}

// TestDSTFSDirectorySeek: lseek on a directory fd is permitted at any
// offset, as Linux permits — the offset is an opaque readdir cookie (the
// deterministic sorted-name index here): seeking resumes the listing there,
// and seeking to 0 restarts it (the rewinddir shape).
func TestDSTFSDirectorySeek(t *testing.T) {
	simulation.Run(1, func() {
		for _, name := range []string{"a", "b", "c", "d"} {
			if err := os.WriteFile("/"+name, nil, 0o644); err != nil {
				t.Fatalf("WriteFile %s: %v", name, err)
			}
		}
		dir, err := os.Open("/")
		if err != nil {
			t.Fatalf("Open /: %v", err)
		}
		defer dir.Close()
		all, err := dir.Readdirnames(-1) // the full sorted listing, no hardcoded tree
		if err != nil {
			t.Fatalf("Readdirnames: %v", err)
		}
		if pos, err := dir.Seek(2, io.SeekStart); pos != 2 || err != nil {
			t.Fatalf("Seek(2) on a directory = %d, %v; want 2, nil (Linux permits lseek on a directory fd)", pos, err)
		}
		// telldir: SEEK_CUR reads the cookie without moving it.
		if pos, err := dir.Seek(0, io.SeekCurrent); pos != 2 || err != nil {
			t.Fatalf("telldir Seek(0, CUR) = %d, %v; want 2, nil", pos, err)
		}
		names, err := dir.Readdirnames(-1)
		if err != nil {
			t.Fatalf("Readdirnames after seek: %v", err)
		}
		if !slices.Equal(names, all[2:]) {
			t.Fatalf("listing after Seek(2) = %v, want %v", names, all[2:])
		}
		// SEEK_END: the cookie space ends at the entry count — the listing
		// from there is empty.
		if pos, err := dir.Seek(0, io.SeekEnd); pos != int64(len(all)) || err != nil {
			t.Fatalf("Seek(0, END) = %d, %v; want %d, nil", pos, err, len(all))
		}
		if names, err := dir.Readdirnames(-1); (err != nil && err != io.EOF) || len(names) != 0 {
			t.Fatalf("listing at END = %v, %v; want empty", names, err)
		}
		if pos, err := dir.Seek(0, io.SeekStart); pos != 0 || err != nil {
			t.Fatalf("rewind Seek(0) = %d, %v; want 0, nil", pos, err)
		}
		names, err = dir.Readdirnames(-1)
		if err != nil {
			t.Fatalf("Readdirnames after rewind: %v", err)
		}
		if !slices.Equal(names, all) {
			t.Fatalf("listing after rewind = %v, want %v", names, all)
		}
	})
}

// TestDSTFSReaddirRemovedDirENOENT: getdents on an rmdir'd directory is the
// kernel's dead-directory refusal (iterate_dir's IS_DEADDIR): every listing
// through a handle opened before the removal answers ENOENT — never an empty
// success — while fstat through the same handle keeps working.
func TestDSTFSReaddirRemovedDirENOENT(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.MkdirAll("/dead/sub", 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		dir, err := os.Open("/dead")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer dir.Close()
		if err := os.RemoveAll("/dead"); err != nil {
			t.Fatalf("RemoveAll: %v", err)
		}
		if _, err := dir.Readdirnames(-1); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("Readdirnames on removed dir = %v, want ENOENT", err)
		}
		if _, err := dir.ReadDir(1); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("ReadDir on removed dir = %v, want ENOENT", err)
		}
		if _, err := dir.Stat(); err != nil {
			t.Fatalf("Stat on removed dir handle = %v, want success", err)
		}
	})
}

// TestDSTFSRenameat2 pins the raw SYS_RENAMEAT2 dispatch — the
// x/sys/unix Renameat2 path a database's atomic no-clobber publish
// uses. NOREPLACE refuses an existing target with EEXIST (target
// bytes intact) and the same-node case ahead of the same-file no-op;
// flags=0 replaces like rename(2); EXCHANGE answers EINVAL (the
// unsupported-filesystem shape a caller's degradation ladder expects).
func TestDSTFSRenameat2(t *testing.T) {
	renameat2Num := func() uintptr {
		switch runtime.GOARCH {
		case "amd64":
			return 316
		case "386":
			return 353
		case "arm":
			return 382
		case "arm64", "riscv64":
			return 276
		case "ppc64", "ppc64le":
			return 357
		case "s390x":
			return 347
		}
		t.Fatalf("no renameat2 number for %s", runtime.GOARCH)
		return 0
	}
	simulation.Run(1, func() {
		atFdcwd := -0x64
		r2 := func(old, new string, flags uintptr) syscall.Errno {
			ob, err := syscall.BytePtrFromString(old)
			if err != nil {
				t.Fatal(err)
			}
			nb, err := syscall.BytePtrFromString(new)
			if err != nil {
				t.Fatal(err)
			}
			_, _, e := syscall.Syscall6(renameat2Num(), uintptr(atFdcwd),
				uintptr(unsafe.Pointer(ob)), uintptr(atFdcwd),
				uintptr(unsafe.Pointer(nb)), flags, 0)
			return e
		}
		const noreplace, exchange = 0x1, 0x2
		if err := os.WriteFile("/tmp/a", []byte("copy"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("/tmp/b", []byte("existing"), 0o600); err != nil {
			t.Fatal(err)
		}
		if e := r2("/tmp/a", "/tmp/b", noreplace); e != syscall.EEXIST {
			t.Fatalf("NOREPLACE over existing = %v, want EEXIST", e)
		}
		if got, _ := os.ReadFile("/tmp/b"); string(got) != "existing" {
			t.Fatalf("NOREPLACE clobbered the target: %q", got)
		}
		if e := r2("/tmp/a", "/tmp/a", noreplace); e != syscall.EEXIST {
			t.Fatalf("NOREPLACE same node = %v, want EEXIST (ahead of the no-op)", e)
		}
		if e := r2("/tmp/a", "/tmp/c", noreplace); e != 0 {
			t.Fatalf("NOREPLACE into absent = %v, want success", e)
		}
		if got, _ := os.ReadFile("/tmp/c"); string(got) != "copy" {
			t.Fatalf("publish missing: %q", got)
		}
		if _, err := os.Lstat("/tmp/a"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old name survived the rename: %v", err)
		}
		if e := r2("/tmp/c", "/tmp/b", 0); e != 0 {
			t.Fatalf("flags=0 replace = %v, want success", e)
		}
		if got, _ := os.ReadFile("/tmp/b"); string(got) != "copy" {
			t.Fatalf("flags=0 did not replace: %q", got)
		}
		if e := r2("/tmp/b", "/tmp/x", exchange); e != syscall.EINVAL {
			t.Fatalf("EXCHANGE = %v, want EINVAL", e)
		}
		if e := r2("/tmp/missing", "/tmp/y", noreplace); e != syscall.ENOENT {
			t.Fatalf("missing source = %v, want ENOENT", e)
		}
		// Host-probed kernel order: NOREPLACE's positive-target EEXIST
		// precedes the trailing-slash source-type ENOTDIR, both shapes
		// (both names must exist for the probe to be the ordering probe).
		if err := os.WriteFile("/tmp/c", []byte("back"), 0o600); err != nil {
			t.Fatal(err)
		}
		if e := r2("/tmp/b/", "/tmp/c", noreplace); e != syscall.EEXIST {
			t.Fatalf("NOREPLACE slashed source over existing = %v, want EEXIST", e)
		}
		if e := r2("/tmp/b", "/tmp/c/", noreplace); e != syscall.EEXIST {
			t.Fatalf("NOREPLACE onto slashed existing = %v, want EEXIST", e)
		}
		// The dirfd-relative form is not the simulation's: it meets the
		// raw fence (the clock tests' recover pattern).
		d, err := os.Open("/tmp")
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		ob, _ := syscall.BytePtrFromString("/tmp/b")
		nb, _ := syscall.BytePtrFromString("/tmp/z")
		fencePanic := func() (p any) {
			defer func() { p = recover() }()
			syscall.Syscall6(renameat2Num(), d.Fd(),
				uintptr(unsafe.Pointer(ob)), d.Fd(),
				uintptr(unsafe.Pointer(nb)), noreplace, 0)
			return nil
		}()
		if fencePanic == nil || !strings.Contains(fmt.Sprint(fencePanic), "unsupported under deterministic simulation") {
			t.Fatalf("dirfd-relative renameat2 = %v, want the fence refusal", fencePanic)
		}
	})
}
