// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"testing"
	"time"
)

// TestDSTClockSkewApplied verifies the per-host clock substrate (docs/dst/faults.md
// "Per-host clock"): a host's configured offset shifts what time.Now reads on that
// host, co-located processes and child goroutines inherit the host's clock, and two
// hosts carry independent offsets. All readings here are taken at the same base
// instant (nothing between them blocks on a timer, so the synctest clock does not
// advance), so a reading's distance from the root reading is exactly its offset.
func TestDSTClockSkewApplied(t *testing.T) {
	const skewA = 50 * time.Millisecond
	const skewB = -30 * time.Millisecond
	var root, hostA, procA, childA, hostB time.Time
	Run(1, func() {
		root = time.Now()
		Host("hA", HostConfig{Clock: Skew(skewA)}, func() {
			hostA = time.Now()
			Process("p", func() { procA = time.Now() }) // inherits hA's clock
			done := make(chan struct{})
			go func() {
				childA = time.Now() // child inherits hA's clock
				close(done)
			}()
			<-done
		})
		Host("hB", HostConfig{Clock: Skew(skewB)}, func() {
			hostB = time.Now()
		})
	})

	if got := hostA.Sub(root); got != skewA {
		t.Errorf("host hA Now - root Now = %v, want %v (skew not applied to wall)", got, skewA)
	}
	if got := procA.Sub(root); got != skewA {
		t.Errorf("co-located process Now - root = %v, want %v (process must inherit host clock)", got, skewA)
	}
	if got := childA.Sub(root); got != skewA {
		t.Errorf("child goroutine Now - root = %v, want %v (child must inherit host clock)", got, skewA)
	}
	if got := hostB.Sub(root); got != skewB {
		t.Errorf("host hB Now - root = %v, want %v", got, skewB)
	}
	if got := hostA.Sub(hostB); got != skewA-skewB {
		t.Errorf("host hA Now - host hB Now = %v, want %v (per-host offsets must be independent)", got, skewA-skewB)
	}
}

// TestDSTClockDurationPreserved enforces DST-CLOCK-DURATION: a static per-host
// offset shifts only the wall reading, never monotonic time, durations, or timer
// deadlines. So a duration measured on a skewed host (the offset cancels in the
// subtraction) and the base-clock advance caused by the host's sleep are both
// exactly the slept interval, for every offset — including the strongest
// counterexample to a wrong implementation that folds the offset into bubble.now
// (which would still pass a naive "Now differs per host" check but skew durations).
func TestDSTClockDurationPreserved(t *testing.T) {
	for _, skew := range []time.Duration{0, 50 * time.Millisecond, -50 * time.Millisecond} {
		var sinceElapsed, baseAdvance time.Duration
		Run(1, func() {
			rootStart := time.Now() // on root (no skew)
			done := make(chan struct{})
			Host("h", HostConfig{Clock: Skew(skew)}, func() {
				go func() {
					start := time.Now()
					time.Sleep(time.Second)
					sinceElapsed = time.Since(start) // on the skewed host
					close(done)
				}()
			})
			<-done
			baseAdvance = time.Since(rootStart) // base advance over the host's 1s sleep
		})
		if sinceElapsed != time.Second {
			t.Errorf("skew=%v: time.Since over a 1s sleep on skewed host = %v, want 1s (offset must cancel in a duration)", skew, sinceElapsed)
		}
		if baseAdvance != time.Second {
			t.Errorf("skew=%v: base clock advanced %v over the host's 1s sleep, want 1s (offset must not perturb timers)", skew, baseAdvance)
		}
	}
}

