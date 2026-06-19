// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os_test

import (
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
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

// enospcErr fails the test unless err is a *PathError wrapping syscall.ENOSPC.
func enospcErr(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("%s: error %v, want errors.Is syscall.ENOSPC", what, err)
	}
	if _, ok := err.(*os.PathError); !ok {
		t.Fatalf("%s: error %T, want *os.PathError", what, err)
	}
}

// TestDSTDiskENOSPCBasic: under a capacity, a write up to the cap succeeds, a write
// past it fails ENOSPC, and UnlimitDisk restores writes.
func TestDSTDiskENOSPCBasic(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.LimitDisk("h", 4)
			f, err := os.Create("/f")
			mustOK(t, "Create", err)
			if n, err := f.Write([]byte("abcd")); n != 4 || err != nil {
				t.Fatalf("write to cap = %d, %v; want 4, nil", n, err)
			}
			if _, err := f.Write([]byte("e")); !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("write past cap: %v, want ENOSPC", err)
			}
			simulation.UnlimitDisk("h")
			if n, err := f.Write([]byte("e")); n != 1 || err != nil {
				t.Fatalf("write after UnlimitDisk = %d, %v; want 1, nil", n, err)
			}
			f.Close()
		})
	})
}

// TestDSTDiskENOSPCPartialFill (DST-FAULT-SOUND): a write that would exceed the cap
// fills the remaining space (a short write, io.ErrShortWrite) rather than failing
// outright — a real disk writes what it can; the next write, with nothing left, gets
// ENOSPC. An all-or-nothing implementation (0 bytes + ENOSPC when room > 0) fails
// here.
func TestDSTDiskENOSPCPartialFill(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.LimitDisk("h", 5)
			f, err := os.Create("/f")
			mustOK(t, "Create", err)

			n, err := f.Write([]byte("abcdefghij")) // 10 bytes, room for 5
			if n != 5 || !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("filling write = %d, %v; want 5, io.ErrShortWrite", n, err)
			}
			if _, err := f.Write([]byte("Z")); !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("write on full disk: %v, want ENOSPC", err)
			}
			f.Close()
			if b, err := os.ReadFile("/f"); err != nil || string(b) != "abcde" {
				t.Fatalf("content = %q, %v; want abcde (the bytes that fit)", b, err)
			}
		})
	})
}

// TestDSTDiskENOSPCFillsToExactlyCapacity (DST-FAULT-SOUND, property): writing in
// chunks until ENOSPC fills the disk to *exactly* the cap — never less (a false
// ENOSPC with room to spare) nor more (over-allocation past the cap).
func TestDSTDiskENOSPCFillsToExactlyCapacity(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			const cap = 1000
			simulation.LimitDisk("h", cap)
			f, err := os.Create("/f")
			mustOK(t, "Create", err)
			defer f.Close()
			chunk := make([]byte, 7) // a size that does not divide the cap
			total := 0
			for i := 0; i < cap+100; i++ {
				n, err := f.Write(chunk)
				total += n
				if errors.Is(err, syscall.ENOSPC) {
					break
				}
				if err != nil && !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("unexpected write error: %v", err)
				}
			}
			if total != cap {
				t.Fatalf("filled %d bytes, want exactly %d (cap)", total, cap)
			}
		})
	})
}

// TestDSTDiskENOSPCFreesHonored (DST-FAULT-SOUND): space in use is the live total, so
// deleting a file makes room for a write that just failed. A budget that decremented
// per byte written (ignoring frees) would still fail after the delete — a false
// positive.
func TestDSTDiskENOSPCFreesHonored(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.LimitDisk("h", 10)
			mustOK(t, "fill /a", os.WriteFile("/a", make([]byte, 10), 0o644)) // 10/10 full
			if err := os.WriteFile("/b", []byte("x"), 0o644); !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("create on full disk: %v, want ENOSPC", err)
			}
			mustOK(t, "remove /a", os.Remove("/a")) // frees 10
			if err := os.WriteFile("/b", []byte("x"), 0o644); err != nil {
				t.Fatalf("write after freeing space: %v, want nil (frees not honored)", err)
			}
		})
	})
}

