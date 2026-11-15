// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// CorruptFile (bit rot) tests — the silent-corruption counterpart of the EIO
// suite. They assert the fault's contract from disk_fault.go: the flip lands on
// the platter and the page cache masks it until a host crash surfaces it
// (SOUND: latent rot is discovered on the next platter read, and live reads
// serve cached bytes exactly as a real kernel does); a sync that rewrites the
// affected page's sectors heals it while syncs elsewhere in the file do not
// (SOUND: writeback rewrites only what changed); it corrupts exactly the named
// file (VICTIM); and the same seed rots the same bit (REPLAY — the draws come
// from the stream-isolated fault RNG).

// corruptRun runs one write/corrupt/crash/reboot cycle on host "h" and returns
// what the reboot reads from /f. seed drives the fault RNG (where the flip
// lands); tear turns on CrashTear (the flip must survive the tear draws of a
// fully-synced file — clean pages have no writeback fate to draw); between, if
// non-nil, runs after the corruption and before the crash (the heal arms
// rewrite or append there).
// noopsFirst names paths CorruptFile'd BEFORE /f — no-op targets that must
// draw nothing (identical wreckage with and without them pins the no-draw
// contract).
func corruptRun(t *testing.T, seed uint64, content []byte, tear bool, between func(t *testing.T), noopsFirst ...string) []byte {
	t.Helper()
	var got []byte
	RunWith(seed, Options{CrashTear: tear}, func() {
		Host("h", HostConfig{}, func() {
			go Process("db", func() {
				if err := os.WriteFile("/f", content, 0o644); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				syncFile(t, "/f")
				if err := os.WriteFile("/empty", nil, 0o644); err != nil {
					t.Errorf("write empty: %v", err)
					return
				}
				syncFile(t, "/empty")
				syncDir(t, "/")
				select {}
			})
			for range 30 {
				runtime.Gosched()
			}
		})

		for _, p := range noopsFirst {
			CorruptFile("h", p)
		}
		CorruptFile("h", "/f")

		// The cache masks the platter: a live read after the corruption still
		// returns the written bytes, on every run of every arm.
		Host("h", HostConfig{}, func() {
			b, err := os.ReadFile("/f")
			if err != nil {
				t.Fatalf("live read: %v", err)
			}
			if !bytes.Equal(b, content) {
				t.Fatalf("live read observed the rot: the page cache must mask the platter until a reboot")
			}
		})

		if between != nil {
			Host("h", HostConfig{}, func() { between(t) })
		}

		CrashHost("h")

		Host("h", HostConfig{}, func() {
			Process("recover", func() {
				b, err := os.ReadFile("/f")
				if err != nil {
					t.Fatalf("read after reboot: %v", err)
				}
				got = b
			})
		})
	})
	return got
}

// syncFile fsyncs one existing file.
func syncFile(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		t.Fatalf("sync %s: %v", path, err)
	}
}

// bitDiff returns the offsets whose bytes differ and the total differing bits.
func bitDiff(a, b []byte) (offsets []int, bits int) {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			offsets = append(offsets, i)
			for x := a[i] ^ b[i]; x != 0; x &= x - 1 {
				bits++
			}
		}
	}
	return offsets, bits
}

// TestDSTDiskCorruptSurfacesAtReboot: one CorruptFile flips exactly one bit of
// the durable image, invisible to live reads, visible after the reboot; the
// same seed rots the same bit (bit-for-bit identical wreckage) and a different
// seed rots elsewhere.
func TestDSTDiskCorruptSurfacesAtReboot(t *testing.T) {
	content := bytes.Repeat([]byte("R"), 2*4096)
	a1 := corruptRun(t, 7, content, false, nil)
	if len(a1) != len(content) {
		t.Fatalf("reboot read %d bytes, want %d", len(a1), len(content))
	}
	offs, bits := bitDiff(content, a1)
	if len(offs) != 1 || bits != 1 {
		t.Fatalf("corruption = %d bytes / %d bits differing, want exactly 1/1 (offsets %v)", len(offs), bits, offs)
	}
	if a2 := corruptRun(t, 7, content, false, nil); !bytes.Equal(a1, a2) {
		t.Fatal("same seed produced different rot (DST-FAULT-REPLAY)")
	}
	b1 := corruptRun(t, 8, content, false, nil)
	if bytes.Equal(a1, b1) {
		t.Fatal("different seeds produced identical rot: the flip is not seed-drawn")
	}
}

// TestDSTDiskCorruptTearKeepsCleanPageRot: with CrashTear on and the file fully
// synced, the tear has nothing to draw (no page is dirty) and must not heal the
// rot — a clean page's platter bytes stay rotted through the tear path too.
func TestDSTDiskCorruptTearKeepsCleanPageRot(t *testing.T) {
	content := bytes.Repeat([]byte("T"), 2*4096)
	for seed := uint64(1); seed <= 8; seed++ {
		got := corruptRun(t, seed, content, true, nil)
		if len(got) != len(content) {
			t.Fatalf("seed %d: reboot read %d bytes, want %d (a fully-synced file must not shrink or grow)", seed, len(got), len(content))
		}
		if _, bits := bitDiff(content, got); bits != 1 {
			t.Fatalf("seed %d: corruption = %d bits differing after a tearing crash, want exactly 1", seed, bits)
		}
	}
}

