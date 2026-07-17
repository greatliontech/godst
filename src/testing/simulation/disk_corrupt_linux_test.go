// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"bytes"
	"os"
	"runtime"
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
