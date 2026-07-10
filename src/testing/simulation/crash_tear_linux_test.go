// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
)

// tornRun writes a durable prefix and an unsynced suffix on host "h", crashes
// the machine with CrashTear on, and returns what the reboot finds.
//
// Layout: a 3-page file whose first page is durable ("D"*4096) and whose
// remaining two pages were written after the last fsync ("U"*8192). Under the
// contract, page 0 must survive byte-exactly; pages 1 and 2 may each be absent
// (zeros — never written to the platter), present, or torn.
func tornRun(t *testing.T, seed uint64) (content []byte, dirEntries []string) {
	t.Helper()
	const page = 4096
	durable := bytes.Repeat([]byte("D"), page)
	unsynced := bytes.Repeat([]byte("U"), 2*page)

	RunWith(seed, Options{CrashTear: true}, func() {
		Host("h", HostConfig{}, func() {
			go Process("db", func() {
				f, err := os.Create("/f")
				if err != nil {
					t.Errorf("create: %v", err)
					return
				}
				if _, err := f.Write(durable); err != nil {
					t.Errorf("write durable: %v", err)
					return
				}
				if err := f.Sync(); err != nil {
					t.Errorf("sync: %v", err)
					return
				}
				syncDir(t, "/") // the name is durable too
				if _, err := f.Write(unsynced); err != nil {
					t.Errorf("write unsynced: %v", err)
					return
				}
				f.Close()

				// Two unsynced creates: each independently lands or does not.
				os.WriteFile("/a", []byte("a"), 0o644)
				os.WriteFile("/b", []byte("b"), 0o644)
				select {}
			})
			for range 30 {
				runtime.Gosched()
			}
		})

		CrashHost("h")

		Host("h", HostConfig{}, func() {
			Process("recover", func() {
				b, err := os.ReadFile("/f")
				if err != nil {
					t.Fatalf("read after reboot: %v", err)
				}
				content = b
				des, err := os.ReadDir("/")
				if err != nil {
					t.Fatalf("readdir after reboot: %v", err)
				}
				for _, de := range des {
					dirEntries = append(dirEntries, de.Name())
				}
			})
		})
	})
	return content, dirEntries
}

// TestDSTCrashTearRespectsDurableBytes: whatever a torn crash does, it never
// touches a byte that fsync committed, and never invents a byte no write
// produced. Over a sweep of seeds, page 0 is always "D"*4096, and every byte of
// pages 1-2 is either 'U' (the write landed) or 0 (it did not) — never a
// partially-overwritten older value, and never a byte beyond the file's
// pre-crash length.
func TestDSTCrashTearRespectsDurableBytes(t *testing.T) {
	const page = 4096
	sawShort, sawFull, sawTorn := false, false, false
	for seed := uint64(1); seed <= 24; seed++ {
		content, _ := tornRun(t, seed)
		if len(content) < page {
			t.Fatalf("seed %d: file shrank below its durable size: %d bytes", seed, len(content))
		}
		if len(content) > 3*page {
			t.Fatalf("seed %d: file grew past its pre-crash size: %d bytes", seed, len(content))
		}
		for i := 0; i < page; i++ {
			if content[i] != 'D' {
				t.Fatalf("seed %d: durable byte %d = %q, want 'D' (fsync'd bytes are stable)", seed, i, content[i])
			}
		}
		for i := page; i < len(content); i++ {
			if content[i] != 'U' && content[i] != 0 {
				t.Fatalf("seed %d: unsynced byte %d = %q, want 'U' (landed) or 0 (did not)", seed, i, content[i])
			}
		}
		switch {
		case len(content) == page:
			sawShort = true
		case bytes.Equal(content[page:], bytes.Repeat([]byte("U"), len(content)-page)):
			sawFull = true
		}
		// A byte-granular tear: some page that is neither wholly landed nor
		// wholly absent — 'U' bytes and 0 bytes inside ONE page, the sector
		// caught in flight. Page-level mixture (one page landed, another not) is
		// a different outcome and does not count here.
		for start := page; start < len(content); start += page {
			end := min(start+page, len(content))
			p := content[start:end]
			if bytes.Contains(p, []byte("U")) && bytes.Contains(p, []byte{0}) {
				sawTorn = true
			}
		}
	}
	// The sweep must actually explore the outcome space, or the tear is a no-op
	// dressed as a fault.
	if !sawShort || !sawFull || !sawTorn {
		t.Fatalf("seed sweep did not reach every outcome: lost-size=%v all-landed=%v byte-torn=%v", sawShort, sawFull, sawTorn)
	}
}