// TestDSTDiskCorruptHealedByRewriteSync: rewriting the whole file with new
// content and syncing rewrites every sector, so the rot is gone — the reboot
// reads the new content byte-exact.
func TestDSTDiskCorruptHealedByRewriteSync(t *testing.T) {
	content := bytes.Repeat([]byte("O"), 2*4096)
	rewritten := bytes.Repeat([]byte("N"), 2*4096)
	got := corruptRun(t, 7, content, false, func(t *testing.T) {
		if err := os.WriteFile("/f", rewritten, 0o644); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		syncFile(t, "/f")
	})
	if !bytes.Equal(got, rewritten) {
		offs, bits := bitDiff(rewritten, got)
		t.Fatalf("rot survived a full rewrite+sync (%d bytes / %d bits differ at %v): writeback must heal the sectors it rewrites", len(offs), bits, offs)
	}
}

// TestDSTDiskCorruptSurvivesAppendSync: the rot lands in the file's original
// (write-once) pages; a later append+sync rewrites only the new tail page, so
// the rot survives — the WAL shape, where corrupting an old frame must not be
// silently healed by the next append's fsync.
func TestDSTDiskCorruptSurvivesAppendSync(t *testing.T) {
	content := bytes.Repeat([]byte("W"), 2*4096) // page-aligned: the append dirties only the new page
	tail := bytes.Repeat([]byte("A"), 4096)
	got := corruptRun(t, 7, content, false, func(t *testing.T) {
		f, err := os.OpenFile("/f", os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("open append: %v", err)
		}
		defer f.Close()
		if _, err := f.Write(tail); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := f.Sync(); err != nil {
			t.Fatalf("sync: %v", err)
		}
	})
	if len(got) != len(content)+len(tail) {
		t.Fatalf("reboot read %d bytes, want %d", len(got), len(content)+len(tail))
	}
	if !bytes.Equal(got[len(content):], tail) {
		t.Fatal("appended page corrupted: the rot was drawn over the pre-append durable image")
	}
	if _, bits := bitDiff(content, got[:len(content)]); bits != 1 {
		t.Fatalf("rot in the write-once pages = %d bits differing, want exactly 1 (an append's sync must not heal sectors it never rewrote)", bits)
	}
}

// TestDSTDiskCorruptVictimIsolation: the rot touches exactly the named file —
// a sibling is byte-exact after the same reboot; the fault follows the file
// across a rename (it keys on the node, as FailFile does); and the no-op
// targets (a missing path, a directory, an empty durable image) neither panic
// nor corrupt anything.
func TestDSTDiskCorruptVictimIsolation(t *testing.T) {
	content := bytes.Repeat([]byte("V"), 4096)
	var gotG, gotSib []byte
	RunWith(7, Options{}, func() {
		Host("h", HostConfig{}, func() {
			go Process("db", func() {
				for _, p := range []string{"/f", "/sib"} {
					if err := os.WriteFile(p, content, 0o644); err != nil {
						t.Errorf("write %s: %v", p, err)
						return
					}
					syncFile(t, p)
				}
				if err := os.WriteFile("/empty", nil, 0o644); err != nil {
					t.Errorf("write empty: %v", err)
					return
				}
				syncFile(t, "/empty")
				syncDir(t, "/")
				select {}
			})
			for range 30 {
				runtime.Gosched()
			}
		})

		CorruptFile("h", "/f")
		CorruptFile("h", "/no-such-file") // no-op: nothing exists to rot
		CorruptFile("h", "/")             // no-op: a directory has no data blocks
		CorruptFile("h", "/empty")        // no-op: an empty durable image has no platter blocks

		// The rot rides the file, not the path.
		Host("h", HostConfig{}, func() {
			if err := os.Rename("/f", "/g"); err != nil {
				t.Fatalf("rename: %v", err)
			}
			syncDir(t, "/")
		})

		CrashHost("h")

		Host("h", HostConfig{}, func() {
			Process("recover", func() {
				var err error
				if gotG, err = os.ReadFile("/g"); err != nil {
					t.Fatalf("read /g after reboot: %v", err)
				}
				if gotSib, err = os.ReadFile("/sib"); err != nil {
					t.Fatalf("read /sib after reboot: %v", err)
				}
			})
		})
	})
	if _, bits := bitDiff(content, gotG); bits != 1 {
		t.Fatalf("renamed victim shows %d differing bits, want 1 (the fault keys on the node)", bits)
	}
	if !bytes.Equal(gotSib, content) {
		t.Fatal("sibling file corrupted: the fault must touch exactly the named file (DST-FAULT-VICTIM)")
	}
}

// TestDSTDiskCorruptNoOpDrawsNothing: the no-op targets (a missing path, a
// directory, an empty durable image) draw nothing from the fault RNG — the
// following real injection lands identically with and without them (a skipped
// target never shifts a later fault's stream).
func TestDSTDiskCorruptNoOpDrawsNothing(t *testing.T) {
	content := bytes.Repeat([]byte("S"), 2*4096)
	plain := corruptRun(t, 9, content, false, nil)
	shifted := corruptRun(t, 9, content, false, nil, "/no-such-file", "/", "/empty")
	if !bytes.Equal(plain, shifted) {
		t.Fatal("a no-op CorruptFile shifted the fault stream: identical seeds must rot identically around a skipped target")
	}
}