// TestDSTDiskENOSPCTruncateFrees (DST-FAULT-SOUND): truncating a file down frees its
// bytes, so a subsequent write that needs that space succeeds.
func TestDSTDiskENOSPCTruncateFrees(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.LimitDisk("h", 10)
			f, err := os.Create("/f")
			mustOK(t, "Create", err)
			defer f.Close()
			if n, err := f.Write([]byte("0123456789")); n != 10 || err != nil {
				t.Fatalf("fill = %d, %v; want 10, nil", n, err)
			}
			if _, err := f.Write([]byte("Z")); !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("write on full disk: %v, want ENOSPC", err)
			}
			mustOK(t, "Truncate", f.Truncate(4)) // frees 6 bytes
			if n, err := f.WriteAt([]byte("ab"), 4); n != 2 || err != nil {
				t.Fatalf("write after truncate = %d, %v; want 2, nil", n, err)
			}
		})
	})
}

// TestDSTDiskENOSPCOverwriteInPlace (DST-FAULT-SOUND): an in-place overwrite consumes
// no new space, so it succeeds even on a full disk. A check on total size regardless
// of growth would wrongly fail it.
func TestDSTDiskENOSPCOverwriteInPlace(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.LimitDisk("h", 4)
			mustOK(t, "fill", os.WriteFile("/f", []byte("abcd"), 0o644)) // 4/4 full
			f, err := os.OpenFile("/f", os.O_RDWR, 0)
			mustOK(t, "OpenFile", err)
			defer f.Close()
			if n, err := f.WriteAt([]byte("WXYZ"), 0); n != 4 || err != nil {
				t.Fatalf("in-place overwrite on full disk = %d, %v; want 4, nil", n, err)
			}
			if b, err := os.ReadFile("/f"); err != nil || string(b) != "WXYZ" {
				t.Fatalf("content = %q, %v; want WXYZ", b, err)
			}
		})
	})
}

// TestDSTDiskENOSPCCreate: creating a new file or directory on a full disk fails
// ENOSPC; with room, both succeed.
func TestDSTDiskENOSPCCreate(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.LimitDisk("h", 4)
			mustOK(t, "fill", os.WriteFile("/f", []byte("abcd"), 0o644)) // 4/4 full

			_, err := os.Create("/g")
			enospcErr(t, "Create on full disk", err)
			enospcErr(t, "Mkdir on full disk", os.Mkdir("/d", 0o755))

			// Freeing space lets a create succeed again.
			mustOK(t, "remove /f", os.Remove("/f"))
			mustOK(t, "Mkdir after free", os.Mkdir("/d", 0o755))
		})
	})
}

// TestDSTDiskENOSPCTruncExistingOnFull: opening an EXISTING file with O_CREATE|O_TRUNC
// on a full disk succeeds — the node already exists (no allocation) and O_TRUNC frees
// its bytes. The create-full check must fire only for a genuinely new node; a refactor
// that hoisted it above the resolve would wrongly fail this.
func TestDSTDiskENOSPCTruncExistingOnFull(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.LimitDisk("h", 4)
			mustOK(t, "fill", os.WriteFile("/f", []byte("abcd"), 0o644)) // 4/4 full
			f, err := os.OpenFile("/f", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
			mustOK(t, "O_CREATE|O_TRUNC existing on full disk", err)
			defer f.Close()
			if n, err := f.Write([]byte("WXYZ")); n != 4 || err != nil {
				t.Fatalf("write after O_TRUNC freed space = %d, %v; want 4, nil", n, err)
			}
		})
	})
}

// TestDSTDiskENOSPCSyncPartialDurable: an O_SYNC write the cap only partly satisfies
// commits exactly the bytes that fit to the durable image — never 0, never more than
// fit. Guards the durability invariant on the partial-write path.
func TestDSTDiskENOSPCSyncPartialDurable(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.LimitDisk("h", 3)
			f, err := os.OpenFile("/f", os.O_CREATE|os.O_RDWR|os.O_SYNC, 0o644)
			mustOK(t, "OpenFile O_SYNC", err)
			n, err := f.Write([]byte("abcde")) // room 3, only 3 fit
			if n != 3 || !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("O_SYNC partial write = %d, %v; want 3, io.ErrShortWrite", n, err)
			}
			f.Close()
			_, synced, _, _, _, _, ok := os.DSTFSNodeState("/f")
			if !ok || synced != "abc" {
				t.Fatalf("durable image = %q (ok=%v), want abc (O_SYNC committed exactly what fit)", synced, ok)
			}
		})
	})
}

