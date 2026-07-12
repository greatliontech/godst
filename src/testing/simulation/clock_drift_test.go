// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"context"
	"math"
	"math/big"
	"math/rand"
	"testing"
	"time"
	_ "unsafe" // for go:linkname
)

//go:linkname dstDriftToBase runtime.dstDriftToBase
func dstDriftToBase(d, ppb int64) int64

const maxWhen = 1<<63 - 1 // runtime/time.go maxWhen

// These tests exercise the constant-rate clock DRIFT fault (Drift — a host whose clock
// runs fast/slow at rate 1 + ppb/1e9, docs/dst/faults.md "Clock faults"). The defining
// behaviors:
//   - a drifting host's time.Now and time.Since advance at the rate (wall drift);
//   - its relative timers (Sleep/After/NewTimer/NewTicker/AfterFunc/context) fire after
//     the rate-converted base interval: a rate-r host's d-timer fires after d/r of base;
//   - a host's own clock is self-consistent (it cannot detect its own drift);
//   - drift composes with skew and step, isolates per host (DST-FAULT-VICTIM), replays
//     deterministically, and rate 1 is byte-identical (the N=1 collapse).
// Base-time advances are measured on the root (host 0, rate 1, in sync with base).
//
// Exact rates (2x = ppb 1e9, 1/2 = -5e8, 1/10 = -9e8) give exact assertions; the
// property test (TestDSTClockDriftProperty) covers arbitrary rates against a big.Int
// oracle of the same integer-ceiling conversion the runtime uses (the rounding contract).

// driftBaseAdvance runs fn on a goroutine of a host with the given clock config and
// returns the base-time advance fn caused, measured on the root. Nothing blocks on a
// timer between start and fn's arm, so the advance is exactly fn's converted duration.
func driftBaseAdvance(t *testing.T, clock ClockConfig, fn func()) time.Duration {
	t.Helper()
	var adv time.Duration
	Run(1, func() {
		done := make(chan struct{})
		start := time.Now() // root = base
		Host("h", HostConfig{Clock: clock}, func() {
			go func() {
				fn()
				close(done)
			}()
		})
		<-done
		adv = time.Since(start)
	})
	return adv
}

// TestDSTClockDriftTimerConversion is the core check: every relative-timer entry point
// on a rate-r host fires after d/r of BASE time. All entry points funnel through the
// single modify choke, so each must convert identically.
func TestDSTClockDriftTimerConversion(t *testing.T) {
	const d = time.Second
	rates := []struct {
		name string
		ppb  int64
		want time.Duration // base advance for a d-duration host timer
	}{
		{"rate1", 0, d}, // 1s host = 1s base
		{"2x", 1_000_000_000, 500 * time.Millisecond}, // 1s host = 0.5s base
		{"half", -500_000_000, 2 * time.Second},       // 1s host = 2s base
		{"tenth", -900_000_000, 10 * time.Second},     // 1s host = 10s base
	}
	entry := []struct {
		name string
		fn   func(time.Duration) func()
	}{
		{"Sleep", func(d time.Duration) func() { return func() { time.Sleep(d) } }},
		{"After", func(d time.Duration) func() { return func() { <-time.After(d) } }},
		{"NewTimer", func(d time.Duration) func() { return func() { <-time.NewTimer(d).C } }},
		{"AfterFunc", func(d time.Duration) func() {
			return func() {
				fired := make(chan struct{})
				time.AfterFunc(d, func() { close(fired) })
				<-fired
			}
		}},
		{"context", func(d time.Duration) func() {
			return func() {
				ctx, cancel := context.WithTimeout(context.Background(), d)
				defer cancel()
				<-ctx.Done()
			}
		}},
	}
	for _, r := range rates {
		for _, e := range entry {
			t.Run(r.name+"/"+e.name, func(t *testing.T) {
				if got := driftBaseAdvance(t, Drift(r.ppb), e.fn(d)); got != r.want {
					t.Errorf("rate %s, %s(%v): base advanced %v, want %v (d/r)", r.name, e.name, d, got, r.want)
				}
			})
		}
	}
}