// TestDSTDiskCorruptTearDirtyPageComposition: rot composed with a DIRTY page
// under CrashTear. The page's fate draw runs against the unrotted durable
// bytes; where the writeback LANDED (the current outcome, or a torn prefix)
// the sector was rewritten and the rot is gone — rot may survive only in
// durable bytes (the lost outcome, or a torn suffix). A fold applied after
// the page loop would leave flips inside landed bytes: wreckage no real
// writeback-over-rot can produce.
func TestDSTDiskCorruptTearDirtyPageComposition(t *testing.T) {
	const page = 4096
	content := bytes.Repeat([]byte("D"), page)
	dirty := bytes.Repeat([]byte("C"), page)
	sawCleared, sawRot := false, false
	for seed := uint64(1); seed <= 24; seed++ {
		got := corruptRun(t, seed, content, true, func(t *testing.T) {
			if err := os.WriteFile("/f", dirty, 0o644); err != nil {
				t.Fatalf("dirty overwrite: %v", err)
			}
		})
		if len(got) != page {
			t.Fatalf("seed %d: %d bytes, want %d (same-length overwrite draws no size)", seed, len(got), page)
		}
		split := 0
		for split < page && got[split] == 'C' {
			split++
		}
		if split == page {
			sawCleared = true // writeback landed the whole page: rot must be gone
			continue
		}
		rotted := 0
		for i := split; i < page; i++ {
			if got[i] == 'D' {
				continue
			}
			if _, bits := bitDiff([]byte{'D'}, []byte{got[i]}); bits == 1 {
				rotted++
				continue
			}
			t.Fatalf("seed %d: byte %d = %q — neither landed 'C', durable 'D', nor a one-bit rot of 'D'", seed, i, got[i])
		}
		if rotted > 1 {
			t.Fatalf("seed %d: %d rotted bytes, want at most 1", seed, rotted)
		}
		if rotted == 1 {
			sawRot = true
		}
	}
	if !sawCleared || !sawRot {
		t.Fatalf("sweep did not explore the composition (cleared=%v rot-survived=%v): widen the seed range", sawCleared, sawRot)
	}
}