// TestDSTDiskENOSPCCapBelowUsage: a cap set below current usage puts the disk over
// quota — growth and creates fail, but in-place overwrites still work, and freeing
// below the cap re-enables writes.
func TestDSTDiskENOSPCCapBelowUsage(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "write 10", os.WriteFile("/f", []byte("0123456789"), 0o644))
			simulation.LimitDisk("h", 5) // below the 10 already in use

			f, err := os.OpenFile("/f", os.O_RDWR, 0)
			mustOK(t, "OpenFile", err)
			// Growth (append at the end) fails: the disk is over quota.
			if _, err := f.WriteAt([]byte("X"), 10); !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("append while over quota: %v, want ENOSPC", err)
			}
			// In-place overwrite consumes no space: succeeds even over quota.
			if n, err := f.WriteAt([]byte("AB"), 8); n != 2 || err != nil {
				t.Fatalf("in-place overwrite over quota = %d, %v; want 2, nil", n, err)
			}
			mustOK(t, "Truncate to 3", f.Truncate(3)) // 3 < cap 5, frees room
			if n, err := f.WriteAt([]byte("YZ"), 3); n != 2 || err != nil {
				t.Fatalf("append after truncate = %d, %v; want 2, nil (freed space unusable)", n, err)
			}
			f.Close()
		})
	})
}

// TestDSTDiskENOSPCStraddleOverQuota (DST-FAULT-SOUND): a write that straddles EOF on
// an over-quota disk writes its in-place prefix (no new space) and fails only on the
// growth past EOF — the room-clamp must not let a negative free count exclude the
// no-growth prefix. Without the clamp this write stores 0 bytes and the prefix is
// lost.
func TestDSTDiskENOSPCStraddleOverQuota(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "write 10", os.WriteFile("/f", []byte("0123456789"), 0o644))
			simulation.LimitDisk("h", 5) // over quota (10 in use, 0 free)
			f, err := os.OpenFile("/f", os.O_RDWR, 0)
			mustOK(t, "OpenFile", err)
			defer f.Close()
			// 5 bytes at offset 7: [7,10) overwrites in place, [10,12) is growth.
			n, err := f.WriteAt([]byte("ABCDE"), 7)
			if n != 3 || !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("straddle write over quota = %d, %v; want 3, ENOSPC (in-place prefix, then growth fails)", n, err)
			}
			if b, err := os.ReadFile("/f"); err != nil || string(b) != "0123456ABC" {
				t.Fatalf("content = %q, %v; want 0123456ABC (bytes 7-9 overwritten, no growth)", b, err)
			}
		})
	})
}

// TestDSTDiskENOSPCReadUnaffected (DST-FAULT-SOUND): ENOSPC is a write/create fault;
// reads on a full disk are unaffected.
func TestDSTDiskENOSPCReadUnaffected(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.LimitDisk("h", 4)
			mustOK(t, "fill", os.WriteFile("/f", []byte("abcd"), 0o644))
			if b, err := os.ReadFile("/f"); err != nil || string(b) != "abcd" {
				t.Fatalf("read on full disk = %q, %v; want abcd, nil", b, err)
			}
		})
	})
}

// TestDSTDiskENOSPCVictim (DST-FAULT-VICTIM): LimitDisk caps exactly the named host's
// disk; another host writing the same data is unaffected.
func TestDSTDiskENOSPCVictim(t *testing.T) {
	simulation.Run(1, func() {
		simulation.LimitDisk("hA", 0) // full: even a new file's create fails
		onHost("hA", func() {
			if err := os.WriteFile("/f", []byte("abcd"), 0o644); !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("hA write: %v, want ENOSPC", err)
			}
		})
		onHost("hB", func() {
			if err := os.WriteFile("/f", []byte("abcd"), 0o644); err != nil {
				t.Fatalf("hB write: %v, want nil (cap leaked onto hB)", err)
			}
		})
	})
}