// TestDSTClockDriftTicker: a ticker on a rate-r host ticks every d/r of base, for every
// tick (the periodic re-arm reuses the converted period, so ticks stay rate-correct
// without re-entering the arm path).
func TestDSTClockDriftTicker(t *testing.T) {
	const d = time.Second
	const ticks = 4
	var tickBase time.Duration
	Run(1, func() {
		done := make(chan struct{})
		start := time.Now()
		Host("h", HostConfig{Clock: Drift(1_000_000_000)}, func() { // rate 2: each tick 0.5s base
			go func() {
				tk := time.NewTicker(d)
				defer tk.Stop()
				for i := 0; i < ticks; i++ {
					<-tk.C
				}
				close(done)
			}()
		})
		<-done
		tickBase = time.Since(start)
	})
	// 4 ticks of 1s host-time at rate 2 = 4 * 0.5s = 2s of base — the periodic re-arm
	// must keep converting the period, not just the first tick.
	if want := time.Duration(ticks) * 500 * time.Millisecond; tickBase != want {
		t.Errorf("%d ticks of %v on a rate-2 host took %v of base, want %v", ticks, d, tickBase, want)
	}
}

// TestDSTClockDriftWall: a drifting host's time.Now wall advances at the rate over a
// base interval (created by the root sleeping). rate-r over base B advances r*B.
func TestDSTClockDriftWall(t *testing.T) {
	const baseInterval = time.Second
	for _, r := range []struct {
		name string
		ppb  int64
		want time.Duration
	}{
		{"2x", 1_000_000_000, 2 * time.Second}, // wall advances 2x base
		{"half", -500_000_000, 500 * time.Millisecond},
		{"rate1", 0, time.Second},
	} {
		t.Run(r.name, func(t *testing.T) {
			var w0, w1 time.Time
			Run(1, func() {
				ready := make(chan struct{})
				next := make(chan struct{})
				done := make(chan struct{})
				Host("h", HostConfig{Clock: Drift(r.ppb)}, func() {
					go func() {
						w0 = time.Now()
						close(ready)
						<-next
						w1 = time.Now()
						close(done)
					}()
				})
				<-ready
				time.Sleep(baseInterval) // root (rate 1) advances base by exactly baseInterval
				close(next)
				<-done
			})
			if got := w1.Sub(w0); got != r.want {
				t.Errorf("rate %s: host wall advanced %v over %v of base, want %v", r.name, got, baseInterval, r.want)
			}
		})
	}
}

// driftSince returns time.Since measured across a Sleep(d) on a host drifting at ppb —
// the duration the host perceives its own d-sleep to take.
func driftSince(t *testing.T, ppb int64, d time.Duration) time.Duration {
	t.Helper()
	var since time.Duration
	Run(1, func() {
		done := make(chan struct{})
		Host("h", HostConfig{Clock: Drift(ppb)}, func() {
			go func() {
				start := time.Now()
				time.Sleep(d)
				since = time.Since(start)
				close(done)
			}()
		})
		<-done
	})
	return since
}