// TestDSTDiskCorruptFsyncgateDroppedKeepsRot: a page a failed writeback
// DROPPED is never written back by the retried sync (fsyncgate), so its
// sectors keep both their stale bytes AND their rot — the retry's commit must
// not heal what it never wrote.
func TestDSTDiskCorruptFsyncgateDroppedKeepsRot(t *testing.T) {
	const page = 4096
	content := bytes.Repeat([]byte("D"), page)
	got := corruptRun(t, 7, content, false, func(t *testing.T) {
		if err := os.WriteFile("/f", bytes.Repeat([]byte("C"), page), 0o644); err != nil {
			t.Fatalf("dirty overwrite: %v", err)
		}
		FailDisk("h")
		f, err := os.OpenFile("/f", os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		if err := f.Sync(); err == nil {
			t.Fatal("sync under FailDisk succeeded, want EIO (the fsyncgate drop)")
		}
		HealDisk("h")
		if err := f.Sync(); err != nil {
			t.Fatalf("retried sync after heal: %v (fsyncgate: the retry succeeds)", err)
		}
	})
	if len(got) != page {
		t.Fatalf("reboot read %d bytes, want %d", len(got), page)
	}
	if _, bits := bitDiff(content, got); bits != 1 {
		t.Fatalf("dropped page shows %d bits differing from the durable content, want exactly 1 (stale bytes + surviving rot; the dirty 'C' bytes never reached the platter)", bits)
	}
}

// TestDSTDiskCorruptAccumulates: two injections, two independent flips — the
// diff against the original content carries exactly two differing bits (seed
// chosen so the draws do not collide; a same-offset-same-bit repeat would
// cancel, the XOR semantics the API documents).
func TestDSTDiskCorruptAccumulates(t *testing.T) {
	content := bytes.Repeat([]byte("K"), 2*4096)
	var got []byte
	RunWith(11, Options{}, func() {
		Host("h", HostConfig{}, func() {
			go Process("db", func() {
				if err := os.WriteFile("/f", content, 0o644); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				syncFile(t, "/f")
				syncDir(t, "/")
				select {}
			})
			for range 30 {
				runtime.Gosched()
			}
		})
		CorruptFile("h", "/f")
		CorruptFile("h", "/f")
		CrashHost("h")
		Host("h", HostConfig{}, func() {
			Process("recover", func() {
				b, err := os.ReadFile("/f")
				if err != nil {
					t.Fatalf("read after reboot: %v", err)
				}
				got = b
			})
		})
	})
	if len(got) != len(content) {
		t.Fatalf("reboot read %d bytes, want %d", len(got), len(content))
	}
	if _, bits := bitDiff(content, got); bits != 2 {
		t.Fatalf("two injections show %d differing bits, want 2", bits)
	}
}

// TestDSTDiskCorruptSameBitCancels: XOR semantics — on a one-byte durable
// image the offset draw is forced, so two injections either pick distinct bits
// (2 differing bits) or the same bit twice (the flips cancel: 0). A sweep must
// see both, and no seed may leave any other count — an accumulate-that-never-
// cancels (OR semantics) shows 1 bit on a colliding seed and fails here.
func TestDSTDiskCorruptSameBitCancels(t *testing.T) {
	content := []byte{'K'}
	sawCancel, sawTwo := false, false
	for seed := uint64(1); seed <= 64; seed++ {
		var got []byte
		RunWith(seed, Options{}, func() {
			Host("h", HostConfig{}, func() {
				go Process("db", func() {
					if err := os.WriteFile("/f", content, 0o644); err != nil {
						t.Errorf("write: %v", err)
						return
					}
					syncFile(t, "/f")
					syncDir(t, "/")
					select {}
				})
				for range 30 {
					runtime.Gosched()
				}
			})
			CorruptFile("h", "/f")
			CorruptFile("h", "/f")
			CrashHost("h")
			Host("h", HostConfig{}, func() {
				Process("recover", func() {
					b, err := os.ReadFile("/f")
					if err != nil {
						t.Fatalf("read after reboot: %v", err)
					}
					got = b
				})
			})
		})
		switch _, bits := bitDiff(content, got); bits {
		case 0:
			sawCancel = true
		case 2:
			sawTwo = true
		default:
			t.Fatalf("seed %d: %d differing bits, want 0 (same bit twice, cancelled) or 2 (distinct bits)", seed, bits)
		}
	}
	if !sawCancel || !sawTwo {
		t.Fatalf("sweep did not reach both outcomes (cancel=%v two=%v): widen the seed range", sawCancel, sawTwo)
	}
}

func TestDSTHostFSSparseAndEpochFencing(t *testing.T) {
	const sparseSize = int64(8) << 30 // 8 GiB sparse file; dense snapshot would OOM
	const tailOff = sparseSize - 4

	var (
		leakedFS         fs.FS
		foreignPanicked  bool
		foreignOpenErr   error
		foreignStatErr   error
		foreignDirErr    error
	)
	foreignInspect := make(chan fs.FS, 1)
	foreignDone := make(chan struct{})
	go func() {
		defer close(foreignDone)
		hfs := <-foreignInspect
		func() {
			defer func() {
				if r := recover(); r != nil {
					msg, ok := r.(string)
					if !ok || !strings.Contains(msg, "outside the run's bubble") {
						panic(r)
					}
					foreignPanicked = true
				}
			}()
			_ = HostFS("h1")
		}()
		_, foreignOpenErr = hfs.Open("wal")
		_, foreignStatErr = fs.Stat(hfs, "wal")
		_, foreignDirErr = fs.ReadDir(hfs, ".")
	}()

	Run(1, func() {
		Host("h1", HostConfig{}, func() {
			f, err := os.Create("/wal")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := f.Truncate(sparseSize); err != nil {
				t.Fatalf("Truncate: %v", err)
			}
			if _, err := f.WriteAt([]byte("head"), 0); err != nil {
				t.Fatalf("WriteAt head: %v", err)
			}
			if _, err := f.WriteAt([]byte("tail"), tailOff); err != nil {
				t.Fatalf("WriteAt tail: %v", err)
			}
			_ = f.Close()
		})

		hfs := HostFS("h1")
		leakedFS = hfs

		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		fi, err := fs.Stat(hfs, "wal")
		if err != nil || fi.Size() != sparseSize {
			t.Fatalf("fs.Stat(HostFS, wal) = %v, size=%d; want nil, %d", err, fi.Size(), sparseSize)
		}
		ents, err := fs.ReadDir(hfs, ".")
		if err != nil || len(ents) == 0 {
			t.Fatalf("fs.ReadDir(HostFS, .) = %v, len=%d", err, len(ents))
		}
		hf, err := hfs.Open("wal")
		if err != nil {
			t.Fatalf("HostFS.Open(wal): %v", err)
		}
		defer hf.Close()
		runtime.ReadMemStats(&m1)
		if delta := int64(m1.TotalAlloc) - int64(m0.TotalAlloc); delta > 16<<20 {
			t.Fatalf("sparse HostFS Stat/ReadDir/Open allocated %d bytes (> 16 MiB); dense hole copy regressed", delta)
		}

		ra, ok := hf.(io.ReaderAt)
		if !ok {
			t.Fatalf("HostFS file does not implement io.ReaderAt")
		}
		sk, ok := hf.(io.Seeker)
		if !ok {
			t.Fatalf("HostFS file does not implement io.Seeker")
		}
		var buf [4]byte
		if n, err := ra.ReadAt(buf[:], 0); n != 4 || err != nil || string(buf[:]) != "head" {
			t.Fatalf("ReadAt(0) = %d, %v, %q; want 4, nil, head", n, err, buf)
		}
		if n, err := ra.ReadAt(buf[:], 1<<30); n != 4 || err != nil || buf != [4]byte{} {
			t.Fatalf("ReadAt(hole) = %d, %v, %v; want zeros", n, err, buf)
		}
		if pos, err := sk.Seek(tailOff, io.SeekStart); err != nil || pos != tailOff {
			t.Fatalf("Seek(tail) = %d, %v", pos, err)
		}
		if n, err := io.ReadFull(hf, buf[:]); n != 4 || err != nil || string(buf[:]) != "tail" {
			t.Fatalf("Read(tail) = %d, %v, %q; want 4, nil, tail", n, err, buf)
		}

		foreignInspect <- hfs
		<-foreignDone
	})

	if !foreignPanicked {
		t.Fatalf("foreign goroutine HostFS(h1) did not panic during active run")
	}
	for name, err := range map[string]error{
		"foreign Open":    foreignOpenErr,
		"foreign Stat":    foreignStatErr,
		"foreign ReadDir": foreignDirErr,
	} {
		if !errors.Is(err, fs.ErrClosed) {
			t.Fatalf("%s = %v, want fs.ErrClosed", name, err)
		}
	}

	// Post-run: leaked HostFS refuses every method with fs.ErrClosed.
	if _, err := leakedFS.Open("wal"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("post-run HostFS.Open = %v, want fs.ErrClosed", err)
	}
	if _, err := fs.Stat(leakedFS, "wal"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("post-run fs.Stat(HostFS) = %v, want fs.ErrClosed", err)
	}
	if _, err := fs.ReadDir(leakedFS, "."); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("post-run fs.ReadDir(HostFS) = %v, want fs.ErrClosed", err)
	}

	// Cross-run: leaked HostFS never aliases Run 2's host at the same ID.
	Run(2, func() {
		Host("h1", HostConfig{}, func() {
			if err := os.WriteFile("/wal", []byte("run2"), 0o644); err != nil {
				t.Fatalf("run2 WriteFile: %v", err)
			}
		})
		if _, err := leakedFS.Open("wal"); !errors.Is(err, fs.ErrClosed) {
			t.Fatalf("cross-run HostFS.Open = %v, want fs.ErrClosed", err)
		}
		if _, err := fs.Stat(leakedFS, "wal"); !errors.Is(err, fs.ErrClosed) {
			t.Fatalf("cross-run fs.Stat(HostFS) = %v, want fs.ErrClosed", err)
		}
		if _, err := fs.ReadDir(leakedFS, "."); !errors.Is(err, fs.ErrClosed) {
			t.Fatalf("cross-run fs.ReadDir(HostFS) = %v, want fs.ErrClosed", err)
		}
	})
}