// TestDSTDiskENOSPCEIOPrecedence: when a disk is both EIO-failing and full, a write
// reports EIO — the hardware failure is checked before the space check, as on a real
// disk that cannot even attempt the write.
func TestDSTDiskENOSPCEIOPrecedence(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			f, err := os.Create("/f")
			mustOK(t, "Create", err)
			defer f.Close()
			simulation.LimitDisk("h", 0) // full
			simulation.FailDisk("h")     // and failing
			if _, err := f.Write([]byte("x")); !errors.Is(err, syscall.EIO) {
				t.Fatalf("write on EIO+full disk: %v, want EIO (EIO precedes ENOSPC)", err)
			}
		})
	})
}

// TestDSTDiskENOSPCDeterminism (DST-FAULT-REPLAY): LimitDisk is an explicit toggle, so
// the same seed + same fault schedule yields an identical outcome sequence.
func TestDSTDiskENOSPCDeterminism(t *testing.T) {
	run := func() string {
		var trace string
		simulation.Run(9, func() {
			onHost("h", func() {
				wr := func() string {
					if err := os.WriteFile("/f", []byte("abc"), 0o644); errors.Is(err, syscall.ENOSPC) {
						return "E"
					}
					os.Remove("/f")
					return "."
				}
				trace += wr() // ok
				simulation.LimitDisk("h", 0)
				trace += wr() // E
				simulation.UnlimitDisk("h")
				trace += wr() // ok
			})
		})
		return trace
	}
	a, b := run(), run()
	if a != b {
		t.Fatalf("non-deterministic ENOSPC trace: %q vs %q", a, b)
	}
	if a != ".E." {
		t.Fatalf("trace = %q, want .E.", a)
	}
}

// --- Latency (slow disk) tests ---
// Latency is observed via the bubble's virtual clock: time.Since across a slowed op
// equals the configured per-op delay; across an in-memory op it is zero.

// TestDSTDiskLatencyBasic: under SlowDisk a read takes the per-op latency (virtual);
// SlowDisk(0) removes it.
func TestDSTDiskLatencyBasic(t *testing.T) {
	const lat = 50 * time.Millisecond
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "WriteFile", os.WriteFile("/f", []byte("data"), 0o644))
			f, err := os.Open("/f")
			mustOK(t, "Open", err)
			defer f.Close()

			simulation.SlowDisk("h", lat)
			t0 := time.Now()
			if _, err := f.Read(make([]byte, 4)); err != nil {
				t.Fatalf("read: %v", err)
			}
			if d := time.Since(t0); d != lat {
				t.Fatalf("slowed read took %v, want %v", d, lat)
			}

			simulation.SlowDisk("h", 0) // clear
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				t.Fatalf("seek: %v", err)
			}
			t1 := time.Now()
			if _, err := f.Read(make([]byte, 4)); err != nil {
				t.Fatalf("read after clear: %v", err)
			}
			if d := time.Since(t1); d != 0 {
				t.Fatalf("read after SlowDisk(0) took %v, want 0", d)
			}
		})
	})
}

