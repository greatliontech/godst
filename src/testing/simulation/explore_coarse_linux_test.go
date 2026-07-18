// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"os"
	"syscall"
	"testing"
)

// Coarse cross-process dependency model acceptance (exploration.md,
// "Coarse cross-process dependencies"): WITHOUT -race, DPOR must explore
// both orders of two processes conflicting through a simulated
// filesystem node, a flock, and a shared futex word — the exact
// multi-process channels the motivating probes showed collapsing into
// one falsely-preclaimed class. Each test asserts the OUTCOME SET, the
// strongest observable: both orders genuinely ran. The tests are
// NON-RACE-ONLY by design: the coarse model exists for the build the
// fine-grained instrumentation cannot serve, and under -race that
// instrumentation multiplies the decision tree past any small budget
// (the race build's conflict coverage is the access relation itself,
// enforced elsewhere) — skipped there with this reason, never silently.

func skipUnderRace(t *testing.T) {
	t.Helper()
	if dstRaceEnabledFP() {
		t.Skip("coarse-model acceptance targets the non-race build; -race's access instrumentation subsumes these conflicts at finer grain")
	}
}

// TestDSTExploreCoarseFSOrder: two processes write one path; the final
// content is order-determined. The pre-model behavior was 1 schedule,
// Exhausted=true, one outcome.
func TestDSTExploreCoarseFSOrder(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	skipUnderRace(t)
	outcomes := map[string]bool{}
	sut := func() bool {
		done := make(chan struct{}, 2)
		Host("h", HostConfig{}, func() {
			go Process("a", func() { os.WriteFile("/f", []byte("A"), 0o600); done <- struct{}{} })
			go Process("b", func() { os.WriteFile("/f", []byte("B"), 0o600); done <- struct{}{} })
			<-done
			<-done
			Process("obs", func() {
				b, _ := os.ReadFile("/f")
				outcomes[string(b)] = true
			})
		})
		return false
	}
	res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 500}, sut)
	t.Logf("schedules=%d exhausted=%v uninstrumented=%v outcomes=%v", res.Schedules, res.Exhausted, res.Uninstrumented, outcomes)
	if res.Uninstrumented {
		t.Fatal("coarse FS dependencies fired no events — Uninstrumented must be false")
	}
	if !outcomes["A"] || !outcomes["B"] {
		t.Fatalf("FS write order not explored: outcomes %v", outcomes)
	}
}

// TestDSTExploreCoarseFlockWinner: two processes race a nonblocking
// LOCK_EX on one file; which one collects EWOULDBLOCK is
// order-determined.
func TestDSTExploreCoarseFlockWinner(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	skipUnderRace(t)
	outcomes := map[string]bool{}
	sut := func() bool {
		results := make(chan string, 2)
		release := make(chan struct{})
		Host("h", HostConfig{}, func() {
			Process("setup", func() {
				if err := os.WriteFile("/l", nil, 0o600); err != nil {
					panic(err)
				}
			})
			// A winner HOLDS its lock (process alive, fd open) until both
			// contenders have reported: a winner whose process exits
			// releases the lock, making serial both-won a LEGITIMATE
			// execution that would mask the race this test explores.
			contend := func(name string) {
				f, err := os.OpenFile("/l", os.O_RDWR, 0)
				if err != nil {
					panic(err)
				}
				err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				if err == syscall.EWOULDBLOCK {
					f.Close()
					results <- name + "-lost"
					return
				}
				if err != nil {
					panic(err)
				}
				results <- name + "-won"
				<-release
			}
			go Process("a", func() { contend("a") })
			go Process("b", func() { contend("b") })
			r1, r2 := <-results, <-results
			close(release)
			outcomes[r1+"/"+r2] = true
		})
		return false
	}
	res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 500}, sut)
	t.Logf("schedules=%d exhausted=%v uninstrumented=%v outcomes=%v", res.Schedules, res.Exhausted, res.Uninstrumented, outcomes)
	// Result-channel arrival order varies too; the load-bearing claims:
	// no schedule saw two winners (exclusivity under overlap), and BOTH
	// contenders win in some explored schedule (the race is explored).
	aWins, bWins := false, false
	for o := range outcomes {
		switch o {
		case "a-won/b-won", "b-won/a-won":
			t.Fatalf("both contenders won overlapping exclusive locks: outcomes %v", outcomes)
		case "a-won/b-lost", "b-lost/a-won":
			aWins = true
		case "b-won/a-lost", "a-lost/b-won":
			bWins = true
		}
	}
	if !aWins || !bWins {
		t.Fatalf("flock winner not explored both ways: outcomes %v", outcomes)
	}
}