// TestDSTCrashTearEntriesSubset: unsynced directory entries land independently,
// so a crash can persist one file of a two-file write and lose the other — the
// interleaving a crash-consistency bug hides in. The durable name always
// survives.
func TestDSTCrashTearEntriesSubset(t *testing.T) {
	seen := map[string]bool{}
	for seed := uint64(1); seed <= 24; seed++ {
		_, entries := tornRun(t, seed)
		set := map[string]bool{}
		for _, e := range entries {
			set[e] = true
		}
		if !set["f"] {
			t.Fatalf("seed %d: durable name /f vanished", seed)
		}
		key := ""
		if set["a"] {
			key += "a"
		}
		if set["b"] {
			key += "b"
		}
		seen[key] = true
	}
	// Both files, neither, and exactly one must all be reachable across seeds.
	if len(seen) < 3 {
		t.Fatalf("unsynced entries did not vary independently across seeds: outcomes %v", seen)
	}
}

// TestDSTCrashTearReplays: a torn crash is a deterministic function of the seed
// — same seed, byte-identical wreckage — and different seeds tear differently.
// This is DST-FAULT-REPLAY for the disk axis: a crash-recovery bug found once
// can be re-run forever.
func TestDSTCrashTearReplays(t *testing.T) {
	a1, e1 := tornRun(t, 7)
	a2, e2 := tornRun(t, 7)
	if !bytes.Equal(a1, a2) {
		t.Fatalf("same seed produced different content: %d vs %d bytes", len(a1), len(a2))
	}
	if len(e1) != len(e2) {
		t.Fatalf("same seed produced different entries: %v vs %v", e1, e2)
	}
	for i := range e1 {
		if e1[i] != e2[i] {
			t.Fatalf("same seed produced different entries: %v vs %v", e1, e2)
		}
	}
	differs := false
	for seed := uint64(1); seed <= 24 && !differs; seed++ {
		b, _ := tornRun(t, seed)
		if !bytes.Equal(a1, b) {
			differs = true
		}
	}
	if !differs {
		t.Fatalf("every seed tore identically: the tear does not draw from the fault RNG")
	}
}

// TestDSTCrashTearOffIsAllOrNothing: with CrashTear off (the default), a host
// crash loses every unsynced byte and every unsynced name — the deterministic
// outcome the rest of the suite relies on.
func TestDSTCrashTearOffIsAllOrNothing(t *testing.T) {
	const page = 4096
	var content []byte
	var names []string
	Run(1, func() {
		Host("h", HostConfig{}, func() {
			go Process("db", func() {
				f, _ := os.Create("/f")
				f.Write(bytes.Repeat([]byte("D"), page))
				f.Sync()
				syncDir(t, "/")
				f.Write(bytes.Repeat([]byte("U"), page))
				f.Close()
				os.WriteFile("/a", []byte("a"), 0o644)
				select {}
			})
			for range 30 {
				runtime.Gosched()
			}
		})
		CrashHost("h")
		Host("h", HostConfig{}, func() {
			Process("recover", func() {
				content, _ = os.ReadFile("/f")
				des, _ := os.ReadDir("/")
				for _, de := range des {
					names = append(names, de.Name())
				}
			})
		})
	})
	if len(content) != page || !bytes.Equal(content, bytes.Repeat([]byte("D"), page)) {
		t.Fatalf("untorn crash content = %d bytes, want exactly the durable page", len(content))
	}
	for _, n := range names {
		if n == "a" {
			t.Fatalf("untorn crash kept an unsynced name")
		}
	}
}

// crashTearProbe runs one host crash with an unsynced write and reports whether
// the byte survived — i.e. whether this run tore, or lost everything unsynced.
// It synchronizes on a channel rather than spinning on Gosched: under an
// exploration strategy the scheduler chooses, and a spin can starve the writer.
func crashTearProbe() (survived bool) {
	written := make(chan struct{})
	Host("h", HostConfig{}, func() {
		go Process("db", func() {
			f, err := os.Create("/p")
			if err == nil {
				f.Write(bytes.Repeat([]byte("U"), 8))
				f.Close()
			}
			close(written)
			select {}
		})
		<-written
	})
	CrashHost("h")
	Host("h", HostConfig{}, func() {
		Process("recover", func() {
			b, err := os.ReadFile("/p")
			survived = err == nil && len(b) > 0
		})
	})
	return survived
}

