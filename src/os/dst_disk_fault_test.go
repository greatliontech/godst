// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os_test

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
)

// Disk-fault (EIO) tests — the storage-axis counterpart of the net partition/reset
// suite. They drive simulation.FailDisk / FailFile and assert the spec's three
// fault invariants: SOUND (EIO only at calls a real disk can fail, and never
// corrupting the durable image), VICTIM (exactly the named host/file), and REPLAY
// (an explicit toggle replays under a fixed schedule). The white-box durable-image
// inspector os.DSTFSNodeState lets the durability-preservation case observe that a
// failed fsync does not advance the synced image. All FS work runs inside a named
// Host because the host id a fault targets is interned from the name (the root host
// 0 of a no-Host run is unaddressable, exactly as for the clock faults).

// eioErr fails the test unless err is a *PathError wrapping syscall.EIO.
func eioErr(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("%s: error %v, want errors.Is syscall.EIO", what, err)
	}
	if _, ok := err.(*os.PathError); !ok {
		t.Fatalf("%s: error %T, want *os.PathError", what, err)
	}
}

func mustOK(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error %v", what, err)
	}
}

// onHost runs fn inside a fresh entry of host h (re-entering the same name keeps the
// same tree, so a seed step and a later assertion step share a disk).
func onHost(h string, fn func()) {
	simulation.Host(h, simulation.HostConfig{}, fn)
}

// TestDSTDiskEIOAllMediaOps: under FailDisk every read/pread/write/pwrite/fsync on
// the host's disk fails EIO; HealDisk restores them. Covers all five op choke
// points — a mutation dropping any one EIO check leaves that op succeeding here.
func TestDSTDiskEIOAllMediaOps(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			f, err := os.Create("/f")
			mustOK(t, "Create", err)
			_, err = f.WriteString("seed")
			mustOK(t, "seed write", err)
			mustOK(t, "seed sync", f.Sync())

			simulation.FailDisk("h")
			buf := make([]byte, 4)
			if _, err := f.Read(buf); !errors.Is(err, syscall.EIO) {
				t.Fatalf("Read under FailDisk: %v, want EIO", err)
			}
			eioErr(t, "ReadAt", func() error { _, e := f.ReadAt(buf, 0); return e }())
			eioErr(t, "Write", func() error { _, e := f.Write([]byte("x")); return e }())
			eioErr(t, "WriteAt", func() error { _, e := f.WriteAt([]byte("x"), 0); return e }())
			eioErr(t, "Sync", f.Sync())

			simulation.HealDisk("h")
			_, err = f.Seek(0, io.SeekStart)
			mustOK(t, "Seek after heal", err)
			if _, err := f.Read(buf); err != nil {
				t.Fatalf("Read after heal: %v", err)
			}
			mustOK(t, "Write after heal", func() error { _, e := f.Write([]byte("y")); return e }())
			mustOK(t, "Sync after heal", f.Sync())
			f.Close()
		})
	})
}

// TestDSTDiskEIOInfallibleOpsUnaffected (DST-FAULT-SOUND): EIO is injected only at
// calls a real disk can fail. Seek, Stat, Name and Close are in-memory/metadata ops
// that do not touch the media, so they must keep working under FailDisk — injecting
// EIO there would surface a failure the real stack never produces at that call.
func TestDSTDiskEIOInfallibleOpsUnaffected(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			f, err := os.Create("/f")
			mustOK(t, "Create", err)
			_, err = f.WriteString("abcdef")
			mustOK(t, "write", err)

			simulation.FailDisk("h")
			if off, err := f.Seek(2, io.SeekStart); err != nil || off != 2 {
				t.Fatalf("Seek under FailDisk = %d, %v; want 2, nil", off, err)
			}
			if fi, err := f.Stat(); err != nil || fi.Size() != 6 {
				t.Fatalf("Stat under FailDisk = %v, %v; want size 6, nil", fi, err)
			}
			if f.Name() != "/f" {
				t.Fatalf("Name under FailDisk = %q", f.Name())
			}
			if err := f.Close(); err != nil {
				t.Fatalf("Close under FailDisk: %v", err)
			}
		})
	})
}