// TestDSTClockDriftSelfConsistent: a host cannot detect its own drift — its time.Since
// over its own Sleep(d) reads d back (it slept d/r of base, and its wall advanced r
// times that = d). For exact-dividing rates this is exact; for an arbitrary rate the
// rounding contract (docs/dst/faults.md "Clock faults") bounds the round trip to
// [d, d + ~rate+1 ns]: the arm conversion rounds UP and the wall accumulation rounds
// down, composing to floor(ceil(d/r)·r) ≥ d — NEVER below d, because Sleep returning
// with Since < d is real Go's documented "at least d" broken, the Soundness invariant's
// "timer before its deadline" false positive. The strongest internal-consistency
// invariant.
func TestDSTClockDriftSelfConsistent(t *testing.T) {
	const d = 700 * time.Millisecond
	for _, ppb := range []int64{0, 1_000_000_000, -500_000_000, -900_000_000} {
		if since := driftSince(t, ppb, d); since != d {
			t.Errorf("ppb %d (exact rate): time.Since over the host's own Sleep(%v) = %v, want exactly %v", ppb, d, since, d)
		}
	}
	const over = 4 * time.Nanosecond // >= rate+1 for rate <= 2
	for _, ppb := range []int64{250_000, -123_457, 7_777_777, -333_333_333} {
		since := driftSince(t, ppb, d)
		if since < d || since > d+over {
			t.Errorf("ppb %d: time.Since over Sleep(%v) = %v, want in [%v, %v] (never early; ceil-arm/floor-wall rounding)", ppb, d, since, d, d+over)
		}
	}
}

// TestDSTClockDriftSleepNeverEarly is the property sweep of the never-early half of the
// rounding contract at non-dividing rates: for arbitrary (rate, duration), a host's own
// time.Since over its own Sleep(d) is >= d, always. With a floor at the arm this fails
// for most non-dividing pairs (elapsed d-1 or d-2 ns) — e.g. rate 1.5, d=100ms reads
// 99999999ns — the "timer fires before its deadline in the host's own clock" false
// positive verbatim.
func TestDSTClockDriftSleepNeverEarly(t *testing.T) {
	rng := rand.New(rand.NewSource(0x51EE9EA871))
	for i := 0; i < 64; i++ {
		ppb := rng.Int63n(1_900_000_001) - 900_000_000 // rate in [0.1, 2]
		d := time.Duration(rng.Int63n(1_000_000_000) + 1)
		if since := driftSince(t, ppb, d); since < d {
			t.Fatalf("ppb %d: time.Since over Sleep(%v) = %v < d — the host observed its timer fire early", ppb, d, since)
		}
	}
	// The named anchor case from the audit: rate 1.5 (ppb 5e8), d = 100ms.
	if d, since := 100*time.Millisecond, driftSince(t, 500_000_000, 100*time.Millisecond); since < d {
		t.Fatalf("rate 1.5: time.Since over Sleep(100ms) = %v < 100ms", since)
	}
}

// TestDSTClockDriftCrossHost: hosts at different rates keep independent clocks — a
// rate-2 host's 1s sleep (0.5s base) fires before a rate-1 host's 1s sleep (1s base),
// and each at its own base instant.
func TestDSTClockDriftCrossHost(t *testing.T) {
	var firstName, secondName string
	var firstBase, secondBase time.Duration
	Run(1, func() {
		order := make(chan string, 2)
		Host("A", HostConfig{Clock: Drift(1_000_000_000)}, func() { // rate 2 -> 0.5s base
			go func() { time.Sleep(time.Second); order <- "A" }()
		})
		Host("B", HostConfig{}, func() { // rate 1 -> 1s base
			go func() { time.Sleep(time.Second); order <- "B" }()
		})
		start := time.Now()
		firstName = <-order
		firstBase = time.Since(start)
		secondName = <-order
		secondBase = time.Since(start)
	})
	if firstName != "A" || secondName != "B" {
		t.Errorf("wake order = %q then %q, want A then B (the faster clock's timer fires first in base)", firstName, secondName)
	}
	if firstBase != 500*time.Millisecond {
		t.Errorf("rate-2 host woke at base %v, want 500ms", firstBase)
	}
	if secondBase != time.Second {
		t.Errorf("rate-1 host woke at base %v, want 1s", secondBase)
	}
}