// TestDSTCrashTearDoesNotLeakAcrossRuns: the tear policy is per-run. A run that
// did not ask for it never gets one, whichever entry point it came through —
// Explore and Replay take no Options, so a stale policy from a previous torn run
// must not follow them. (An unsynced create's name is durable only by chance
// under a tear, so this probes the file's very existence.)
func TestDSTCrashTearDoesNotLeakAcrossRuns(t *testing.T) {
	// A torn run first: it may or may not keep the byte, but it arms the policy.
	RunWith(3, Options{CrashTear: true}, func() { crashTearProbe() })

	// A plain Run must lose everything unsynced, deterministically.
	Run(3, func() {
		if crashTearProbe() {
			t.Errorf("plain Run inherited the previous run's crash-tear policy")
		}
	})

	// So must an Explore that did not ask for it.
	res := Explore(3, Exhaustive, func() bool {
		return crashTearProbe() // a "failure" means the byte survived
	})
	if len(res.Failures) != 0 {
		t.Fatalf("Explore inherited the previous run's crash-tear policy")
	}

	// And an Explore that DOES ask for it can tear: over enough seeds some
	// schedule keeps the unsynced byte (each surviving run is reported as a
	// "failure" by the probe's contract).
	torn := false
	for seed := uint64(1); seed <= 40 && !torn; seed++ {
		r := ExploreWith(seed, ExploreOptions{CrashTear: true, MaxSchedules: 1}, func() bool {
			return crashTearProbe()
		})
		if len(r.Failures) != 0 {
			torn = true
		}
	}
	if !torn {
		t.Fatalf("ExploreWith(CrashTear: true) never tore: the policy does not reach exploration")
	}
}

// TestDSTCrashTearFailureReplays: a failure found by a TORN exploration carries
// the crash policy that shaped it, so Replay reproduces it. Without that, the
// replay would restore a different disk (the untorn one) and the bug would
// vanish — the "found once, re-runnable forever" promise of DST-FAULT-REPLAY.
func TestDSTCrashTearFailureReplays(t *testing.T) {
	var seed uint64
	var failure Failure
	for s := uint64(1); s <= 40 && failure.Schedule == nil && !failure.CrashTear; s++ {
		r := ExploreWith(s, ExploreOptions{CrashTear: true, MaxSchedules: 1}, func() bool {
			return crashTearProbe() // "fails" when the unsynced byte survives
		})
		if len(r.Failures) > 0 {
			seed, failure = s, r.Failures[0]
		}
	}
	if !failure.CrashTear {
		t.Fatalf("no torn failure found to replay, or the policy was not recorded in the Failure")
	}
	failed, _ := Replay(seed, failure, func() bool { return crashTearProbe() })
	if !failed {
		t.Fatalf("replaying a torn failure did not reproduce it: the crash policy was not restored")
	}
	// And a failure recorded WITHOUT the tear replays untorn: the byte stays lost.
	untorn := failure
	untorn.CrashTear = false
	failedUntorn, _ := Replay(seed, untorn, func() bool { return crashTearProbe() })
	if failedUntorn {
		t.Fatalf("replaying with CrashTear cleared still tore the disk")
	}
}

// TestDSTCrashTearTruncateDown: an unsynced SHRINK either landed or did not. If
// it did not, the file keeps its durable length and its durable tail bytes —
// never a shorter file with live bytes past the end, and never a tail of
// unwritten garbage.
func TestDSTCrashTearTruncateDown(t *testing.T) {
	const page = 4096
	sawKept, sawShrunk := false, false
	for seed := uint64(1); seed <= 24; seed++ {
		var got []byte
		RunWith(seed, Options{CrashTear: true}, func() {
			done := make(chan struct{})
			Host("h", HostConfig{}, func() {
				go Process("db", func() {
					f, err := os.Create("/t")
					if err == nil {
						f.Write(bytes.Repeat([]byte("D"), 2*page))
						f.Sync()
						f.Close()
					}
					syncDir(t, "/")
					os.Truncate("/t", page) // unsynced shrink
					close(done)
					select {}
				})
				<-done
			})
			CrashHost("h")
			Host("h", HostConfig{}, func() {
				Process("recover", func() { got, _ = os.ReadFile("/t") })
			})
		})
		switch len(got) {
		case 2 * page:
			sawKept = true
			if !bytes.Equal(got, bytes.Repeat([]byte("D"), 2*page)) {
				t.Fatalf("seed %d: unshrunk file lost durable bytes", seed)
			}
		case page:
			sawShrunk = true
			if !bytes.Equal(got, bytes.Repeat([]byte("D"), page)) {
				t.Fatalf("seed %d: shrunk file lost durable bytes", seed)
			}
		default:
			t.Fatalf("seed %d: file length %d, want %d or %d", seed, len(got), page, 2*page)
		}
	}
	if !sawKept || !sawShrunk {
		t.Fatalf("truncate-down sweep did not reach both outcomes: kept=%v shrunk=%v", sawKept, sawShrunk)
	}
}