// TestDSTExploreCoarseFutexLostWakeWindow: a waiter's value-check races a
// peer's store+wake; whether the waiter parks-and-wakes (0) or sees the
// new value (EAGAIN) is order-determined.
func TestDSTExploreCoarseFutexLostWakeWindow(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	skipUnderRace(t)
	outcomes := map[string]bool{}
	sut := func() bool {
		done := make(chan struct{}, 2)
		Host("h", HostConfig{}, func() {
			Process("setup", func() {
				f, err := os.Create("/w")
				if err != nil {
					panic(err)
				}
				defer f.Close()
				if err := f.Truncate(4096); err != nil {
					panic(err)
				}
			})
			// Mappings are per-process (address-space isolation): each
			// side maps the shared file itself; the futex queue keys on
			// (node, offset), so the two mappings meet at one word.
			mapWord := func() *uint32 {
				f, err := os.OpenFile("/w", os.O_RDWR, 0)
				if err != nil {
					panic(err)
				}
				defer f.Close()
				data, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					panic(err)
				}
				return (*uint32)(mmapWordPtr(data))
			}
			go Process("waiter", func() {
				word := mapWord()
				_, _, e := syscall.Syscall6(syscall.SYS_FUTEX, mmapWordUintptr(word), 0 /*FUTEX_WAIT*/, 0, 0, 0, 0)
				if e == syscall.EAGAIN {
					outcomes["eagain"] = true
				} else if e == 0 {
					outcomes["woken"] = true
				}
				done <- struct{}{}
			})
			go Process("waker", func() {
				word := mapWord()
				storeWord(word, 1)
				syscall.Syscall6(syscall.SYS_FUTEX, mmapWordUintptr(word), 1 /*FUTEX_WAKE*/, 1, 0, 0, 0)
				done <- struct{}{}
			})
			<-done
			<-done
		})
		return false
	}
	res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 500}, sut)
	t.Logf("schedules=%d exhausted=%v uninstrumented=%v outcomes=%v", res.Schedules, res.Exhausted, res.Uninstrumented, outcomes)
	if !outcomes["eagain"] || !outcomes["woken"] {
		t.Fatalf("futex wait/wake order not explored both ways: outcomes %v", outcomes)
	}
}

// TestDSTExploreCoarseNamespaceOrder: a rename races a stat of the new
// name — a conflict carried ONLY by the namespace identity (no data
// I/O on a shared node), so it pins the per-host namespace announce
// specifically.
func TestDSTExploreCoarseNamespaceOrder(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	skipUnderRace(t)
	outcomes := map[bool]bool{}
	sut := func() bool {
		done := make(chan struct{}, 2)
		Host("h", HostConfig{}, func() {
			Process("setup", func() {
				if err := os.WriteFile("/x", []byte("v"), 0o600); err != nil {
					panic(err)
				}
			})
			go Process("mover", func() {
				// Through os.Root, so the Root family's announces have a
				// kill-path too (they are separate implementations from
				// the plain named ops).
				root, err := os.OpenRoot("/")
				if err != nil {
					panic(err)
				}
				defer root.Close()
				if err := root.Rename("x", "y"); err != nil {
					panic(err)
				}
				done <- struct{}{}
			})
			go Process("prober", func() {
				_, err := os.Stat("/y")
				outcomes[err == nil] = true
				done <- struct{}{}
			})
			<-done
			<-done
		})
		return false
	}
	res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 500}, sut)
	t.Logf("schedules=%d exhausted=%v uninstrumented=%v outcomes=%v", res.Schedules, res.Exhausted, res.Uninstrumented, outcomes)
	if !outcomes[true] || !outcomes[false] {
		t.Fatalf("rename/stat namespace order not explored both ways: outcomes %v", outcomes)
	}
}

// TestDSTExploreCoarseFlockCloseRelease: the holder's Close releases its
// exclusive flock; a contender's nonblocking attempt lands on either
// side of that release — the close-path announce (dstFlockReleaseFD) is
// what makes both orders explorable.
func TestDSTExploreCoarseFlockCloseRelease(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	skipUnderRace(t)
	outcomes := map[string]bool{}
	sut := func() bool {
		locked := make(chan struct{})
		attempted := make(chan string, 1)
		Host("h", HostConfig{}, func() {
			Process("setup", func() {
				if err := os.WriteFile("/l", nil, 0o600); err != nil {
					panic(err)
				}
			})
			go Process("holder", func() {
				f, err := os.OpenFile("/l", os.O_RDWR, 0)
				if err != nil {
					panic(err)
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					panic(err)
				}
				close(locked)
				f.Close() // releases the flock — the racing decision
			})
			Process("contender", func() {
				<-locked
				f, err := os.OpenFile("/l", os.O_RDWR, 0)
				if err != nil {
					panic(err)
				}
				defer f.Close()
				err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				if err == syscall.EWOULDBLOCK {
					attempted <- "blocked"
				} else if err == nil {
					attempted <- "granted"
				} else {
					panic(err)
				}
			})
			outcomes[<-attempted] = true
		})
		return false
	}
	res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 500}, sut)
	t.Logf("schedules=%d exhausted=%v uninstrumented=%v outcomes=%v", res.Schedules, res.Exhausted, res.Uninstrumented, outcomes)
	if !outcomes["blocked"] || !outcomes["granted"] {
		t.Fatalf("close-release order not explored both ways: outcomes %v", outcomes)
	}
}