// TestDSTClockDeterminism enforces DST-CLOCK-DET: the same seed and the same host
// clock config produce identical per-host time.Now readings and identical timer
// firings. It mixes a fixed Skew and a seeded BoundedSkew and a relative timer, and
// asserts two same-seed runs agree on every reading.
func TestDSTClockDeterminism(t *testing.T) {
	type sample struct {
		a, b, fired time.Time
		bAdvance    time.Duration
	}
	run := func() sample {
		var s sample
		Run(99, func() {
			Host("hA", HostConfig{Clock: Skew(40 * time.Millisecond)}, func() {
				s.a = time.Now()
			})
			Host("hB", HostConfig{Clock: BoundedSkew(100 * time.Millisecond)}, func() {
				s.b = time.Now()
				start := time.Now()
				<-time.After(250 * time.Millisecond)
				s.fired = time.Now()
				s.bAdvance = s.fired.Sub(start)
			})
		})
		return s
	}
	x, y := run(), run()
	if !x.a.Equal(y.a) || !x.b.Equal(y.b) || !x.fired.Equal(y.fired) || x.bAdvance != y.bAdvance {
		t.Errorf("clock readings not reproducible across same-seed runs:\n x=%+v\n y=%+v", x, y)
	}
	// The seeded host's timer still fires after exactly its relative delay (the
	// offset does not perturb durations).
	if x.bAdvance != 250*time.Millisecond {
		t.Errorf("seeded-skew host timer advance = %v, want 250ms", x.bAdvance)
	}
}

// TestDSTClockBoundedSeeded checks BoundedSkew: the offset is within the bound, is a
// deterministic function of (seed, host) — stable across a host re-declaration
// (restart) and reproducible at a fixed seed — and varies across seeds (the
// permutation knob for exploring bounded skew). Drawing it advances no RNG stream,
// which the restart-stability check exercises.
func TestDSTClockBoundedSeeded(t *testing.T) {
	const bound = 100 * time.Millisecond
	offsetOf := func(seed uint64, name string, checkRestart bool) time.Duration {
		var off time.Duration
		Run(seed, func() {
			root := time.Now()
			Host(name, HostConfig{Clock: BoundedSkew(bound)}, func() {
				off = time.Now().Sub(root)
			})
			if checkRestart {
				Host(name, HostConfig{Clock: BoundedSkew(bound)}, func() {
					if got := time.Now().Sub(root); got != off {
						t.Errorf("restart of host %q changed seeded offset: %v != %v (must depend only on seed+host)", name, got, off)
					}
				})
			}
		})
		return off
	}

	if o := offsetOf(1, "h", true); o < -bound || o > bound {
		t.Errorf("BoundedSkew offset %v out of [-%v, +%v]", o, bound, bound)
	}
	if a, b := offsetOf(7, "h", false), offsetOf(7, "h", false); a != b {
		t.Errorf("BoundedSkew offset not reproducible at same seed: %v != %v", a, b)
	}
	seen := map[time.Duration]bool{}
	for seed := uint64(1); seed <= 12; seed++ {
		seen[offsetOf(seed, "h", false)] = true
	}
	if len(seen) < 2 {
		t.Errorf("BoundedSkew offset identical across 12 seeds (%v); the seed must vary the skew", seen)
	}
}

// TestDSTClockN1Collapse pins the N=1 collapse: a host with no clock config (or an
// explicit Skew(0)) is in sync with the universe base clock, so its time.Now reads
// identically to the root — byte-identical to pre-feature behavior.
func TestDSTClockN1Collapse(t *testing.T) {
	var rootT, plainHostT, zeroSkewT time.Time
	Run(1, func() {
		rootT = time.Now()
		Host("h", HostConfig{}, func() { plainHostT = time.Now() })
		Host("h2", HostConfig{Clock: Skew(0)}, func() { zeroSkewT = time.Now() })
	})
	if !plainHostT.Equal(rootT) {
		t.Errorf("HostConfig{} host wall = %v, want root wall %v (zero config must be no skew)", plainHostT, rootT)
	}
	if !zeroSkewT.Equal(rootT) {
		t.Errorf("Skew(0) host wall = %v, want root wall %v", zeroSkewT, rootT)
	}
}