// TestDSTDiskEIODurabilityPreserved: a failed write writes no bytes and a failed
// fsync does not advance the durable image (the reason the EIO checks precede the
// mutation / the commit). With cur != synced before the fault, a faulted fsync that
// committed anyway would let a later crash restore data the app was told did not
// persist — a soundness violation.
func TestDSTDiskEIODurabilityPreserved(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			f, err := os.Create("/f")
			mustOK(t, "Create", err)
			_, err = f.WriteString("abc")
			mustOK(t, "write abc", err)
			mustOK(t, "sync", f.Sync()) // synced = abc
			_, err = f.WriteString("def")
			mustOK(t, "write def", err) // cur = abcdef, synced = abc

			simulation.FailDisk("h")
			if _, err := f.WriteString("XYZ"); !errors.Is(err, syscall.EIO) {
				t.Fatalf("faulted write: %v, want EIO", err)
			}
			if err := f.Sync(); !errors.Is(err, syscall.EIO) {
				t.Fatalf("faulted sync: %v, want EIO", err)
			}
			cur, synced, _, _, _, _, ok := os.DSTFSNodeState("/f")
			if !ok {
				t.Fatal("DSTFSNodeState not ok")
			}
			if cur != "abcdef" {
				t.Fatalf("current content = %q, want abcdef (faulted write mutated state)", cur)
			}
			if synced != "abc" {
				t.Fatalf("durable image = %q, want abc (faulted fsync advanced the durable image)", synced)
			}
			f.Close()
		})
	})
}

// TestDSTDiskEIOVictimHost (DST-FAULT-VICTIM): FailDisk on one host fails exactly
// that host's I/O; a co-running host is untouched. A check that ignores the host id
// (fails all disks once any fault exists) fails on hB here.
func TestDSTDiskEIOVictimHost(t *testing.T) {
	simulation.Run(1, func() {
		seed := func() {
			f, err := os.Create("/f")
			mustOK(t, "Create", err)
			_, err = f.WriteString("data")
			mustOK(t, "write", err)
			mustOK(t, "sync", f.Sync())
			f.Close()
		}
		onHost("hA", seed)
		onHost("hB", seed)

		simulation.FailDisk("hA")

		onHost("hA", func() {
			f, err := os.Open("/f")
			mustOK(t, "hA open", err)
			defer f.Close()
			if _, err := f.Read(make([]byte, 4)); !errors.Is(err, syscall.EIO) {
				t.Fatalf("hA read: %v, want EIO", err)
			}
		})
		onHost("hB", func() {
			f, err := os.Open("/f")
			mustOK(t, "hB open", err)
			defer f.Close()
			if n, err := f.Read(make([]byte, 4)); err != nil || n != 4 {
				t.Fatalf("hB read = %d, %v; want 4, nil (victim leaked onto hB)", n, err)
			}
		})
	})
}

// TestDSTDiskEIOPerFile: FailFile fails exactly one file; a sibling on the same disk
// is untouched (DST-FAULT-VICTIM at file granularity), and HealFile restores it.
func TestDSTDiskEIOPerFile(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "WriteFile x", os.WriteFile("/x", []byte("xxxx"), 0o644))
			mustOK(t, "WriteFile y", os.WriteFile("/y", []byte("yyyy"), 0o644))

			simulation.FailFile("h", "/x")
			if _, err := os.ReadFile("/x"); !errors.Is(err, syscall.EIO) {
				t.Fatalf("read /x under FailFile: %v, want EIO", err)
			}
			if b, err := os.ReadFile("/y"); err != nil || string(b) != "yyyy" {
				t.Fatalf("read /y = %q, %v; want yyyy, nil (per-file fault leaked to sibling)", b, err)
			}

			simulation.HealFile("h", "/x")
			if b, err := os.ReadFile("/x"); err != nil || string(b) != "xxxx" {
				t.Fatalf("read /x after HealFile = %q, %v; want xxxx, nil", b, err)
			}
		})
	})
}

// TestDSTDiskEIOFollowsRename: the per-file fault keys on the file (node), not its
// path, so it follows the file across a rename — a bad sector is physical. A
// path-keyed implementation would fail this (the new name reads clean).
func TestDSTDiskEIOFollowsRename(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "WriteFile", os.WriteFile("/a", []byte("data"), 0o644))
			simulation.FailFile("h", "/a")
			mustOK(t, "Rename", os.Rename("/a", "/b"))

			if _, err := os.ReadFile("/b"); !errors.Is(err, syscall.EIO) {
				t.Fatalf("read /b (renamed from faulted /a): %v, want EIO", err)
			}
			mustOK(t, "recreate /a", os.WriteFile("/a", []byte("fresh"), 0o644))
			if b, err := os.ReadFile("/a"); err != nil || string(b) != "fresh" {
				t.Fatalf("read fresh /a = %q, %v; want fresh, nil (fault clung to the path)", b, err)
			}
		})
	})
}

// TestDSTDiskEIORemovedButOpen: an open handle to a faulted file keeps failing after
// the file is unlinked — the node (its blocks) outlives the name, so the fault does
// too.
func TestDSTDiskEIORemovedButOpen(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "WriteFile", os.WriteFile("/f", []byte("data"), 0o644))
			f, err := os.Open("/f")
			mustOK(t, "Open", err)
			defer f.Close()

			simulation.FailFile("h", "/f")
			mustOK(t, "Remove", os.Remove("/f"))

			if _, err := f.Read(make([]byte, 4)); !errors.Is(err, syscall.EIO) {
				t.Fatalf("read removed-but-open faulted file: %v, want EIO", err)
			}
		})
	})
}