func TestDSTDiskCorruptOfflineHostSurfacesOnReboot(t *testing.T) {
	want := bytes.Repeat([]byte("platter-sector-"), 64)
	var firstBoot, secondBoot []byte

	Run(11, func() {
		Host("h", HostConfig{}, func() {
			if err := os.WriteFile("/sst", want, 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			syncFile(t, "/sst")
			syncDir(t, "/")
		})

		CrashHost("h")
		// Host "h" is currently powered off; CorruptFile flips a platter bit on the cold disk.
		CorruptFile("h", "/sst")

		Host("h", HostConfig{}, func() {
			b, err := os.ReadFile("/sst")
			if err != nil {
				t.Fatalf("ReadFile on reboot: %v", err)
			}
			firstBoot = b
		})

		// A subsequent crash without rewriting must keep that exact corrupted platter image.
		CrashHost("h")
		Host("h", HostConfig{}, func() {
			b, err := os.ReadFile("/sst")
			if err != nil {
				t.Fatalf("ReadFile on second reboot: %v", err)
			}
			secondBoot = b
		})
	})

	if _, bits := bitDiff(want, firstBoot); bits != 1 {
		t.Fatalf("offline CorruptFile surfaced %d flipped bits on reboot, want 1", bits)
	}
	if !bytes.Equal(firstBoot, secondBoot) {
		t.Fatalf("offline CorruptFile platter state changed across second crash without writes")
	}
}

func TestDSTDiskCorruptSparseHoleNoOpAndZeroBlockMaterialization(t *testing.T) {
	realContent := bytes.Repeat([]byte{0x5A}, 4096)

	runCase := func(corruptSparse bool) (realAfter []byte, sparseBlocks int64) {
		Run(23, func() {
			Host("h", HostConfig{}, func() {
				sf, err := os.Create("/sparse")
				if err != nil {
					t.Fatalf("Create sparse: %v", err)
				}
				if err := sf.Truncate(1 << 30); err != nil {
					t.Fatalf("Truncate sparse: %v", err)
				}
				if err := sf.Sync(); err != nil {
					t.Fatalf("Sync sparse: %v", err)
				}
				_ = sf.Close()

				if err := os.WriteFile("/real", realContent, 0o644); err != nil {
					t.Fatalf("WriteFile real: %v", err)
				}
				syncFile(t, "/real")
				syncDir(t, "/")
			})

			LimitDisk("h", 0)
			if corruptSparse {
				for range 8 {
					CorruptFile("h", "/sparse")
				}
			}
			CorruptFile("h", "/real")
			CrashHost("h")

			Host("h", HostConfig{}, func() {
				var st syscall.Stat_t
				f, err := os.Open("/sparse")
				if err != nil {
					t.Fatalf("Open sparse after crash: %v", err)
				}
				if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
					t.Fatalf("Fstat sparse: %v", err)
				}
				_ = f.Close()
				sparseBlocks = st.Blocks

				b, err := os.ReadFile("/real")
				if err != nil {
					t.Fatalf("ReadFile real: %v", err)
				}
				realAfter = b
			})
		})
		return realAfter, sparseBlocks
	}

	withoutSparse, blocks0 := runCase(false)
	withSparse, blocks1 := runCase(true)
	if blocks0 != 0 || blocks1 != 0 {
		t.Fatalf("all-hole sparse file st_blocks = (%d, %d), want (0, 0)", blocks0, blocks1)
	}
	if _, bits := bitDiff(realContent, withSparse); bits != 1 {
		t.Fatalf("real file bit flips = %d, want 1", bits)
	}
	if !bytes.Equal(withoutSparse, withSparse) {
		t.Fatalf("CorruptFile on all-hole sparse file consumed fault-RNG draws and shifted /real corruption")
	}
}