// TestDSTClockDriftWithSkew: drift composes with a static skew — the wall reads
// base+skew+drift, and the rate still converts timers (the skew does not affect the
// rate). Skew(s).WithDrift(ppb).
func TestDSTClockDriftWithSkew(t *testing.T) {
	const skew = 50 * time.Millisecond
	var wallAhead time.Duration
	var baseAdvance time.Duration
	Run(1, func() {
		ready := make(chan struct{})
		go1 := make(chan struct{})
		done := make(chan struct{})
		var hostWall, rootWall time.Time
		Host("h", HostConfig{Clock: Skew(skew).WithDrift(1_000_000_000)}, func() { // +50ms, rate 2
			go func() {
				hostWall = time.Now() // at declaration base: base + skew (drift term ~0)
				close(ready)
				<-go1
				time.Sleep(time.Second) // 1s host = 0.5s base
				close(done)
			}()
		})
		<-ready
		rootWall = time.Now()
		wallAhead = hostWall.Sub(rootWall) // = skew (drift not yet accumulated)
		start := time.Now()
		close(go1)
		<-done
		baseAdvance = time.Since(start)
	})
	if wallAhead != skew {
		t.Errorf("drift+skew host wall is %v ahead of root at t0, want %v (the skew)", wallAhead, skew)
	}
	if baseAdvance != 500*time.Millisecond {
		t.Errorf("drift+skew host Sleep(1s) advanced base %v, want 500ms (rate 2 converts; skew does not)", baseAdvance)
	}
}

// TestDSTClockDriftWithStep: a StepClock on a drifting host adds to its offset (the wall
// jumps) while the rate is unchanged (timers still convert).
func TestDSTClockDriftWithStep(t *testing.T) {
	const step = 100 * time.Millisecond
	var before, after time.Time
	var sleepBase time.Duration
	Run(1, func() {
		ready := make(chan struct{})
		go1 := make(chan struct{})
		slept := make(chan struct{})
		Host("h", HostConfig{Clock: Drift(1_000_000_000)}, func() { // rate 2
			go func() {
				before = time.Now()
				close(ready)
				<-go1
				after = time.Now()      // post-step, base still frozen
				time.Sleep(time.Second) // rate 2 must still convert -> 0.5s base
				close(slept)
			}()
		})
		<-ready
		StepClock("h", step) // base frozen (channel handoff): only the offset jumps
		startBase := time.Now()
		close(go1)
		<-slept
		sleepBase = time.Since(startBase)
	})
	if jump := after.Sub(before); jump != step {
		t.Errorf("StepClock on a drifting host jumped wall by %v, want %v (step adds to the offset)", jump, step)
	}
	if sleepBase != 500*time.Millisecond {
		t.Errorf("Sleep(1s) after a step on a rate-2 host advanced base %v, want 500ms (the rate survives a step)", sleepBase)
	}
}

// TestDSTClockDriftDeterminism: same seed + same Drift config → identical wall readings
// and identical timer base-firing.
func TestDSTClockDriftDeterminism(t *testing.T) {
	run := func() (time.Time, time.Duration) {
		var w time.Time
		var adv time.Duration
		Run(5, func() {
			done := make(chan struct{})
			start := time.Now()
			Host("h", HostConfig{Clock: Skew(20 * time.Millisecond).WithDrift(300_000_000)}, func() {
				go func() {
					w = time.Now()
					time.Sleep(900 * time.Millisecond)
					close(done)
				}()
			})
			<-done
			adv = time.Since(start)
		})
		return w, adv
	}
	w1, a1 := run()
	w2, a2 := run()
	if !w1.Equal(w2) || a1 != a2 {
		t.Errorf("drift not reproducible across same-seed runs: wall %v/%v, base advance %v/%v", w1, w2, a1, a2)
	}
}

