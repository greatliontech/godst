// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os_test

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
)

func linkStat(t *testing.T, path string) syscall.Stat_t {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatalf("fstat %s: %v", path, err)
	}
	return st
}

// TestDSTFSHardLinks pins link(2)'s modeled contract: a second dirent
// for the same inode — SameFile identity, shared content through every
// name, live Nlink counts, content surviving until the last link goes,
// and the kernel's error order (host-probed on tmpfs: old walk first,
// EPERM for any directory old, EEXIST beating the new-slash rule,
// ENOENT for a slashed missing new).
func TestDSTFSHardLinks(t *testing.T) {
	simulation.Run(1, func() {
		if err := os.WriteFile("/f", []byte("content"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link("/f", "/g"); err != nil {
			t.Fatalf("Link: %v", err)
		}
		fi1, err := os.Stat("/f")
		if err != nil {
			t.Fatal(err)
		}
		fi2, err := os.Stat("/g")
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(fi1, fi2) {
			t.Fatal("linked names are not SameFile")
		}
		if st1, st2 := linkStat(t, "/f"), linkStat(t, "/g"); st1.Ino != st2.Ino || st1.Nlink != 2 || st2.Nlink != 2 {
			t.Fatalf("fstat identity = ino %d/%d nlink %d/%d, want shared ino, nlink 2", st1.Ino, st2.Ino, st1.Nlink, st2.Nlink)
		}
		// Writes through one name are the other name's bytes.
		if err := os.WriteFile("/g", []byte("rewritten"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile("/f"); string(got) != "rewritten" {
			t.Fatalf("write through /g invisible via /f: %q", got)
		}
		// Removing one name leaves the other whole.
		if err := os.Remove("/f"); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile("/g"); string(got) != "rewritten" {
			t.Fatalf("content lost with the FIRST link: %q", got)
		}
		if st := linkStat(t, "/g"); st.Nlink != 1 {
			t.Fatalf("nlink after removing one of two = %d, want 1", st.Nlink)
		}

		// The publish idiom a database's CopyTo uses: temp → link → unlink.
		if err := os.WriteFile("/tmpcopy", []byte("copy"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link("/tmpcopy", "/final"); err != nil {
			t.Fatalf("publish link: %v", err)
		}
		if err := os.Remove("/tmpcopy"); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile("/final"); string(got) != "copy" {
			t.Fatalf("published content = %q", got)
		}

		// RemoveAll of a directory holding ONE name of a two-link file.
		if err := os.Mkdir("/d", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("/d/in", []byte("shared"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link("/d/in", "/out"); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll("/d"); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile("/out"); string(got) != "shared" {
			t.Fatalf("outside link lost by RemoveAll: %q", got)
		}
		if st := linkStat(t, "/out"); st.Nlink != 1 {
			t.Fatalf("nlink after RemoveAll of the sibling = %d, want 1", st.Nlink)
		}

		// Error order, each shape host-probed.
		expect := func(name string, err error, want syscall.Errno) {
			t.Helper()
			var le *os.LinkError
			if !errors.As(err, &le) || !errors.Is(err, want) {
				t.Fatalf("%s = %v, want LinkError wrapping %v", name, err, want)
			}
		}
		expect("existing new", os.Link("/g", "/out"), syscall.EEXIST)
		if err := os.Mkdir("/dir", 0o755); err != nil {
			t.Fatal(err)
		}
		expect("dir old", os.Link("/dir", "/n"), syscall.EPERM)
		expect("root old", os.Link("/", "/n"), syscall.EPERM)
		expect("missing old", os.Link("/zz", "/n"), syscall.ENOENT)
		expect("missing old, slashed new", os.Link("/zz", "/n/"), syscall.ENOENT)
		expect("slashed file old", os.Link("/g/", "/n"), syscall.ENOTDIR)
		expect("slashed missing new", os.Link("/g", "/n/"), syscall.ENOENT)
		expect("slashed existing new", os.Link("/g", "/out/"), syscall.EEXIST)
		expect("new in missing dir", os.Link("/g", "/no/n"), syscall.ENOENT)
		// Precedence rows, each host-probed: old-walk errors beat the new
		// side; the new side's EEXIST/ENOENT beat the dir-old EPERM.
		expect("missing old beats existing new", os.Link("/zz", "/out"), syscall.ENOENT)
		expect("slashed file old beats existing new", os.Link("/g/", "/out"), syscall.ENOTDIR)
		expect("existing new beats dir old", os.Link("/dir", "/out"), syscall.EEXIST)
		expect("slashed missing new beats dir old", os.Link("/dir", "/n/"), syscall.ENOENT)
		expect("missing-dir new beats dir old", os.Link("/dir", "/no/n"), syscall.ENOENT)
	})
}

// TestDSTFSHardLinkCrashDurability pins reboot semantics: a durably
// linked name survives power loss sharing its inode; an UNSYNCED link
// vanishes; an unsynced removal of one name resurrects it — and the
// restored link count equals the surviving durable dirents.
func TestDSTFSHardLinkCrashDurability(t *testing.T) {
	simulation.Run(2, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("setup", func() {
				if err := os.WriteFile("/f", []byte("v"), 0o600); err != nil {
					t.Fatal(err)
				}
				f, err := os.Open("/f")
				if err != nil {
					t.Fatal(err)
				}
				f.Sync()
				f.Close()
				if err := os.Link("/f", "/durable"); err != nil {
					t.Fatal(err)
				}
				d, err := os.Open("/")
				if err != nil {
					t.Fatal(err)
				}
				d.Sync() // /f and /durable dirents durable
				d.Close()
				if err := os.Link("/f", "/unsynced"); err != nil {
					t.Fatal(err)
				}
			})
		})
		simulation.CrashHost("h")
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("after", func() {
				fi1, err := os.Stat("/f")
				if err != nil {
					t.Fatalf("/f after reboot: %v", err)
				}
				fi2, err := os.Stat("/durable")
				if err != nil {
					t.Fatalf("/durable after reboot: %v", err)
				}
				if !os.SameFile(fi1, fi2) {
					t.Fatal("restored links no longer share an inode")
				}
				if _, err := os.Lstat("/unsynced"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unsynced link survived power loss: %v", err)
				}
				if st := linkStat(t, "/f"); st.Nlink != 2 {
					t.Fatalf("restored nlink = %d, want 2 (the durable dirents)", st.Nlink)
				}
			})
		})
	})
}

// TestDSTFSHardLinksRootSurface pins the os.Root surface: Root.Link
// follows the host Root.Link ladder (one probed divergence from plain
// link(2): a slashed EXISTING regular-file new answers ENOTDIR), and
// every Root dirent removal — Remove, RemoveAll, rename-over — routes
// the same link-count funnel as the named surface.
func TestDSTFSHardLinksRootSurface(t *testing.T) {
	simulation.Run(3, func() {
		if err := os.WriteFile("/a", []byte("v"), 0o600); err != nil {
			t.Fatal(err)
		}
		r, err := os.OpenRoot("/")
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		defer r.Close()
		if err := r.Link("a", "b"); err != nil {
			t.Fatalf("Root.Link: %v", err)
		}
		if st := linkStat(t, "/b"); st.Nlink != 2 {
			t.Fatalf("nlink after Root.Link = %d, want 2", st.Nlink)
		}
		if err := r.Remove("a"); err != nil {
			t.Fatal(err)
		}
		if st := linkStat(t, "/b"); st.Nlink != 1 {
			t.Fatalf("nlink after Root.Remove of one name = %d, want 1", st.Nlink)
		}
		// rename-over a linked target through the Root.
		if err := r.Link("b", "c"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("/solo", []byte("s"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := r.Rename("solo", "c"); err != nil {
			t.Fatalf("Root.Rename over linked target: %v", err)
		}
		if st := linkStat(t, "/b"); st.Nlink != 1 {
			t.Fatalf("nlink after Root.Rename-over = %d, want 1", st.Nlink)
		}
		// RemoveAll through the Root of a dir holding one of two names.
		if err := r.Mkdir("d", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("/d/in", []byte("z"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link("/d/in", "/keep"); err != nil {
			t.Fatal(err)
		}
		if err := r.RemoveAll("d"); err != nil {
			t.Fatal(err)
		}
		if st := linkStat(t, "/keep"); st.Nlink != 1 {
			t.Fatalf("nlink after Root.RemoveAll of sibling = %d, want 1", st.Nlink)
		}
		// Ladder rows, host-probed via os.Root.Link on tmpfs.
		expectL := func(name string, err error, want syscall.Errno) {
			t.Helper()
			if !errors.Is(err, want) {
				t.Fatalf("%s = %v, want %v", name, err, want)
			}
		}
		if err := r.Mkdir("dir", 0o755); err != nil {
			t.Fatal(err)
		}
		expectL("existing new", r.Link("b", "keep"), syscall.EEXIST)
		expectL("missing old beats existing new", r.Link("zz", "keep"), syscall.ENOENT)
		expectL("existing new beats dir old", r.Link("dir", "keep"), syscall.EEXIST)
		expectL("dir old", r.Link("dir", "n"), syscall.EPERM)
		expectL("slashed file old", r.Link("b/", "n"), syscall.ENOTDIR)
		expectL("slashed missing new", r.Link("b", "n/"), syscall.ENOENT)
		expectL("slashed existing file new (rooted divergence)", r.Link("b", "keep/"), syscall.ENOTDIR)
		expectL("dir old + slashed missing new", r.Link("dir", "n/"), syscall.ENOENT)
	})
}

// TestDSTFSHardLinkUnlinkedButOpen pins the fd view of the link count:
// 2 → 1 → 0 as names go, with content readable and writable through
// the surviving fd after the last name is gone.
func TestDSTFSHardLinkUnlinkedButOpen(t *testing.T) {
	simulation.Run(4, func() {
		if err := os.WriteFile("/x", []byte("v"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link("/x", "/y"); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile("/x", os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		nlink := func() uint64 {
			var st syscall.Stat_t
			if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
				t.Fatalf("fstat: %v", err)
			}
			return uint64(st.Nlink)
		}
		if n := nlink(); n != 2 {
			t.Fatalf("nlink = %d, want 2", n)
		}
		if err := os.Remove("/x"); err != nil {
			t.Fatal(err)
		}
		if n := nlink(); n != 1 {
			t.Fatalf("nlink = %d, want 1", n)
		}
		if err := os.Remove("/y"); err != nil {
			t.Fatal(err)
		}
		if n := nlink(); n != 0 {
			t.Fatalf("nlink = %d, want 0 (unlinked-but-open)", n)
		}
		if _, err := f.WriteAt([]byte("w"), 0); err != nil {
			t.Fatalf("write through unlinked fd: %v", err)
		}
		buf := make([]byte, 1)
		if _, err := f.ReadAt(buf, 0); err != nil || buf[0] != 'w' {
			t.Fatalf("read through unlinked fd = (%q, %v)", buf, err)
		}
	})
}
