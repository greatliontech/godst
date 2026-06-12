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
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
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
			{"truncate", func() error { return os.Truncate("/x", 0) }},
			{"chmod", func() error { return os.Chmod("/x", 0o644) }},
			{"chown", func() error { return os.Chown("/x", 0, 0) }},
			{"lchown", func() error { return os.Lchown("/x", 0, 0) }},
			{"chtimes", func() error { return os.Chtimes("/x", time.Time{}, time.Time{}) }},
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
// metadata round-trips (fake-clock ModTime, stored perm), and os.OpenRoot
// plus the unmodeled handle methods carry the fence shape.
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

		// os.Pipe is fenced until dst-io implements it.
		if _, _, err := os.Pipe(); !isDSTUnsupportedFS(err) {
			t.Fatalf("Pipe = %v, want unsupported-under-simulation", err)
		}

		// Fd panics loud on a simulated file; SyscallConn fails with the
		// fence shape.
		func() {
			defer func() {
				r := recover()
				if r == nil || !strings.Contains(fmt.Sprint(r), "unsupported under deterministic simulation") {
					t.Fatalf("Fd panic = %v, want the unsupported shape", r)
				}
			}()
			dst.Fd()
		}()
		if _, err := dst.SyscallConn(); !isDSTUnsupportedFS(err) {
			t.Fatalf("SyscallConn = %v, want unsupported", err)
		}

		// os.OpenRoot is fenced (the H1 host-leak class).
		if _, err := os.OpenRoot("/tmp"); !isDSTUnsupportedFS(err) {
			t.Fatalf("OpenRoot = %v, want unsupported-under-simulation", err)
		}

		// Readdir on a regular file: the production ENOTDIR shape (the
		// funnel is implemented as of the namespace chunk); the remaining
		// unmodeled handle methods carry the fence shape.
		if _, err := mf.Readdirnames(0); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("Readdirnames on file = %v, want ENOTDIR", err)
		}
		if err := mf.Chmod(0o600); !isDSTUnsupportedFS(err) {
			t.Fatalf("File.Chmod = %v, want unsupported", err)
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
		want := "aa,bb,mm,sub,zz"
		if got := strings.Join(names, ","); got != want {
			t.Fatalf("ReadDir order = %s, want %s", got, want)
		}
		if !ents[3].IsDir() || ents[3].Type()&os.ModeDir == 0 {
			t.Fatalf("DirEntry sub not a dir: %v %v", ents[3].IsDir(), ents[3].Type())
		}

		// Chunked reads: stable cursor, io.EOF at exhaustion.
		dh, err := os.Open("/")
		if err != nil {
			t.Fatalf("Open dir: %v", err)
		}
		n1, err1 := dh.Readdirnames(2)
		n2, err2 := dh.Readdirnames(2)
		n3, err3 := dh.Readdirnames(2)
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

// TestDSTFSMixedHandleCopy: io.Copy pairing a simulated source with a
// pre-run host destination takes the generic loop (the spec's mixed-handle
// rule); the zero-copy fast paths must bail on the simulated side seen
// through wrappers (fileWithoutWriteTo, LimitedReader), not only bare *File.
func TestDSTFSMixedHandleCopy(t *testing.T) {
	host, err := os.CreateTemp(t.TempDir(), "dst-mixed-*")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	simulation.Run(1, func() {
		src, _ := os.Create("/payload")
		src.WriteString("across-the-boundary")
		src.Seek(0, io.SeekStart)
		if n, err := io.Copy(host, src); n != 19 || err != nil {
			t.Fatalf("io.Copy(host, sim) = %d, %v (zero-copy fast path leaked through?)", n, err)
		}
		src.Seek(0, io.SeekStart)
		if n, err := io.CopyN(host, src, 6); n != 6 || err != nil {
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
