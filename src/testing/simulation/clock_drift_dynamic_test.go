// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"math/big"
	"math/rand"
	"testing"
	"time"
	_ "unsafe" // for go:linkname
)

//go:linkname dstDriftRemap runtime.dstDriftRemap
func dstDriftRemap(x, ppbOld, ppbNew int64) int64

// These tests exercise the MID-RUN clock rate change (DriftClock, D2). The defining
// behaviors beyond constant-rate Drift (clock_drift_test.go):
//   - changing a host's rate mid-run re-maps every PENDING timer of that host so it still
//     fires after the host-perceived time it was set for: when' = T + (when-T)*r_old/r_new;
//   - the wall stays continuous across the change (no jump);
//   - timers armed after the change use the new rate; a re-sync (DriftClock(h,0)) restores
//     rate 1; changes compose; isolate per host; replay deterministically.
// All base-time measurements are on the root (host 0, rate 1).

// bigCeilQuo = ceil(a/b) for positive a, b — the rounding the arm/re-map conversions
// use (the rounding contract: never early in host-perceived time).
func bigCeilQuo(a, b *big.Int) int64 {
	q, r := new(big.Int).QuoRem(a, b, new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

// bigToBase = ceil(d*1e9/(1e9+ppb)) — the arm conversion, as an independent oracle.
func bigToBase(d, ppb int64) int64 {
	return bigCeilQuo(new(big.Int).Mul(big.NewInt(d), big.NewInt(1_000_000_000)), big.NewInt(1_000_000_000+ppb))
}

// bigRemap = ceil(x*(1e9+ppbOld)/(1e9+ppbNew)) — the pending-timer re-map oracle.
func bigRemap(x, ppbOld, ppbNew int64) int64 {
	return bigCeilQuo(new(big.Int).Mul(big.NewInt(x), big.NewInt(1_000_000_000+ppbOld)), big.NewInt(1_000_000_000+ppbNew))
}

// driftClockReSleep arms a Sleep(d) on host h (initially rate ppbOld) at base ~0, lets the
// root advance base to T (< the sleep's firing), changes h's rate to ppbNew, and returns
// the base instant the sleep actually finished — measured on the root.
func driftClockReSleep(t *testing.T, ppbOld, ppbNew int64, d, T time.Duration) time.Duration {
	t.Helper()
	var total time.Duration
	Run(1, func() {
		armed := make(chan struct{})
		done := make(chan struct{})
		Host("h", HostConfig{Clock: Drift(ppbOld)}, func() {
			go func() {
				close(armed) // about to arm the sleep at base ~0
				time.Sleep(d)
				close(done)
			}()
		})
		<-armed
		start := time.Now() // root: the arm instant (no base advance over the handoff)
		time.Sleep(T)       // root advances base to T (< the sleep's firing)
		DriftClock("h", ppbNew)
		<-done
		total = time.Since(start)
	})
	return total
}

// TestDSTClockDriftClockPendingTimer is the core D2 check: a timer armed under the old
// rate is re-mapped when the rate changes before it fires. A 1s sleep armed at rate 1
// (fires at base 1s); at base 0.5s switch to rate 2; the remaining 0.5s base = 0.5s host
// now takes 0.25s base, so it fires at 0.75s.
func TestDSTClockDriftClockPendingTimer(t *testing.T) {
	for _, tc := range []struct {
		name           string
		ppbOld, ppbNew int64
		d, T, want     time.Duration
	}{
		{"1to2", 0, 1_000_000_000, time.Second, 500 * time.Millisecond, 750 * time.Millisecond},
		{"1tohalf", 0, -500_000_000, time.Second, 500 * time.Millisecond, 1500 * time.Millisecond},
		{"2to1", 1_000_000_000, 0, time.Second, 250 * time.Millisecond, 750 * time.Millisecond},                // armed rate2: fires 0.5s; at 0.25s -> rate1: remaining 0.25s base=0.5s host -> 0.5s base; total 0.75s
		{"2tohalf", 1_000_000_000, -500_000_000, time.Second, 250 * time.Millisecond, 1250 * time.Millisecond}, // armed rate2 fires 0.5s; at 0.25s -> rate0.5: remaining 0.5s host -> 1s base; total 1.25s
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := driftClockReSleep(t, tc.ppbOld, tc.ppbNew, tc.d, tc.T); got != tc.want {
				t.Errorf("re-map %s: sleep finished at base %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestDSTClockDriftClockProperty: for arbitrary rates and a change instant before firing,
// the re-mapped firing matches the big.Int oracle of when' = T + remap(whenOld-T).
func TestDSTClockDriftClockProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(0xD2C10C))
	for i := 0; i < 48; i++ {
		// rates in [-9e8, 1e9] (rate [0.1, 2]); d in [1ms, 1s]; T a fraction of whenOld.
		ppbOld := rng.Int63n(1_900_000_001) - 900_000_000
		ppbNew := rng.Int63n(1_900_000_001) - 900_000_000
		d := time.Duration(rng.Int63n(int64(time.Second)) + int64(time.Millisecond))
		whenOld := bigToBase(int64(d), ppbOld) // base instant the timer would fire, un-changed
		if whenOld < 4 {
			continue // too small to place a change strictly inside
		}
		T := time.Duration(whenOld / (2 + rng.Int63n(4))) // strictly between 0 and whenOld
		want := int64(T) + bigRemap(whenOld-int64(T), ppbOld, ppbNew)
		got := driftClockReSleep(t, ppbOld, ppbNew, d, T)
		if int64(got) != want {
			t.Fatalf("ppbOld %d ppbNew %d d %v T %v: finished at %d, want %d", ppbOld, ppbNew, d, T, int64(got), want)
		}
	}
}

// TestDSTClockDriftClockWallContinuity: the wall reading does not jump across a rate
// change — after some accumulated drift, a DriftClock at a frozen base leaves time.Now
// reading the same value (the re-anchor folds drift-so-far into the offset).
func TestDSTClockDriftClockWallContinuity(t *testing.T) {
	var before, after time.Time
	Run(1, func() {
		phase1 := make(chan struct{})
		ready := make(chan struct{})
		go1 := make(chan struct{})
		done := make(chan struct{})
		Host("h", HostConfig{Clock: Drift(1_000_000_000)}, func() { // rate 2 from declaration
			go func() {
				<-phase1
				before = time.Now() // wall after base advanced (drift accumulated)
				close(ready)
				<-go1
				after = time.Now() // post-DriftClock, base still frozen
				close(done)
			}()
		})
		time.Sleep(time.Second) // root advances base by 1s; host wall has drifted to base+1s
		close(phase1)
		<-ready
		DriftClock("h", -500_000_000) // switch to rate 0.5, frozen base
		close(go1)
		<-done
	})
	if !after.Equal(before) {
		t.Errorf("wall jumped across a rate change: before=%v after=%v (re-anchor must keep the wall continuous)", before, after)
	}
}

// TestDSTClockDriftClockResync: DriftClock(h, 0) restores rate 1 — a sleep armed after the
// re-sync takes its full base duration again.
func TestDSTClockDriftClockResync(t *testing.T) {
	var afterResync time.Duration
	Run(1, func() {
		ready := make(chan struct{})
		go1 := make(chan struct{})
		done := make(chan struct{})
		var start time.Time
		Host("h", HostConfig{Clock: Drift(1_000_000_000)}, func() { // rate 2
			go func() {
				close(ready)
				<-go1
				start = time.Now()
				time.Sleep(time.Second) // armed AFTER the re-sync: rate 1 -> 1s base
				close(done)
			}()
		})
		<-ready
		DriftClock("h", 0) // re-sync to rate 1
		close(go1)
		<-done
		afterResync = time.Since(start)
	})
	if afterResync != time.Second {
		t.Errorf("Sleep(1s) after DriftClock(h,0) advanced base %v, want 1s (re-sync must restore rate 1)", afterResync)
	}
}

// TestDSTClockDriftClockMultiplePending: a single rate change re-maps every pending timer
// of the host. Two sleeps (1s and 2s, rate 1) re-mapped to rate 2 at base 0.5s both shift.
func TestDSTClockDriftClockMultiplePending(t *testing.T) {
	var d1, d2 time.Duration
	Run(1, func() {
		armed := make(chan struct{}, 2)
		done1 := make(chan struct{})
		done2 := make(chan struct{})
		var start time.Time
		Host("h", HostConfig{}, func() { // rate 1
			go func() { armed <- struct{}{}; time.Sleep(1 * time.Second); close(done1) }()
			go func() { armed <- struct{}{}; time.Sleep(2 * time.Second); close(done2) }()
		})
		<-armed
		<-armed
		start = time.Now()
		time.Sleep(500 * time.Millisecond) // base 0.5s
		DriftClock("h", 1_000_000_000)     // rate 2: re-map both pending timers
		<-done1
		d1 = time.Since(start)
		<-done2
		d2 = time.Since(start)
	})
	// timer1: fires at 1s un-changed; remaining 0.5s -> 0.25s base -> 0.75s.
	// timer2: fires at 2s; remaining 1.5s -> 0.75s base -> 1.25s.
	if d1 != 750*time.Millisecond {
		t.Errorf("pending 1s timer re-mapped to base %v, want 750ms", d1)
	}
	if d2 != 1250*time.Millisecond {
		t.Errorf("pending 2s timer re-mapped to base %v, want 1250ms", d2)
	}
}

// TestDSTClockDriftClockFromOwnGoroutine: DriftClock called from a goroutine OF the host
// being changed (not the root) must still re-map that host's pending timers correctly. The
// re-map sets the re-mapped base when directly (not via the timer-arm path), so it is
// immune to the caller goroutine's own rate — a 1s sleep (rate 1) at base 0 re-maps to
// 0.75s when an h goroutine switches h to rate 2 at base 0.5, regardless of who calls.
func TestDSTClockDriftClockFromOwnGoroutine(t *testing.T) {
	var total time.Duration
	Run(1, func() {
		armed := make(chan struct{})
		doChange := make(chan struct{})
		done := make(chan struct{})
		var start time.Time
		Host("h", HostConfig{}, func() { // rate 1
			go func() { close(armed); time.Sleep(time.Second); close(done) }() // pending, owned by h
			go func() { <-doChange; DriftClock("h", 1_000_000_000) }()         // h's own goroutine changes h's rate
		})
		<-armed
		start = time.Now()
		time.Sleep(500 * time.Millisecond) // base 0.5
		close(doChange)                    // DriftClock runs on an h goroutine (gp.dstHost == h)
		<-done
		total = time.Since(start)
	})
	if total != 750*time.Millisecond {
		t.Errorf("DriftClock from the host's own goroutine re-mapped its pending timer to base %v, want 750ms (the re-arm must not be re-converted)", total)
	}
}

// TestDSTClockDriftClockTwice: two rate changes on a host with one pending timer re-map it
// each time — the timer's owner tag and registry entry survive the first re-walk (the
// in-place re-map does not touch them), so the second DriftClock still finds it.
func TestDSTClockDriftClockTwice(t *testing.T) {
	var total time.Duration
	Run(1, func() {
		armed := make(chan struct{})
		done := make(chan struct{})
		var start time.Time
		Host("h", HostConfig{}, func() { // rate 1
			go func() { close(armed); time.Sleep(time.Second); close(done) }()
		})
		<-armed
		start = time.Now()
		time.Sleep(250 * time.Millisecond) // base 0.25
		DriftClock("h", 1_000_000_000)     // rate 2: re-map 1s -> 0.625s
		time.Sleep(250 * time.Millisecond) // base 0.5
		DriftClock("h", 0)                 // rate 1: re-map 0.625s -> 0.75s (needs the owner tag intact)
		<-done
		total = time.Since(start)
	})
	if total != 750*time.Millisecond {
		t.Errorf("two rate changes re-mapped the pending timer to base %v, want 750ms (owner tag must survive the first re-walk)", total)
	}
}

// TestDSTClockDriftClockUnheapedTimer is the H-1 regression: a channel timer is in the
// bubble's heap only while a goroutine is blocked on its channel, so a NewTimer armed but
// not yet awaited is unheaped. A heap-scan re-walk would miss it; the per-run fake-timer
// list includes it, so DriftClock re-maps it. The timer is armed (1s, rate 1) and held
// unheaped across a DriftClock to rate 2 at base 0.5s, then awaited — it must fire at the
// re-mapped 0.75s, not the un-changed 1s.
func TestDSTClockDriftClockUnheapedTimer(t *testing.T) {
	var total time.Duration
	Run(1, func() {
		armed := make(chan struct{})
		doAwait := make(chan struct{})
		fired := make(chan struct{})
		var start time.Time
		Host("h", HostConfig{}, func() { // rate 1
			go func() {
				tmr := time.NewTimer(time.Second) // armed at base 0, UNHEAPED (not yet awaited)
				close(armed)
				<-doAwait // held here: the timer stays unheaped across the DriftClock
				<-tmr.C   // only now block on it -> it heaps with whatever when it carries
				close(fired)
			}()
		})
		<-armed
		start = time.Now()
		time.Sleep(500 * time.Millisecond) // base 0.5
		DriftClock("h", 1_000_000_000)     // rate 2: must re-map the UNHEAPED timer 1s -> 0.75s
		close(doAwait)
		<-fired
		total = time.Since(start)
	})
	if total != 750*time.Millisecond {
		t.Errorf("unheaped NewTimer re-mapped to base %v, want 750ms (the re-walk must cover unheaped channel timers, not just the heap)", total)
	}
}

// TestDSTClockDriftClockUnheapedTicker: a ticker armed but not yet awaited is unheaped;
// DriftClock to rate 2 must re-map both its first-tick when and its period, so the ticks
// land at 0.5s, 1.0s, 1.5s of base (each 1s host-tick = 0.5s base) rather than 1s, 2s, 3s.
func TestDSTClockDriftClockUnheapedTicker(t *testing.T) {
	const n = 3
	var bases []time.Duration
	Run(1, func() {
		armed := make(chan struct{})
		doAwait := make(chan struct{})
		ticks := make(chan struct{}, n)
		Host("h", HostConfig{}, func() { // rate 1
			go func() {
				tk := time.NewTicker(time.Second) // armed, UNHEAPED until awaited
				defer tk.Stop()
				close(armed)
				<-doAwait
				for i := 0; i < n; i++ {
					<-tk.C
					ticks <- struct{}{}
				}
			}()
		})
		<-armed
		start := time.Now()
		DriftClock("h", 1_000_000_000) // rate 2 before any tick is awaited (ticker unheaped)
		close(doAwait)
		for i := 0; i < n; i++ {
			<-ticks
			bases = append(bases, time.Since(start))
		}
	})
	for i, b := range bases {
		if want := time.Duration(i+1) * 500 * time.Millisecond; b != want {
			t.Errorf("tick %d at base %v, want %v (rate-2 ticker; unheaped first tick + period must re-map)", i, b, want)
		}
	}
}

// TestDSTClockDriftClockAbandonedTimer exercises a timer that was heaped (a goroutine
// blocked on its channel in a select) and then unheaped when that select was won by
// another case, leaving the timer armed-but-reader-less — the heaped→unheaped transition.
// The re-map must still move it (it remains in the per-run list), so when the goroutine
// re-blocks on it after a rate change to 2 at base 0.5s, it fires at the re-mapped 0.75s.
//
// Note on the M-1 zombie branch (the re-map preserves timerZombie rather than clearing it
// like modify): that branch fires only for a HEAPED timer with timerZombie set and when>0,
// which is a transient state during a channel send with no reader — not deterministically
// constructible (a select-abandon unheaps the timer; a Stop zeroes its when). Preserve vs
// clear is also observably equivalent for a cap-1 timer channel (any observation re-blocks
// and resurrects). So that branch is verified by inspection (it mirrors modify minus the
// zombie-clear) and the round-2 review, not by a distinguishing test.
func TestDSTClockDriftClockAbandonedTimer(t *testing.T) {
	var total time.Duration
	Run(1, func() {
		armed := make(chan struct{})
		other := make(chan struct{})
		doDrift := make(chan struct{})
		fired := make(chan struct{})
		var start time.Time
		Host("h", HostConfig{}, func() { // rate 1
			go func() {
				tmr := time.NewTimer(time.Second) // armed at base 0
				close(armed)
				select { // 'other' wins -> tmr's reader leaves while pending -> heaped zombie
				case <-tmr.C:
				case <-other:
				}
				<-doDrift
				<-tmr.C // re-block -> un-zombie -> fire at the re-mapped when
				close(fired)
			}()
		})
		<-armed
		start = time.Now()
		close(other)                       // goroutine leaves the select; tmr is now a heaped zombie at 1s
		time.Sleep(500 * time.Millisecond) // base 0.5
		DriftClock("h", 1_000_000_000)     // rate 2: re-map the zombie 1s -> 0.75s, preserving zombie
		close(doDrift)
		<-fired
		total = time.Since(start)
	})
	if total != 750*time.Millisecond {
		t.Errorf("re-mapped zombie timer fired at base %v, want 750ms", total)
	}
}

// TestDSTClockDriftClockVictim: DriftClock(A) re-maps only A's pending timers; B's pending
// timer is untouched.
func TestDSTClockDriftClockVictim(t *testing.T) {
	var aBase, bBase time.Duration
	Run(1, func() {
		armed := make(chan struct{}, 2)
		aDone := make(chan struct{})
		bDone := make(chan struct{})
		var start time.Time
		Host("A", HostConfig{}, func() { // rate 1, will be changed
			go func() { armed <- struct{}{}; time.Sleep(time.Second); close(aDone) }()
		})
		Host("B", HostConfig{}, func() { // rate 1, must be untouched
			go func() { armed <- struct{}{}; time.Sleep(time.Second); close(bDone) }()
		})
		<-armed
		<-armed
		start = time.Now()
		time.Sleep(500 * time.Millisecond)
		DriftClock("A", 1_000_000_000) // only A -> rate 2
		<-aDone
		aBase = time.Since(start)
		<-bDone
		bBase = time.Since(start)
	})
	if aBase != 750*time.Millisecond {
		t.Errorf("host A's pending timer re-mapped to base %v, want 750ms", aBase)
	}
	if bBase != time.Second {
		t.Errorf("host B's pending timer fired at base %v, want 1s (DriftClock(A) must not touch B)", bBase)
	}
}

// TestDSTClockDriftClockWithStep: DriftClock and StepClock compose — a rate change then a
// wall step on the same host both take effect (rate converts the next sleep; step jumps
// the wall).
func TestDSTClockDriftClockWithStep(t *testing.T) {
	const step = 100 * time.Millisecond
	var sleepBase time.Duration
	var before, after time.Time
	Run(1, func() {
		ready := make(chan struct{})
		go1 := make(chan struct{})
		slept := make(chan struct{})
		Host("h", HostConfig{}, func() { // rate 1
			go func() {
				before = time.Now()
				close(ready)
				<-go1
				after = time.Now()
				time.Sleep(time.Second) // rate 2 (after DriftClock) -> 0.5s base
				close(slept)
			}()
		})
		<-ready
		DriftClock("h", 1_000_000_000) // rate 2
		StepClock("h", step)           // wall +100ms, frozen base
		startBase := time.Now()
		close(go1)
		<-slept
		sleepBase = time.Since(startBase)
	})
	if jump := after.Sub(before); jump != step {
		t.Errorf("step on a DriftClock'd host jumped wall %v, want %v", jump, step)
	}
	if sleepBase != 500*time.Millisecond {
		t.Errorf("Sleep(1s) after DriftClock(rate2)+step advanced base %v, want 500ms", sleepBase)
	}
}

// TestDSTClockDriftClockDeterminism: same seed + same DriftClock schedule → identical.
func TestDSTClockDriftClockDeterminism(t *testing.T) {
	run := func() time.Duration {
		return driftClockReSleep(t, 250_000_000, -100_000_000, 900*time.Millisecond, 200*time.Millisecond)
	}
	if a, b := run(), run(); a != b {
		t.Errorf("DriftClock re-map not reproducible: %v vs %v", a, b)
	}
}

// TestDSTClockDriftClockNoChange: DriftClock to the host's current rate is a no-op (no
// re-walk, no wall change).
func TestDSTClockDriftClockNoChange(t *testing.T) {
	got := driftClockReSleep(t, 1_000_000_000, 1_000_000_000, time.Second, 100*time.Millisecond)
	// rate 2 unchanged: 1s host sleep = 0.5s base, regardless of the no-op DriftClock.
	if got != 500*time.Millisecond {
		t.Errorf("no-op DriftClock changed the firing: base %v, want 500ms", got)
	}
}

// TestDSTClockDriftClockRateValidation: DriftClock rejects a non-positive/reversed rate.
func TestDSTClockDriftClockRateValidation(t *testing.T) {
	for _, ppb := range []int64{-driftPPBBase, -driftPPBBase - 1, maxDriftPPB + 1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("DriftClock(_, %d) did not panic (rate out of (0,2])", ppb)
				}
			}()
			DriftClock("h", ppb) // validation panics before any runtime call, even outside a run
		}()
	}
}

// TestDSTClockDriftRemapExact: the re-map helper is the exact integer CEILING of
// x*(1e9+ppbOld)/(1e9+ppbNew) (the rounding contract — a re-map is never early in
// host-perceived time), matched against a big.Int oracle (incl. the overflow region).
func TestDSTClockDriftRemapExact(t *testing.T) {
	rng := rand.New(rand.NewSource(0x2EFA17))
	check := func(x, ppbOld, ppbNew int64) {
		if got, want := dstDriftRemap(x, ppbOld, ppbNew), bigRemap(x, ppbOld, ppbNew); got != want {
			t.Fatalf("dstDriftRemap(%d, %d, %d) = %d, want %d", x, ppbOld, ppbNew, got, want)
		}
	}
	for _, x := range []int64{0, 1, int64(time.Second), int64(time.Hour)} {
		for _, po := range []int64{0, 1_000_000_000, -500_000_000} {
			for _, pn := range []int64{0, 1_000_000_000, -500_000_000, -900_000_000} {
				if x > 0 || po != pn {
					check(x, po, pn)
				}
			}
		}
	}
	for i := 0; i < 200; i++ {
		check(rng.Int63n(int64(time.Hour))+1, rng.Int63n(1_900_000_001)-900_000_000, rng.Int63n(1_900_000_001)-900_000_000)
	}
}

// TestDSTClockDriftResetByRedeclare: re-declaring a drifting host with a zero clock
// config re-establishes rate 1 and an in-sync wall (docs/dst/faults.md "Clock faults",
// Host re-declaration) — neither the drift RATE nor its accumulated wall departure
// survives a restart. Measured against BASE (the root): the defect this pins — an
// offset overwrite that leaves a stale rate and anchor — is self-consistent to the
// host's own Since-over-Sleep probes, so only a base-relative check catches it.
func TestDSTClockDriftResetByRedeclare(t *testing.T) {
	var atRestart, base time.Time
	var sleepBase time.Duration
	Run(1, func() {
		Host("h", HostConfig{Clock: Drift(1_000_000_000)}, func() {}) // rate 2
		time.Sleep(time.Second)                                       // root advances base 1s; host h accumulates +1s of wall departure
		base = time.Now()
		done := make(chan struct{})
		Host("h", HostConfig{}, func() { // restart: zero config = in sync, rate 1
			atRestart = time.Now()
			go func() { time.Sleep(time.Second); close(done) }()
		})
		<-done
		sleepBase = time.Since(base)
	})
	if !atRestart.Equal(base) {
		t.Errorf("restarted zero-config host reads %v, want base %v (re-declare must clear accumulated drift and rate)", atRestart, base)
	}
	if sleepBase != time.Second {
		t.Errorf("restarted host's Sleep(1s) advanced base by %v, want 1s (rate must reset to 1)", sleepBase)
	}
}

// TestDSTClockDriftRedeclareSameRate: re-declaring the SAME nonzero rate still
// re-anchors — drift accumulated before the restart is discarded. This is the leg a
// fold-then-overwrite composition misses: the offset overwrite discards the fold, but
// a surviving stale anchor keeps pre-restart accumulation in every later wall read.
func TestDSTClockDriftRedeclareSameRate(t *testing.T) {
	var atRestart, base time.Time
	Run(1, func() {
		Host("h", HostConfig{Clock: Drift(1_000_000_000)}, func() {}) // rate 2
		time.Sleep(time.Second)                                       // host accumulates +1s of wall departure
		base = time.Now()
		Host("h", HostConfig{Clock: Drift(1_000_000_000)}, func() { atRestart = time.Now() })
	})
	if !atRestart.Equal(base) {
		t.Errorf("same-rate re-declare kept accumulated drift: host reads %v, want base %v (anchor must reset)", atRestart, base)
	}
}

// TestDSTClockRedeclareRemapsPendingTimer: a timer armed under the old rate is
// re-mapped by a re-declaration that changes the rate, exactly as DriftClock re-maps
// it — a surviving goroutine's pending Sleep must fire at the new rate's converted
// instant, not stay converted at the dead incarnation's rate.
func TestDSTClockRedeclareRemapsPendingTimer(t *testing.T) {
	const d = 2 * time.Second        // armed at rate 1: fires at base 2s
	const T = 500 * time.Millisecond // rate change instant
	var total time.Duration
	Run(1, func() {
		armed := make(chan struct{})
		done := make(chan struct{})
		start := time.Now()
		Host("h", HostConfig{}, func() { // rate 1
			go func() {
				close(armed)
				time.Sleep(d)
				close(done)
			}()
		})
		<-armed
		time.Sleep(T)                                                 // root: advance base to T with the sleep pending
		Host("h", HostConfig{Clock: Drift(1_000_000_000)}, func() {}) // restart at rate 2
		<-done
		total = time.Since(start)
	})
	// Remaining 1.5s of host time re-maps to 0.75s of base at rate 2: total = T + 0.75s.
	if want := T + 750*time.Millisecond; total != want {
		t.Errorf("pending Sleep(%v) with a rate-2 re-declare at %v fired after %v of base, want %v (re-map)", d, T, total, want)
	}
}

// TestDSTClockTableFreshPerRun: host ids restart at 1 each run, so the per-host clock
// table must be per-run state (reset at run entry, dstSetSimEnv) — otherwise a host id
// reused by a later run inherits the earlier run's rate/offset and a process's timing
// depends on which runs came before it in the binary.
func TestDSTClockTableFreshPerRun(t *testing.T) {
	Run(1, func() { // run 1: host id 1 gets rate 2
		Host("a", HostConfig{Clock: Drift(1_000_000_000)}, func() {})
	})
	var adv time.Duration
	Run(1, func() { // run 2: Process's implicit host also interns to id 1
		start := time.Now()
		done := make(chan struct{})
		Process("p", func() {
			go func() { time.Sleep(time.Second); close(done) }()
			<-done // body return is process exit: the sleeper must finish inside
		})
		adv = time.Since(start)
	})
	if adv != time.Second {
		t.Errorf("second run's Sleep(1s) advanced base by %v, want 1s (a previous run's clock leaked through the reused host id — the table must reset per run)", adv)
	}
}

// TestDSTClockImplicitHostSurvivesProcessRestart: a process restart does not reboot
// its host — a StepClock applied to a process's implicit host persists across a
// Process re-invocation (the host stayed up; only a Host re-declaration models the
// machine reboot and re-establishes the clock).
func TestDSTClockImplicitHostSurvivesProcessRestart(t *testing.T) {
	const step = 250 * time.Millisecond
	var second, base time.Time
	Run(1, func() {
		Process("p", func() {})
		StepClock("p", step) // step the implicit host's clock
		base = time.Now()
		Process("p", func() { second = time.Now() }) // restart on the surviving host
	})
	if got := second.Sub(base); got != step {
		t.Errorf("after a process restart the implicit host reads base+%v, want base+%v (a restart must not reset the host clock)", got, step)
	}
}