// TestDSTDiskLatencyAllOps: every disk-touching op pays the per-op latency exactly
// once. Setup runs before SlowDisk so it is not itself delayed.
func TestDSTDiskLatencyAllOps(t *testing.T) {
	const lat = 20 * time.Millisecond
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "WriteFile f", os.WriteFile("/f", []byte("data"), 0o644))
			mustOK(t, "WriteFile g", os.WriteFile("/g", []byte("xyz"), 0o644))
			mustOK(t, "Mkdir", os.Mkdir("/d", 0o755))
			mustOK(t, "Mkdir rr", os.Mkdir("/rr", 0o755))
			fr, err := os.Open("/f")
			mustOK(t, "Open fr", err)
			defer fr.Close()
			fw, err := os.OpenFile("/f", os.O_RDWR, 0)
			mustOK(t, "OpenFile fw", err)
			defer fw.Close()

			simulation.SlowDisk("h", lat)
			measure := func(name string, op func()) {
				t0 := time.Now()
				op()
				if d := time.Since(t0); d != lat {
					t.Errorf("%s took %v, want %v", name, d, lat)
				}
			}
			measure("read", func() { fr.Read(make([]byte, 4)) })
			measure("pread", func() { fr.ReadAt(make([]byte, 4), 0) })
			measure("write", func() { fw.WriteAt([]byte("X"), 0) })
			measure("sync", func() { fw.Sync() })
			measure("truncate-handle", func() { fw.Truncate(3) })
			measure("chmod-handle", func() { fw.Chmod(0o600) })
			measure("open", func() { g, _ := os.Open("/g"); g.Close() })
			measure("stat", func() { os.Stat("/g") })
			measure("mkdir", func() { os.Mkdir("/d2", 0o755) })
			measure("remove", func() { os.Remove("/d2") })
			measure("removeAll", func() { os.RemoveAll("/rr") })
			measure("truncate-name", func() { os.Truncate("/f", 2) })
			measure("chmod-name", func() { os.Chmod("/f", 0o644) })
			measure("chtimes", func() { os.Chtimes("/f", time.Now(), time.Now()) })

			// os.Rename does an internal Lstat(newname) before the rename itself,
			// so on a slow disk it pays the latency twice — two real disk ops, the
			// faithful result (not double-counting one op).
			t0 := time.Now()
			mustOK(t, "Rename", os.Rename("/g", "/g2"))
			if d := time.Since(t0); d != 2*lat {
				t.Errorf("rename took %v, want %v (Lstat + rename)", d, 2*lat)
			}
		})
	})
}

// TestDSTDiskLatencyReaddir: a directory read is delayed too. ReadDir may call the
// backend more than once, so assert it is delayed by at least the per-op latency.
func TestDSTDiskLatencyReaddir(t *testing.T) {
	const lat = 10 * time.Millisecond
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "Mkdir", os.Mkdir("/d", 0o755))
			mustOK(t, "WriteFile", os.WriteFile("/d/a", nil, 0o644))
			d, err := os.Open("/d")
			mustOK(t, "Open dir", err)
			defer d.Close()

			simulation.SlowDisk("h", lat)
			t0 := time.Now()
			if _, err := d.ReadDir(-1); err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			if got := time.Since(t0); got < lat {
				t.Fatalf("ReadDir took %v, want >= %v", got, lat)
			}
		})
	})
}

// TestDSTDiskLatencyInMemoryOpsUnaffected (DST-FAULT-SOUND): seek and Getwd touch no
// disk, so a slow disk never delays them — delaying them would be a delay the real
// stack never imposes.
func TestDSTDiskLatencyInMemoryOpsUnaffected(t *testing.T) {
	const lat = 50 * time.Millisecond
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "WriteFile", os.WriteFile("/f", []byte("data"), 0o644))
			f, err := os.Open("/f")
			mustOK(t, "Open", err)
			defer f.Close()

			simulation.SlowDisk("h", lat)
			t0 := time.Now()
			if _, err := f.Seek(2, io.SeekStart); err != nil {
				t.Fatalf("seek: %v", err)
			}
			if _, err := os.Getwd(); err != nil {
				t.Fatalf("getwd: %v", err)
			}
			if d := time.Since(t0); d != 0 {
				t.Fatalf("seek+getwd under SlowDisk took %v, want 0", d)
			}
		})
	})
}

// TestDSTDiskLatencyClosedFdNoDelay (DST-FAULT-SOUND): a read on a closed handle
// returns EBADF/closing without touching the disk, so it is not delayed.
func TestDSTDiskLatencyClosedFdNoDelay(t *testing.T) {
	const lat = 50 * time.Millisecond
	simulation.Run(1, func() {
		onHost("h", func() {
			mustOK(t, "WriteFile", os.WriteFile("/f", []byte("data"), 0o644))
			f, err := os.Open("/f")
			mustOK(t, "Open", err)
			f.Close()

			simulation.SlowDisk("h", lat)
			t0 := time.Now()
			if _, err := f.Read(make([]byte, 4)); err == nil {
				t.Fatal("read on closed fd succeeded, want error")
			}
			if d := time.Since(t0); d != 0 {
				t.Fatalf("read on closed fd under SlowDisk took %v, want 0 (no disk access)", d)
			}
		})
	})
}