// TestDSTCrashTearRenameDoubleLinkRestoredOnce: a rename whose source and
// destination directories were never fsynced can leave the inode linked from
// BOTH on the platter (each link lands independently). The restore must draw for
// that inode exactly once — it mutates nodes in place, so a second visit would
// read already-restored bytes as page cache and tear them again. Both links must
// therefore show identical content.
func TestDSTCrashTearRenameDoubleLinkRestoredOnce(t *testing.T) {
	const page = 4096
	for seed := uint64(1); seed <= 24; seed++ {
		var src, dst []byte
		var srcErr, dstErr error
		RunWith(seed, Options{CrashTear: true}, func() {
			done := make(chan struct{})
			Host("h", HostConfig{}, func() {
				go Process("db", func() {
					os.Mkdir("/a", 0o755)
					os.Mkdir("/b", 0o755)
					f, err := os.Create("/a/f")
					if err == nil {
						f.Write(bytes.Repeat([]byte("D"), page))
						f.Sync()
						f.Close()
					}
					syncDir(t, "/")
					syncDir(t, "/a") // /a/f's name is durable
					// Unsynced rename and unsynced overwrite: either link may land.
					os.Rename("/a/f", "/b/f")
					if g, err := os.OpenFile("/b/f", os.O_RDWR, 0); err == nil {
						g.WriteAt(bytes.Repeat([]byte("U"), 16), 0)
						g.Close()
					}
					close(done)
					select {}
				})
				<-done
			})
			CrashHost("h")
			Host("h", HostConfig{}, func() {
				Process("recover", func() {
					src, srcErr = os.ReadFile("/a/f")
					dst, dstErr = os.ReadFile("/b/f")
				})
			})
		})
		if srcErr == nil && dstErr == nil && !bytes.Equal(src, dst) {
			t.Fatalf("seed %d: one inode reachable from two links restored to two different images (torn twice)", seed)
		}
		for _, b := range [][]byte{src, dst} {
			for i, c := range b {
				if c != 'D' && c != 'U' {
					t.Fatalf("seed %d: byte %d = %q, want a durable or a written byte", seed, i, c)
				}
			}
		}
	}
}

// dstTearPolicySeed: a seed whose two-dirty-page tear draw is MIXED (one page
// live, one durable) — the strongest anchor: it proves the page-granular tear
// ran, not merely that the policy stayed true.
const dstTearPolicySeed = 3

// TestDSTRejectedNestedRunKeepsCrashTearPolicy: a rejected Run (nested inside
// an active run) leaves every process-global policy untouched — options apply
// only after the run is admitted. Before the guard-then-publish ordering, the
// nested attempt flipped the ACTIVE run's crash-tear policy before its panic,
// and a CrashTear seed sweep silently swept untorn crashes for the rest of the
// run.
func TestDSTRejectedNestedRunKeepsCrashTearPolicy(t *testing.T) {
	var nestedPanicked bool
	var recovered string
	TestWith(t, dstTearPolicySeed, Options{CrashTear: true}, func(t *testing.T) {
		func() {
			defer func() {
				r := recover()
				nestedPanicked = r != nil && strings.Contains(fmt.Sprint(r), "testing/simulation:")
			}()
			RunWith(1, Options{CrashTear: false}, func() {})
		}()

		Host("h", HostConfig{}, func() {
			go Process("db", func() {
				f, err := os.Create("/tmp/state")
				if err != nil {
					t.Errorf("create: %v", err)
					return
				}
				durable := make([]byte, 8<<10)
				for i := range durable {
					durable[i] = 'd'
				}
				f.Write(durable)
				f.Sync()
				syncDir(t, "/tmp")
				unsynced := make([]byte, 8<<10)
				for i := range unsynced {
					unsynced[i] = 'u'
				}
				f.WriteAt(unsynced, 0) // live diverges from durable: tear material
				f.Close()
				select {} // dies with the machine
			})
			for range 30 {
				runtime.Gosched()
			}
		})

		CrashHost("h")

		Host("h", HostConfig{}, func() {
			Process("db", func() {
				b, err := os.ReadFile("/tmp/state")
				if err != nil {
					t.Fatalf("read after crash: %v", err)
				}
				recovered = string(b)
			})
		})
	})
	if !nestedPanicked {
		t.Fatal("nested RunWith inside an active run did not panic")
	}
	if !strings.Contains(recovered, "u") {
		t.Fatalf("no torn (live) bytes survived the crash — the rejected nested Run flipped the active run's CrashTear policy to false")
	}
	if !strings.Contains(recovered, "d") {
		t.Fatalf("no durable bytes present after the torn crash — unexpected image %.16q...", recovered)
	}
}