// TestDSTClockDriftMonotonic enforces DST-CLOCK-DRIFT-MONOTONIC: a drifting clock (rate
// > 0) advances monotonically — its time.Now never goes backward across base advances,
// fast or slow.
func TestDSTClockDriftMonotonic(t *testing.T) {
	for _, ppb := range []int64{1_000_000_000, -900_000_000} {
		var reads []time.Time
		Run(1, func() {
			step := make(chan struct{})
			got := make(chan time.Time, 4)
			Host("h", HostConfig{Clock: Drift(ppb)}, func() {
				go func() {
					for i := 0; i < 4; i++ {
						got <- time.Now()
						<-step
					}
				}()
			})
			for i := 0; i < 4; i++ {
				reads = append(reads, <-got)
				time.Sleep(100 * time.Millisecond) // advance base
				step <- struct{}{}
			}
		})
		for i := 1; i < len(reads); i++ {
			if !reads[i].After(reads[i-1]) {
				t.Errorf("ppb %d: time.Now went backward/stalled: read %d = %v not after read %d = %v", ppb, i, reads[i], i-1, reads[i-1])
			}
		}
	}
}

// TestDSTClockDriftRateValidation: Drift rejects a non-positive or reversed rate loudly
// (a stopped/reversed clock is a step, not drift) and accepts the valid range.
func TestDSTClockDriftRateValidation(t *testing.T) {
	for _, ppb := range []int64{-driftPPBBase, -driftPPBBase - 1, maxDriftPPB + 1, 5_000_000_000} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Drift(%d) did not panic (rate out of (0, 2])", ppb)
				}
			}()
			Drift(ppb)
		}()
	}
	for _, ppb := range []int64{-driftPPBBase + 1, -1, 0, 1, maxDriftPPB} {
		Drift(ppb) // must not panic
	}
}

// TestDSTClockDriftVictim enforces DST-FAULT-VICTIM for the rate leg: drift on host A
// does not change host B's clock or timers — B's 1s sleep still takes 1s of base.
func TestDSTClockDriftVictim(t *testing.T) {
	var aBase, bBase time.Duration
	Run(1, func() {
		aDone := make(chan struct{})
		bDone := make(chan struct{})
		var aStart, bStart time.Time
		Host("A", HostConfig{Clock: Drift(1_000_000_000)}, func() { // rate 2
			go func() { aStart = time.Now(); time.Sleep(time.Second); aDone <- struct{}{} }()
		})
		Host("B", HostConfig{}, func() { // rate 1, must be untouched
			go func() { bStart = time.Now(); time.Sleep(time.Second); bDone <- struct{}{} }()
		})
		root := time.Now()
		<-aDone
		aBase = time.Since(root)
		<-bDone
		bBase = time.Since(root)
		_ = aStart
		_ = bStart
	})
	if aBase != 500*time.Millisecond {
		t.Errorf("rate-2 host A woke at base %v, want 500ms", aBase)
	}
	if bBase != time.Second {
		t.Errorf("rate-1 host B woke at base %v, want 1s (drift on A must not touch B)", bBase)
	}
}

// TestDSTClockDriftN1 pins the N=1 collapse: Drift(0) (rate 1) is byte-identical to no
// drift — the wall reads base and timers fire at base+d, exactly as without the feature.
func TestDSTClockDriftN1(t *testing.T) {
	var rootT, hostT time.Time
	var baseAdvance time.Duration
	Run(1, func() {
		rootT = time.Now()
		done := make(chan struct{})
		start := time.Now()
		Host("h", HostConfig{Clock: Drift(0)}, func() {
			hostT = time.Now()
			go func() { time.Sleep(time.Second); close(done) }()
		})
		<-done
		baseAdvance = time.Since(start)
	})
	if !hostT.Equal(rootT) {
		t.Errorf("Drift(0) host wall = %v, want root wall %v (rate 1 must be in sync)", hostT, rootT)
	}
	if baseAdvance != time.Second {
		t.Errorf("Drift(0) host Sleep(1s) advanced base %v, want 1s (rate 1 must not convert)", baseAdvance)
	}
}