// TestDSTDiskLatencyVictim (DST-FAULT-VICTIM): SlowDisk slows exactly the named host;
// another host's identical read is instant.
func TestDSTDiskLatencyVictim(t *testing.T) {
	const lat = 30 * time.Millisecond
	simulation.Run(1, func() {
		seed := func() { mustOK(t, "WriteFile", os.WriteFile("/f", []byte("data"), 0o644)) }
		onHost("hA", seed)
		onHost("hB", seed)
		simulation.SlowDisk("hA", lat)

		onHost("hA", func() {
			f, _ := os.Open("/f")
			defer f.Close()
			t0 := time.Now()
			f.Read(make([]byte, 4))
			if d := time.Since(t0); d != lat {
				t.Fatalf("hA read took %v, want %v", d, lat)
			}
		})
		onHost("hB", func() {
			f, _ := os.Open("/f")
			defer f.Close()
			t0 := time.Now()
			f.Read(make([]byte, 4))
			if d := time.Since(t0); d != 0 {
				t.Fatalf("hB read took %v, want 0 (slow disk leaked onto hB)", d)
			}
		})
	})
}

// TestDSTDiskLatencyHostIndependence: a slow disk on hA must NOT stall hB's
// filesystem — the delay sleeps outside the shared tree lock. hB's read runs to
// completion at virtual time 0 while hA is mid-sleep; if the sleep held dstFS.mu, hB
// would block until hA woke (== lat).
func TestDSTDiskLatencyHostIndependence(t *testing.T) {
	const lat = 40 * time.Millisecond
	simulation.Run(1, func() {
		seed := func() { mustOK(t, "WriteFile", os.WriteFile("/f", []byte("data"), 0o644)) }
		onHost("hA", seed)
		onHost("hB", seed)
		simulation.SlowDisk("hA", lat)

		var hbElapsed time.Duration
		var wg sync.WaitGroup
		wg.Add(2)
		onHost("hA", func() {
			go func() {
				defer wg.Done()
				f, _ := os.Open("/f") // slow (hA): lat
				f.Read(make([]byte, 4))
				f.Close()
			}()
		})
		onHost("hB", func() {
			go func() {
				defer wg.Done()
				t0 := time.Now()
				f, _ := os.Open("/f") // hB: not slow
				f.Read(make([]byte, 4))
				f.Close()
				hbElapsed = time.Since(t0)
			}()
		})
		wg.Wait()
		if hbElapsed != 0 {
			t.Fatalf("hB filesystem stalled %v behind hA's slow disk (sleep held the shared lock)", hbElapsed)
		}
	})
}

// TestDSTDiskLatencyDeterminism (DST-FAULT-REPLAY): an explicit per-op duration, so
// the same seed + schedule replays the same virtual delays.
func TestDSTDiskLatencyDeterminism(t *testing.T) {
	const lat = 15 * time.Millisecond
	run := func() time.Duration {
		var total time.Duration
		simulation.Run(3, func() {
			onHost("h", func() {
				mustOK(t, "WriteFile", os.WriteFile("/f", []byte("data"), 0o644))
				f, _ := os.Open("/f")
				defer f.Close()
				simulation.SlowDisk("h", lat)
				t0 := time.Now()
				f.Read(make([]byte, 4))
				f.Seek(0, io.SeekStart)
				f.Read(make([]byte, 4))
				total = time.Since(t0)
			})
		})
		return total
	}
	a, b := run(), run()
	if a != b {
		t.Fatalf("non-deterministic latency: %v vs %v", a, b)
	}
	if a != 2*lat { // two slowed reads; the seek is in-memory
		t.Fatalf("two reads + a seek took %v, want %v", a, 2*lat)
	}
}