// TestDSTDiskEIOPerFileNonexistentNoop: faulting a path that does not exist is a
// no-op (no file to fail), and creating it afterward yields a clean file.
func TestDSTDiskEIOPerFileNonexistentNoop(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.FailFile("h", "/ghost") // no such file
			mustOK(t, "WriteFile", os.WriteFile("/ghost", []byte("real"), 0o644))
			if b, err := os.ReadFile("/ghost"); err != nil || string(b) != "real" {
				t.Fatalf("read /ghost = %q, %v; want real, nil (no-op fault leaked)", b, err)
			}
		})
	})
}

// TestDSTDiskEIOHealIndependence: the host-wide and per-file faults are independent.
// HealDisk clears only the host-wide EIO; a file under FailFile keeps failing until
// its own HealFile.
func TestDSTDiskEIOHealIndependence(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "WriteFile", os.WriteFile("/f", []byte("data"), 0o644))
			simulation.FailDisk("h")
			simulation.FailFile("h", "/f")

			simulation.HealDisk("h") // clears host-wide only
			if _, err := os.ReadFile("/f"); !errors.Is(err, syscall.EIO) {
				t.Fatalf("read /f after HealDisk (FailFile still set): %v, want EIO", err)
			}
			simulation.HealFile("h", "/f")
			if b, err := os.ReadFile("/f"); err != nil || string(b) != "data" {
				t.Fatalf("read /f after HealFile = %q, %v; want data, nil", b, err)
			}
		})
	})
}

// TestDSTDiskEIODirFaultNoop: FailFile is scoped to regular files. Naming a directory
// (or the root) is a no-op — even the directory handle's own Sync stays clean. That
// fsync is the behavioral seam the guard protects: sync, unlike read/write, has no
// isDir short-circuit, so without the guard FailFile on a directory would EIO its
// fsync (the assertion below fails if the guard is removed). A file inside the
// directory also reads cleanly — per-file EIO is one file, never a subtree.
func TestDSTDiskEIODirFaultNoop(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "Mkdir", os.Mkdir("/d", 0o755))
			mustOK(t, "WriteFile", os.WriteFile("/d/f", []byte("data"), 0o644))

			simulation.FailFile("h", "/d") // a directory
			simulation.FailFile("h", "/")  // the root

			dh, err := os.Open("/d")
			mustOK(t, "Open dir", err)
			if err := dh.Sync(); err != nil {
				t.Fatalf("dir Sync under FailFile(dir): %v, want nil (a missing guard EIOs a directory fsync)", err)
			}
			dh.Close()

			if b, err := os.ReadFile("/d/f"); err != nil || string(b) != "data" {
				t.Fatalf("read /d/f after FailFile on a dir/root = %q, %v; want data, nil", b, err)
			}
		})
	})
}

// TestDSTDiskEIOHostWideFailsDirSync: the complement — a whole-disk failure
// (FailDisk) does fail a directory handle's Sync, because a dead disk cannot persist
// anything. Host-wide EIO covers directories; a single targeted file (above) does
// not.
func TestDSTDiskEIOHostWideFailsDirSync(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "Mkdir", os.Mkdir("/d", 0o755))
			dh, err := os.Open("/d")
			mustOK(t, "Open dir", err)
			defer dh.Close()

			simulation.FailDisk("h")
			if err := dh.Sync(); !errors.Is(err, syscall.EIO) {
				t.Fatalf("dir Sync under FailDisk: %v, want EIO (a dead disk fsyncs nothing)", err)
			}
		})
	})
}

// TestDSTDiskEIODeterminism (DST-FAULT-REPLAY): EIO is an explicit toggle, so the
// same seed + same fault schedule produces an identical sequence of outcomes.
func TestDSTDiskEIODeterminism(t *testing.T) {
	run := func() string {
		var trace string
		simulation.Run(7, func() {
			onHost("h", func() {
				mustOK(t, "WriteFile", os.WriteFile("/f", []byte("data"), 0o644))
				rd := func() string {
					if _, err := os.ReadFile("/f"); errors.Is(err, syscall.EIO) {
						return "E"
					}
					return "."
				}
				trace += rd() // ok
				simulation.FailDisk("h")
				trace += rd() // E
				simulation.HealDisk("h")
				trace += rd() // ok
				simulation.FailFile("h", "/f")
				trace += rd() // E
				simulation.HealFile("h", "/f")
				trace += rd() // ok
			})
		})
		return trace
	}
	a, b := run(), run()
	if a != b {
		t.Fatalf("non-deterministic disk-fault trace: %q vs %q", a, b)
	}
	if a != ".E.E." {
		t.Fatalf("trace = %q, want .E.E.", a)
	}
}