func TestDSTClockDriftLazyTimerTimestamp(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ppb          int64
		wantAtAnchor time.Duration
		wantDelayed  time.Duration
	}{
		{"fast", 1_000_000_000, -500 * time.Millisecond, -time.Second},
		{"slow", -500_000_000, time.Second, 2 * time.Second},
	} {
		for _, armAfter := range []time.Duration{0, time.Second} {
			for _, receiveAt := range []time.Duration{2 * time.Second, 3 * time.Second} {
				t.Run(tc.name+"/arm-"+armAfter.String()+"/receive-"+receiveAt.String(), func(t *testing.T) {
					var offset time.Duration
					Run(1, func() {
						Host("h", HostConfig{Clock: Drift(tc.ppb)}, func() {
							time.Sleep(armAfter)
							start := time.Now()
							timer := time.NewTimer(time.Second)
							time.Sleep(receiveAt)
							stamp := <-timer.C
							offset = time.Duration(stamp.UnixNano() - start.Add(time.Second).UnixNano())
						})
					})
					want := tc.wantAtAnchor
					if armAfter != 0 {
						want = tc.wantDelayed
					}
					if offset != want {
						t.Fatalf("lazy timer timestamp offset = %v, want %v", offset, want)
					}
				})
			}
		}
	}
}

// TestDSTClockDriftLargeDuration exercises the overflow-safe conversion: a long sleep on
// a drifting host converts without int64 overflow (d*1e9 would overflow naively).
func TestDSTClockDriftLargeDuration(t *testing.T) {
	if got := driftBaseAdvance(t, Drift(1_000_000_000), func() { time.Sleep(time.Hour) }); got != 30*time.Minute {
		t.Errorf("Sleep(1h) on a rate-2 host advanced base %v, want 30m (overflow-safe conversion)", got)
	}
}

// TestDSTClockDriftToBaseOverflowClamp is a regression for the overflow clamp: an extreme
// slow clock (rate near 0, valid) over a long host duration maps to a base duration near
// or past the int64 ceiling. The conversion must clamp to a positive value (a timer that
// never fires), never wrap to a negative when — a negative when trips modify's positivity
// throw. The high-term-only guard let the residual term push the sum past maxInt64; the
// property test's range cannot reach here, so this drives the helper directly.
func TestDSTClockDriftToBaseOverflowClamp(t *testing.T) {
	for _, tc := range []struct{ d, ppb int64 }{
		// The exact boundary the high-term-only guard missed: a = d/den = maxWhen/1e9
		// (un-clamped by the high term), with a residual lo > maxWhen - a*1e9 that pushes
		// the sum past maxInt64. den = 1e9+ppb = 100; d = 9223372036*100 + 86.
		{9_223_372_036*100 + 86, -999_999_900},
		{9_223_372_036*100 + 99, -999_999_900}, // a == threshold, max residual
		{1 << 62, -999_999_999},                // far past: high term alone overflows
		{maxWhen, -999_999_999},
	} {
		got := dstDriftToBase(tc.d, tc.ppb)
		if got <= 0 {
			t.Errorf("dstDriftToBase(%d, %d) = %d, want a positive clamped value, never a negative wrap", tc.d, tc.ppb, got)
		}
		if got > maxWhen {
			t.Errorf("dstDriftToBase(%d, %d) = %d exceeds maxWhen %d", tc.d, tc.ppb, got, int64(maxWhen))
		}
	}
}