func TestDSTDiskCorruptCrashTearShrinkHealsTailRot(t *testing.T) {
	initial := bytes.Repeat([]byte{0x3C}, 200)
	overwritten := bytes.Repeat([]byte{0xA5}, 100)

	t.Run("OverwriteAndShrink", func(t *testing.T) {
		sawTailRotBeforeShrink := false
		sawUnlandedShrinkWithLandedPage := false

		for seed := uint64(1); seed <= 256; seed++ {
			// Probe where CorruptFile lands on this seed using a plain untorn crash first.
			probe := corruptRun(t, seed, initial, false, nil)
			rotIdxs, bits := bitDiff(initial, probe)
			if bits != 1 || len(rotIdxs) != 1 || rotIdxs[0] < 100 {
				continue
			}
			rotIdx := rotIdxs[0]
			sawTailRotBeforeShrink = true

			var got []byte
			RunWith(seed, Options{CrashTear: true}, func() {
				Host("h", HostConfig{}, func() {
					f, err := os.OpenFile("/f", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
					if err != nil {
						t.Fatalf("Create: %v", err)
					}
					if _, err := f.Write(initial); err != nil {
						t.Fatalf("Write initial: %v", err)
					}
					if err := f.Sync(); err != nil {
						t.Fatalf("Sync initial: %v", err)
					}
					syncDir(t, "/")

					CorruptFile("h", "/f")

					// Overwrite [0, 100) and shrink to 100 WITHOUT Sync: ftruncate zeroes [100, 4096) in page cache.
					if _, err := f.WriteAt(overwritten, 0); err != nil {
						t.Fatalf("WriteAt overwrite: %v", err)
					}
					if err := f.Truncate(100); err != nil {
						t.Fatalf("Truncate(100): %v", err)
					}
					_ = f.Close()
				})

				CrashHost("h")
				Host("h", HostConfig{}, func() {
					b, err := os.ReadFile("/f")
					if err != nil {
						t.Fatalf("ReadFile after CrashTear: %v", err)
					}
					got = b
				})
			})

			// When size draw kept 200 (shrink metadata did not land) AND all 100 overwritten bytes landed
			// without page-0 tearing inside [0, 200), page 0's 4 KB writeback wrote zeros over [100, 200),
			// healing the pre-existing platter bit-rot at rotIdx.
			if len(got) == 200 && bytes.Equal(got[:100], overwritten) {
				zeros := 0
				for _, b := range got[100:200] {
					if b == 0 {
						zeros++
					}
				}
				if zeros == 100 {
					sawUnlandedShrinkWithLandedPage = true
				} else if zeros == 99 && got[rotIdx] != 0 {
					t.Fatalf("seed %d: dstPageCurrent zeroed [100,200) except stale rot byte %#x at %d", seed, got[rotIdx], rotIdx)
				}
			}
		}

		if !sawTailRotBeforeShrink || !sawUnlandedShrinkWithLandedPage {
			t.Fatalf("seed sweep coverage incomplete: tailRot=%v unlandedShrinkLandedPage=%v",
				sawTailRotBeforeShrink, sawUnlandedShrinkWithLandedPage)
		}
	})

	t.Run("PureShrinkNoWrite", func(t *testing.T) {
		sawLandedZeroTail := false
		sawKeptDurableTail := false
		sawTornZeroPrefix := false

		for seed := uint64(1); seed <= 256; seed++ {
			probe := corruptRun(t, seed, initial, false, nil)
			rotIdxs, bits := bitDiff(initial, probe)
			if bits != 1 || len(rotIdxs) != 1 || rotIdxs[0] < 100 {
				continue
			}
			rotIdx := rotIdxs[0]

			var got []byte
			RunWith(seed, Options{CrashTear: true}, func() {
				Host("h", HostConfig{}, func() {
					f, err := os.OpenFile("/f", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
					if err != nil {
						t.Fatalf("Create: %v", err)
					}
					if _, err := f.Write(initial); err != nil {
						t.Fatalf("Write initial: %v", err)
					}
					if err := f.Sync(); err != nil {
						t.Fatalf("Sync initial: %v", err)
					}
					syncDir(t, "/")

					CorruptFile("h", "/f")

					// Shrink to 100 with NO Write/WriteAt: bytes [0, 100) match durable,
					// while [100, 200) were zeroed in page cache by ftruncate.
					if err := f.Truncate(100); err != nil {
						t.Fatalf("Truncate(100): %v", err)
					}
					_ = f.Close()
				})

				CrashHost("h")
				Host("h", HostConfig{}, func() {
					b, err := os.ReadFile("/f")
					if err != nil {
						t.Fatalf("ReadFile after CrashTear: %v", err)
					}
					got = b
				})
			})

			if len(got) != 200 {
				if len(got) != 100 || !bytes.Equal(got, initial[:100]) {
					t.Fatalf("seed %d: shrunk file = len %d, want 100 bytes of 0x3C", seed, len(got))
				}
				continue
			}
			if !bytes.Equal(got[:100], initial[:100]) {
				t.Fatalf("seed %d: untouched prefix [0, 100) changed", seed)
			}

			zeros := 0
			for zeros < 100 && got[100+zeros] == 0 {
				zeros++
			}
			switch zeros {
			case 0:
				if !bytes.Equal(got[100:200], probe[100:200]) || got[rotIdx] != probe[rotIdx] {
					t.Fatalf("seed %d: durable tail mismatch vs probe", seed)
				}
				sawKeptDurableTail = true
			case 100:
				sawLandedZeroTail = true
			default:
				if !bytes.Equal(got[100+zeros:200], probe[100+zeros:200]) {
					t.Fatalf("seed %d: torn tail suffix [100+%d, 200) did not match durable platter", seed, zeros)
				}
				sawTornZeroPrefix = true
			}
		}

		if !sawLandedZeroTail || !sawKeptDurableTail || !sawTornZeroPrefix {
			t.Fatalf("pure-shrink coverage incomplete: landedZero=%v keptDurable=%v tornZeroPrefix=%v",
				sawLandedZeroTail, sawKeptDurableTail, sawTornZeroPrefix)
		}
	})
}

func TestDSTDiskWritebackMmapSharedRedirty(t *testing.T) {
	t.Run("ExistingWritableSharedMmap", func(t *testing.T) {
		var p0After, p1After byte
		Run(1, func() {
			Host("h", HostConfig{}, func() {
				f, err := os.OpenFile("/db", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
				if err != nil {
					t.Fatalf("OpenFile: %v", err)
				}
				if err := f.Truncate(8192); err != nil {
					t.Fatalf("Truncate: %v", err)
				}
				m, err := syscall.Mmap(int(f.Fd()), 0, 8192, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Fatalf("Mmap: %v", err)
				}
				m[0] = 0x11
				m[4096] = 0x22
				if err := f.Sync(); err != nil {
					t.Fatalf("initial Sync: %v", err)
				}
				syncDir(t, "/")

				FailWriteback("h")
				m[0] = 0x33
				m[4096] = 0x44
				if err := f.Sync(); !errors.Is(err, syscall.EIO) {
					t.Fatalf("Sync under FailWriteback = %v, want EIO", err)
				}

				HealWriteback("h")
				// Mutate page 0 to 0x00 via MAP_SHARED (including zeroing, which SEEK_DATA alone cannot distinguish),
				// while leaving page 1 untouched since the EIO drop.
				m[0] = 0x00
				if err := f.Sync(); err != nil {
					t.Fatalf("Sync after HealWriteback + mmap store: %v", err)
				}
				_ = syscall.Munmap(m)
				_ = f.Close()
			})

			CrashHost("h")
			Host("h", HostConfig{}, func() {
				b, err := os.ReadFile("/db")
				if err != nil {
					t.Fatalf("ReadFile after CrashHost: %v", err)
				}
				p0After, p1After = b[0], b[4096]
			})
		})
		if p0After != 0x00 {
			t.Fatalf("redirtied MAP_SHARED page 0 after CrashHost = %#x, want 0x00", p0After)
		}
		if p1After != 0x22 {
			t.Fatalf("untouched fsyncgate-dropped page 1 after CrashHost = %#x, want 0x22", p1After)
		}
	})

	t.Run("PostEIOMmapAndMprotectUpgrade", func(t *testing.T) {
		var gotMmap, gotMprotect byte
		Run(2, func() {
			Host("h", HostConfig{}, func() {
				for _, name := range []string{"/late-mmap", "/late-mprotect"} {
					f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
					if err != nil {
						t.Fatalf("OpenFile %s: %v", name, err)
					}
					if err := f.Truncate(4096); err != nil {
						t.Fatalf("Truncate %s: %v", name, err)
					}
					if _, err := f.WriteAt([]byte{0x10}, 0); err != nil {
						t.Fatalf("WriteAt initial %s: %v", name, err)
					}
					if err := f.Sync(); err != nil {
						t.Fatalf("Sync initial %s: %v", name, err)
					}
					_ = f.Close()
				}
				syncDir(t, "/")

				// Neither file has ever been mapped writable when FailWriteback drops dirty page 0.
				FailWriteback("h")
				for _, name := range []string{"/late-mmap", "/late-mprotect"} {
					f, err := os.OpenFile(name, os.O_RDWR, 0)
					if err != nil {
						t.Fatalf("reopen %s: %v", name, err)
					}
					if _, err := f.WriteAt([]byte{0x20}, 0); err != nil {
						t.Fatalf("WriteAt EIO %s: %v", name, err)
					}
					if err := f.Sync(); !errors.Is(err, syscall.EIO) {
						t.Fatalf("Sync EIO %s = %v, want EIO", name, err)
					}
					_ = f.Close()
				}
				HealWriteback("h")

				// File 1: mapped PROT_WRITE after the EIO drop.
				f1, err := os.OpenFile("/late-mmap", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("open late-mmap: %v", err)
				}
				m1, err := syscall.Mmap(int(f1.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Fatalf("Mmap late-mmap: %v", err)
				}
				m1[0] = 0x77
				if err := f1.Sync(); err != nil {
					t.Fatalf("Sync late-mmap: %v", err)
				}
				_ = syscall.Munmap(m1)
				_ = f1.Close()

				// File 2: mapped PROT_READ then upgraded via Mprotect(PROT_READ|PROT_WRITE) after the EIO drop.
				f2, err := os.OpenFile("/late-mprotect", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("open late-mprotect: %v", err)
				}
				m2, err := syscall.Mmap(int(f2.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
				if err != nil {
					t.Fatalf("Mmap late-mprotect: %v", err)
				}
				if err := syscall.Mprotect(m2, syscall.PROT_READ|syscall.PROT_WRITE); err != nil {
					t.Fatalf("Mprotect late-mprotect: %v", err)
				}
				m2[0] = 0x88
				if err := f2.Sync(); err != nil {
					t.Fatalf("Sync late-mprotect: %v", err)
				}
				_ = syscall.Munmap(m2)
				_ = f2.Close()
			})

			CrashHost("h")
			Host("h", HostConfig{}, func() {
				b1, err := os.ReadFile("/late-mmap")
				if err != nil {
					t.Fatalf("ReadFile late-mmap: %v", err)
				}
				b2, err := os.ReadFile("/late-mprotect")
				if err != nil {
					t.Fatalf("ReadFile late-mprotect: %v", err)
				}
				gotMmap, gotMprotect = b1[0], b2[0]
			})
		})
		if gotMmap != 0x77 {
			t.Fatalf("post-EIO Mmap PROT_WRITE byte after CrashHost = %#x, want 0x77", gotMmap)
		}
		if gotMprotect != 0x88 {
			t.Fatalf("post-EIO Mprotect PROT_WRITE byte after CrashHost = %#x, want 0x88", gotMprotect)
		}
	})
}

func TestDSTStatfsAndQuotaExploreDPOR(t *testing.T) {
	block := bytes.Repeat([]byte{0x5A}, 4096)

	for _, useFstatfs := range []bool{false, true} {
		name := "StatfsVsFileWrite"
		if useFstatfs {
			name = "FstatfsVsFileWrite"
		}
		t.Run(name, func(t *testing.T) {
			sawBefore := false
			sawAfter := false
			res := Explore(1, DPOR, func() bool {
				var (
					initFree uint64
					obsFree  uint64
				)
				Host("h", HostConfig{}, func() {
					f, err := os.OpenFile("/f", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
					if err != nil {
						t.Fatalf("Create /f: %v", err)
					}
					dir, err := os.Open("/")
					if err != nil {
						t.Fatalf("Open /: %v", err)
					}
					dirFD := int(dir.Fd())

					var base syscall.Statfs_t
					if err := syscall.Statfs("/", &base); err != nil {
						t.Fatalf("initial Statfs: %v", err)
					}
					initFree = base.Bfree

					wCh := make(chan error, 1)
					sCh := make(chan uint64, 1)
					go func() {
						_, err := f.Write(block)
						wCh <- err
					}()
					go func() {
						var st syscall.Statfs_t
						var err error
						if useFstatfs {
							err = syscall.Fstatfs(dirFD, &st)
						} else {
							err = syscall.Statfs("/", &st)
						}
						if err != nil {
							panic(err)
						}
						sCh <- st.Bfree
					}()
					if err := <-wCh; err != nil {
						t.Fatalf("Write /f: %v", err)
					}
					obsFree = <-sCh
					_ = dir.Close()
					_ = f.Close()
				})
				switch obsFree {
				case initFree:
					sawBefore = true
				case initFree - 1:
					sawAfter = true
				default:
					t.Fatalf("observed Bfree = %d, want %d or %d", obsFree, initFree, initFree-1)
				}
				return false
			})
			if len(res.Failures) != 0 || res.Overflow {
				t.Fatalf("Explore result = %+v", res)
			}
			if !sawBefore || !sawAfter {
				t.Fatalf("DPOR missed Statfs/Write reversal (%s): sawBefore=%v sawAfter=%v (schedules=%d)",
					name, sawBefore, sawAfter, res.Schedules)
			}
		})
	}

	t.Run("CappedDiskCompetingWrites", func(t *testing.T) {
		sawAWins := false
		sawBWins := false
		res := Explore(1, DPOR, func() bool {
			var errA, errB error
			Host("h", HostConfig{}, func() {
				fa, err := os.OpenFile("/a", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
				if err != nil {
					t.Fatalf("Create /a: %v", err)
				}
				fb, err := os.OpenFile("/b", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
				if err != nil {
					t.Fatalf("Create /b: %v", err)
				}
				LimitDisk("h", 4096)

				chA := make(chan error, 1)
				chB := make(chan error, 1)
				go func() {
					_, e := fa.Write(block)
					chA <- e
				}()
				go func() {
					_, e := fb.Write(block)
					chB <- e
				}()
				errA = <-chA
				errB = <-chB
				_ = fa.Close()
				_ = fb.Close()
			})
			switch {
			case errA == nil && errors.Is(errB, syscall.ENOSPC):
				sawAWins = true
			case errors.Is(errA, syscall.ENOSPC) && errB == nil:
				sawBWins = true
			default:
				t.Fatalf("unexpected quota race outcome: errA=%v errB=%v", errA, errB)
			}
			return false
		})
		if len(res.Failures) != 0 || res.Overflow {
			t.Fatalf("Explore result = %+v", res)
		}
		if !sawAWins || !sawBWins {
			t.Fatalf("DPOR missed capped-disk ENOSPC reversal: sawAWins=%v sawBWins=%v (schedules=%d)",
				sawAWins, sawBWins, res.Schedules)
		}
	})
}