// TestDSTClockDriftToBaseExact: the conversion is the exact integer CEILING of
// d*1e9/(1e9+ppb) (the rounding contract — the arm conversion rounds up so a timer never
// fires early in host-perceived time), matched against a big.Int oracle — including the
// d*1e9 overflow region (d > ~9.2s) the quotient/remainder split exists to handle, kept
// below the clamp ceiling.
func TestDSTClockDriftToBaseExact(t *testing.T) {
	rng := rand.New(rand.NewSource(0x0D817B45E))
	check := func(d, ppb int64) {
		num := new(big.Int).Mul(big.NewInt(d), big.NewInt(1_000_000_000))
		den := big.NewInt(1_000_000_000 + ppb)
		want, rem := new(big.Int).QuoRem(num, den, new(big.Int))
		if rem.Sign() != 0 {
			want.Add(want, big.NewInt(1)) // ceil: operands are positive
		}
		if got := dstDriftToBase(d, ppb); got != want.Int64() {
			t.Fatalf("dstDriftToBase(%d, %d) = %d, want %d (ceil)", d, ppb, got, want.Int64())
		}
	}
	for _, d := range []int64{0, 1, 999, int64(time.Second), int64(time.Hour), int64(100 * time.Hour)} {
		for _, ppb := range []int64{0, 1_000_000_000, -500_000_000, -900_000_000, 250_000, -7_777} {
			check(d, ppb)
		}
	}
	for i := 0; i < 200; i++ {
		check(rng.Int63n(1_000_000_000_000)+1, rng.Int63n(1_900_000_001)-900_000_000)
	}
}

// TestDSTClockDriftProperty is the property/fuzz coverage: for an arbitrary rate and
// duration, a Sleep(d) on the drifting host advances base by exactly ceil(d*1e9/(1e9+ppb))
// — the same integer-ceiling the runtime computes at the arm (the rounding contract) —
// verified against a big.Int oracle.
// Inputs are seeded by a test-local PRNG (not the DST RNG), bounded so no clamp applies.
func TestDSTClockDriftProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(0xD81F7C10C))
	for i := 0; i < 64; i++ {
		// ppb in [-9e8, 1e9] (rate in [0.1, 2]); d in [1, 1e9] ns. Then d/r <= 1e10, no clamp.
		ppb := rng.Int63n(1_900_000_001) - 900_000_000
		d := time.Duration(rng.Int63n(1_000_000_000) + 1)
		num := new(big.Int).Mul(big.NewInt(int64(d)), big.NewInt(1_000_000_000))
		den := big.NewInt(1_000_000_000 + ppb)
		want, rem := new(big.Int).QuoRem(num, den, new(big.Int))
		if rem.Sign() != 0 {
			want.Add(want, big.NewInt(1))
		}
		got := driftBaseAdvance(t, Drift(ppb), func() { time.Sleep(d) })
		if int64(got) != want.Int64() {
			t.Fatalf("ppb %d, Sleep(%v): base advance %d, want %d (ceil(d*1e9/(1e9+ppb)))", ppb, d, int64(got), want.Int64())
		}
	}
}

// TestDSTClockDriftHugeSleepFires is the arm-addition overflow regression
// (docs/dst/faults.md "Clock faults", overflow contract): time.Sleep(math.MaxInt64) —
// the standard block-forever idiom, whose when timeSleep clamps to maxWhen — on a
// slow-drifting host converts through dstDriftToBase, whose clamp returns maxWhen; the
// arm's `when = now + converted` then wraps negative unless the ADDITION is clamped
// too (bubble base time is ~9.47e17 ns). A wrapped when fails needsAdd, so the timer
// is silently never heaped and never fires: the sleeper parks forever and the run
// reports a deadlock neither real hardware (a 1 ppm-slow crystal fires after ~292y)
// nor the un-drifted simulation (which advances fake time to maxWhen and fires)
// exhibits — a harness-manufactured false positive. With the clamp, fake time advances
// to maxWhen and the sleeper wakes; without it, this test dies with the synctest
// deadlock panic.
func TestDSTClockDriftHugeSleepFires(t *testing.T) {
	woke := false
	Run(1, func() {
		done := make(chan struct{})
		Host("h", HostConfig{Clock: Drift(-1000)}, func() { // 1 ppm slow
			go func() {
				time.Sleep(math.MaxInt64)
				woke = true
				close(done)
			}()
		})
		<-done
	})
	if !woke {
		t.Fatal("Sleep(math.MaxInt64) on a slow-drifting host never fired")
	}
}
