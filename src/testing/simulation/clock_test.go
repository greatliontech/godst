// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"math/rand"
	_ "net" // TestDSTFaultVictimUnknownPanics reaches HostIP, whose linkname target lives in net
	"testing"
	"time"
	_ "unsafe" // for go:linkname
)

//go:linkname dstHostClockEnsure runtime.dstHostClockEnsure
func dstHostClockEnsure(host uint32)

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
// explicit Skew(0)) is in sync with the universe base clock, so its time.Now wall
// reading is identical to the root — byte-identical to the pre-feature universe-global
// clock. (The truly host-free program never allocates the per-host clock table at all;
// here the claim is about the observable reading, not internal allocation.)
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

// The following tests exercise the STEP clock fault (StepClock — a sudden NTP
// slew/correction over the per-host clock seam, docs/dst/faults.md "Clock faults").
// A step shifts only what time.Now reads on the victim host; timer deadlines read the
// base clock and are untouched. Each reads "before" and "after" across a StepClock
// that the harness arranges at a FROZEN base instant — the readers park on a channel
// (channel ops never advance the synctest clock, only timer waits do), so the only
// change between the two readings is the step, and after.Sub(before) is exactly the
// step with no base advance to subtract out.

// TestDSTClockStepForward: a forward step jumps the host's time.Now ahead by the step,
// and does not touch another host's clock (the root, host 0, here).
func TestDSTClockStepForward(t *testing.T) {
	const step = 500 * time.Millisecond
	var before, after, rootBefore, rootAfter time.Time
	Run(1, func() {
		ready := make(chan struct{})
		go1 := make(chan struct{})
		done := make(chan struct{})
		Host("h", HostConfig{}, func() {
			go func() {
				before = time.Now()
				close(ready)
				<-go1
				after = time.Now()
				close(done)
			}()
		})
		<-ready
		rootBefore = time.Now()
		StepClock("h", step)
		rootAfter = time.Now()
		close(go1)
		<-done
	})
	if got := after.Sub(before); got != step {
		t.Errorf("forward step: host time.Now jumped %v across StepClock(+%v), want %v", got, step, step)
	}
	if !rootBefore.Equal(rootAfter) {
		t.Errorf("root clock moved across a StepClock on host h: %v != %v (a step on h must not touch host 0, and the base must be frozen)", rootBefore, rootAfter)
	}
}

// TestDSTClockStepBackward: a backward step makes the host's wall time go backward —
// exactly the adversary an HLC is built to tolerate (DST-FAULT-SOUND clock class: a
// real NTP correction can move wall backward).
func TestDSTClockStepBackward(t *testing.T) {
	const step = -300 * time.Millisecond
	var before, after time.Time
	Run(1, func() {
		ready := make(chan struct{})
		go1 := make(chan struct{})
		done := make(chan struct{})
		Host("h", HostConfig{}, func() {
			go func() {
				before = time.Now()
				close(ready)
				<-go1
				after = time.Now()
				close(done)
			}()
		})
		<-ready
		StepClock("h", step)
		close(go1)
		<-done
	})
	if got := after.Sub(before); got != step {
		t.Errorf("backward step: host time.Now moved %v across StepClock(%v), want %v", got, step, step)
	}
	if !after.Before(before) {
		t.Errorf("backward step: after=%v is not before before=%v — wall must go backward (the HLC adversary)", after, before)
	}
}

// TestDSTClockStepTimerImmune is the load-bearing soundness test: a step shifts only
// the wall reading, never timer deadlines. A relative timer armed on a host BEFORE a
// step still fires after exactly its delay in BASE time, regardless of a large
// concurrent step — so timeouts/leases built on timers and contexts are step-immune
// (DST-CLOCK-DURATION, dynamic case). Base advance is measured on the root (host 0,
// unstepped), so it is the true base elapsed.
func TestDSTClockStepTimerImmune(t *testing.T) {
	const delay = 1 * time.Second
	const step = 5 * time.Second
	var baseAdvance time.Duration
	Run(1, func() {
		armed := make(chan struct{})
		fired := make(chan struct{})
		Host("h", HostConfig{}, func() {
			go func() {
				timer := time.After(delay) // armed at base T0
				close(armed)
				<-timer
				close(fired)
			}()
		})
		<-armed
		rootStart := time.Now() // root (host 0): base T0
		StepClock("h", step)    // step h forward 5s while its timer is pending
		<-fired
		baseAdvance = time.Since(rootStart)
	})
	if baseAdvance != delay {
		t.Errorf("relative timer on a stepped host fired after %v of base time, want %v (a step must not move timer deadlines)", baseAdvance, delay)
	}
}

// TestDSTClockStepVictim enforces DST-FAULT-VICTIM for the clock leg: StepClock(hA)
// moves every goroutine of host hA together and leaves host hB untouched.
func TestDSTClockStepVictim(t *testing.T) {
	const step = 700 * time.Millisecond
	var aDelta, a2Delta, bDelta time.Duration
	Run(1, func() {
		ready := make(chan struct{}, 3)
		go1 := make(chan struct{})
		done := make(chan struct{}, 3)
		measure := func(d *time.Duration) {
			b := time.Now()
			ready <- struct{}{}
			<-go1
			*d = time.Now().Sub(b)
			done <- struct{}{}
		}
		Host("hA", HostConfig{}, func() {
			go measure(&aDelta) // two goroutines of hA must move together
			go measure(&a2Delta)
		})
		Host("hB", HostConfig{}, func() {
			go measure(&bDelta) // hB must not move
		})
		<-ready
		<-ready
		<-ready
		StepClock("hA", step) // only hA
		close(go1)
		<-done
		<-done
		<-done
	})
	if aDelta != step || a2Delta != step {
		t.Errorf("hA goroutines moved %v/%v across StepClock(hA,+%v), want %v each (a step moves the host's whole subtree)", aDelta, a2Delta, step, step)
	}
	if bDelta != 0 {
		t.Errorf("hB moved %v across StepClock(hA,...), want 0 (DST-FAULT-VICTIM: a step on hA must not touch hB)", bDelta)
	}
}

// TestDSTClockStepAccumulate: successive steps add, on top of a configured base skew —
// the host's offset is base skew + the sum of its steps.
func TestDSTClockStepAccumulate(t *testing.T) {
	const base = 100 * time.Millisecond
	const s1 = 250 * time.Millisecond
	const s2 = -90 * time.Millisecond
	var before, after time.Time
	Run(1, func() {
		ready := make(chan struct{})
		go1 := make(chan struct{})
		done := make(chan struct{})
		Host("h", HostConfig{Clock: Skew(base)}, func() {
			go func() {
				before = time.Now()
				close(ready)
				<-go1
				after = time.Now()
				close(done)
			}()
		})
		<-ready
		StepClock("h", s1)
		StepClock("h", s2)
		close(go1)
		<-done
	})
	if got := after.Sub(before); got != s1+s2 {
		t.Errorf("accumulated steps over a base skew: host moved %v, want %v (steps add on top of the configured skew)", got, s1+s2)
	}
}

// TestDSTClockStepDeterminism enforces DST-FAULT-REPLAY / DST-CLOCK-DET for steps: the
// same seed and the same step schedule produce identical readings, including a reading
// after a relative timer fires (the step does not perturb the timer).
func TestDSTClockStepDeterminism(t *testing.T) {
	run := func() (time.Time, time.Time) {
		var a, b time.Time
		Run(7, func() {
			ready := make(chan struct{})
			go1 := make(chan struct{})
			done := make(chan struct{})
			Host("h", HostConfig{Clock: Skew(40 * time.Millisecond)}, func() {
				go func() {
					close(ready)
					<-go1
					a = time.Now()
					<-time.After(100 * time.Millisecond)
					b = time.Now()
					close(done)
				}()
			})
			<-ready
			StepClock("h", 250*time.Millisecond)
			StepClock("h", -80*time.Millisecond)
			close(go1)
			<-done
		})
		return a, b
	}
	a1, b1 := run()
	a2, b2 := run()
	if !a1.Equal(a2) || !b1.Equal(b2) {
		t.Errorf("stepped-clock readings not reproducible across same-seed runs: a %v/%v, b %v/%v", a1, a2, b1, b2)
	}
}

// stepFrozen runs body inside a host "h" whose worker reads time.Now into *before,
// then parks; the harness calls do (which arranges steps at a frozen base instant —
// channel handoffs never advance the synctest clock), then the worker reads *after.
// So *after − *before isolates exactly what do() did to h's clock, with no base
// advance to subtract. config configures h's base clock.
func stepFrozen(config HostConfig, do func(), before, after *time.Time) {
	Run(1, func() {
		ready := make(chan struct{})
		go1 := make(chan struct{})
		done := make(chan struct{})
		Host("h", config, func() {
			go func() {
				*before = time.Now()
				close(ready)
				<-go1
				*after = time.Now()
				close(done)
			}()
		})
		<-ready
		do()
		close(go1)
		<-done
	})
}

// TestDSTClockStepProperty is the property/fuzz coverage behind the example pins: for
// an arbitrary delta the host's wall jumps by EXACTLY that delta, and for an arbitrary
// SEQUENCE of steps over an arbitrary base skew the host lands at base + the sum of the
// steps (steps are atomic adds, so they compose and commute with the skew). Inputs are
// drawn by a test-local deterministic PRNG (NOT the DST RNG) and generated outside Run,
// so the test itself is reproducible; the magnitudes stay within ±~2s so the assertions
// are about the arithmetic, not time-representation extremes.
func TestDSTClockStepProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(0xC10C57E9))

	// Single step: wall jumps by exactly delta, forward and backward, edges included.
	deltas := []time.Duration{
		0, 1, -1,
		time.Nanosecond, -time.Nanosecond,
		time.Millisecond, -time.Millisecond,
		time.Second, -time.Second,
	}
	for i := 0; i < 80; i++ {
		deltas = append(deltas, time.Duration(rng.Int63n(2_000_000_001)-1_000_000_000))
	}
	for _, d := range deltas {
		var before, after time.Time
		stepFrozen(HostConfig{}, func() { StepClock("h", d) }, &before, &after)
		if got := after.Sub(before); got != d {
			t.Fatalf("single step %v: host wall jumped %v, want exactly %v", d, got, d)
		}
	}

	// Step sequence over a base skew: lands at sum(steps), independent of the skew.
	for trial := 0; trial < 50; trial++ {
		baseSkew := time.Duration(rng.Int63n(200_000_001) - 100_000_000)
		n := 1 + rng.Intn(6)
		steps := make([]time.Duration, n)
		var sum time.Duration
		for j := range steps {
			steps[j] = time.Duration(rng.Int63n(400_000_001) - 200_000_000)
			sum += steps[j]
		}
		var before, after time.Time
		stepFrozen(HostConfig{Clock: Skew(baseSkew)}, func() {
			for _, s := range steps {
				StepClock("h", s)
			}
		}, &before, &after)
		if got := after.Sub(before); got != sum {
			t.Fatalf("step sequence %v over skew %v: host moved %v, want sum %v", steps, baseSkew, got, sum)
		}
	}
}

// TestDSTClockStepWallDurationReflectsStep pins the recorded soundness boundary (see
// docs/dst/faults.md "Clock faults"): inside a bubble time.Now carries no monotonic
// component, so a wall-derived duration (time.Since) ACROSS a step reflects the step —
// the model, matching wall-clock arithmetic across a real NTP step. With the base
// frozen (no timer waited), the whole measured duration IS the step. A future change
// that made in-bubble durations step-immune (e.g. a per-host monotonic reading) would
// fail this and force a deliberate contract revisit, not slip through.
func TestDSTClockStepWallDurationReflectsStep(t *testing.T) {
	const step = 400 * time.Millisecond
	var since time.Duration
	Run(1, func() {
		ready := make(chan struct{})
		go1 := make(chan struct{})
		done := make(chan struct{})
		var start time.Time
		Host("h", HostConfig{}, func() {
			go func() {
				start = time.Now()
				close(ready)
				<-go1
				since = time.Since(start) // wall-based in the bubble; spans the step
				close(done)
			}()
		})
		<-ready
		StepClock("h", step) // base frozen by the channel handoff; only the offset moves
		close(go1)
		<-done
	})
	if since != step {
		t.Errorf("time.Since across a +%v step = %v, want %v (wall-derived durations reflect a step — the recorded boundary)", step, since, step)
	}
}

// TestDSTClockStepResetByRedeclare verifies the restart semantic documented on Host
// and dstReestablishHostClock: re-declaring a host re-establishes its configured base
// clock, discarding any step taken before the re-declaration (a reboot re-syncs to
// config). A long-lived worker of the first declaration reads the stepped offset; the
// re-declared host reads the reset offset. Base is frozen throughout.
func TestDSTClockStepResetByRedeclare(t *testing.T) {
	const skew = 60 * time.Millisecond
	const step = 500 * time.Millisecond
	var root, stepped, restarted time.Time
	Run(1, func() {
		root = time.Now()
		readStepped := make(chan struct{})
		doneStepped := make(chan struct{})
		Host("h", HostConfig{Clock: Skew(skew)}, func() {
			go func() {
				<-readStepped
				stepped = time.Now() // table[h] = skew + step
				close(doneStepped)
			}()
		})
		StepClock("h", step)
		close(readStepped)
		<-doneStepped
		// Re-declare host h (restart): overwrites its offset back to the configured skew.
		Host("h", HostConfig{Clock: Skew(skew)}, func() {
			restarted = time.Now()
		})
	})
	if got := stepped.Sub(root); got != skew+step {
		t.Errorf("before restart host offset = %v, want %v (skew + step)", got, skew+step)
	}
	if got := restarted.Sub(root); got != skew {
		t.Errorf("after re-declaring host its offset = %v, want %v (restart must discard the step, re-establishing config)", got, skew)
	}
}

// TestDSTClockStepOutsideRunNoop verifies the bubble guard: StepClock outside a Run is
// a no-op — it must not panic and must not leak into a later run.
func TestDSTClockStepOutsideRunNoop(t *testing.T) {
	StepClock("h", time.Hour) // no bubble → no-op (must not panic or allocate a leaking table)
	var rootT, hostT time.Time
	Run(1, func() {
		rootT = time.Now()
		Host("h", HostConfig{}, func() { hostT = time.Now() })
	})
	if !hostT.Equal(rootT) {
		t.Errorf("StepClock before a run leaked into it: host=%v root=%v (outside-run calls must be no-ops)", hostT, rootT)
	}
}

// TestDSTClockHostBound verifies the per-host clock bound is enforced loudly (a
// non-silent cap), mirroring TestDSTMemProcessBound for the process counter: an
// over-bound host id at the choke point (dstHostClockEnsure, which Host/StepClock
// reach) panics rather than silently dropping the host's clock state.
func TestDSTClockHostBound(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("dstHostClockEnsure past the host bound did not panic (silent cap)")
		}
	}()
	dstHostClockEnsure(1 << 20) // far beyond dstMaxSimHosts; must panic
}

// TestDSTFaultVictimUnknownPanics: every fault/inspection API that names a victim
// panics during a run on an undeclared name instead of interning a fresh host or
// process id whose state no goroutine observes — a typo'd victim must fail loud,
// never silently test nothing (docs/dst/faults.md "Targeting", victim names fail
// loud). All intake points share the lookupHost/lookupProc choke.
func TestDSTFaultVictimUnknownPanics(t *testing.T) {
	cases := []struct {
		name string
		call func()
	}{
		{"StepClock", func() { StepClock("no-such-host", time.Second) }},
		{"DriftClock", func() { DriftClock("no-such-host", 1000) }},
		{"Partition", func() { Partition("declared", "no-such-host") }},
		{"Heal", func() { Heal("declared", "no-such-host") }},
		{"Isolate", func() { Isolate("no-such-host") }},
		{"HealHost", func() { HealHost("no-such-host") }},
		{"Reset", func() { Reset("declared", "no-such-host") }},
		{"ResetProcess", func() { ResetProcess("no-such-proc") }},
		{"FailDisk", func() { FailDisk("no-such-host") }},
		{"HealDisk", func() { HealDisk("no-such-host") }},
		{"FailWriteback", func() { FailWriteback("no-such-host") }},
		{"HealWriteback", func() { HealWriteback("no-such-host") }},
		{"FailFile", func() { FailFile("no-such-host", "/f") }},
		{"HealFile", func() { HealFile("no-such-host", "/f") }},
		{"LimitDisk", func() { LimitDisk("no-such-host", 1) }},
		{"CorruptFile", func() { CorruptFile("no-such-host", "/f") }},
		{"UnlimitDisk", func() { UnlimitDisk("no-such-host") }},
		{"SlowDisk", func() { SlowDisk("no-such-host", time.Millisecond) }},
		{"HostFS", func() { HostFS("no-such-host") }},
		{"HostIP", func() { HostIP("no-such-host") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var recovered any
			Run(1, func() {
				Host("declared", HostConfig{}, func() {})
				func() {
					defer func() { recovered = recover() }()
					tc.call()
				}()
			})
			if recovered == nil {
				t.Errorf("%s on an undeclared victim name did not panic (a typo'd victim silently tests nothing)", tc.name)
			}
		})
	}
}

// TestDSTFaultVictimOutsideRunNoop: the same victim-name APIs stay documented no-ops
// outside a run — the fail-loud check applies only while a run is active (outside one,
// the registry belongs to no run and every downstream op already discards the call).
func TestDSTFaultVictimOutsideRunNoop(t *testing.T) {
	StepClock("nobody", time.Second)
	FailDisk("nobody")
	Partition("nobody", "nobody-else")
	ResetProcess("nobody") // none of these may panic outside a run
}

// TestDSTClockBoundedDriftSeeded checks BoundedDrift: the clock RATE departure is
// within the bound, is a deterministic function of (seed, host) — stable across a host
// re-declaration (restart) and reproducible at a fixed seed — and varies across seeds
// (the permutation knob for exploring bounded drift). It is observed via base advance:
// a rate-r host's time.Sleep(d) takes d/r of base time, so the measured base interval
// is a pure function of the seeded rate. Drawing it advances no RNG stream, which the
// restart-stability check exercises (mirrors TestDSTClockBoundedSeeded for skew).
func TestDSTClockBoundedDriftSeeded(t *testing.T) {
	const maxPPB = 100_000_000 // ±0.1 → rate in [0.9, 1.1]
	const hostSleep = time.Second
	// baseFor measures the BASE time a 1s host-sleep takes under the seeded rate.
	baseFor := func(seed uint64, name string, checkRestart bool) time.Duration {
		var base time.Duration
		Run(seed, func() {
			measure := func() time.Duration {
				start := time.Now() // root = base
				done := make(chan struct{})
				Host(name, HostConfig{Clock: BoundedDrift(maxPPB)}, func() {
					go func() { time.Sleep(hostSleep); close(done) }()
				})
				<-done
				return time.Since(start)
			}
			base = measure()
			if checkRestart {
				if got := measure(); got != base { // restart: same seed+host → same rate
					t.Errorf("restart of host %q changed seeded rate: base %v != %v (must depend only on seed+host)", name, got, base)
				}
			}
		})
		return base
	}
	// base = hostSleep/rate; rate in [1-maxPPB/1e9, 1+maxPPB/1e9] → base in [lo, hi].
	// The runtime converts the sleep with CEIL rounding, so allow the extreme-rate base
	// to exceed the floor-computed hi by the ≤1ns round-up (lo is safe: ceil ≥ floor).
	lo := time.Duration(int64(hostSleep) * driftPPBBase / (driftPPBBase + maxPPB))
	hi := time.Duration(int64(hostSleep)*driftPPBBase/(driftPPBBase-maxPPB)) + 1
	if b := baseFor(1, "h", true); b < lo || b > hi {
		t.Errorf("BoundedDrift base advance %v out of [%v, %v] (rate out of bound)", b, lo, hi)
	}
	if a, b := baseFor(7, "h", false), baseFor(7, "h", false); a != b {
		t.Errorf("BoundedDrift rate not reproducible at same seed: %v != %v", a, b)
	}
	seen := map[time.Duration]bool{}
	for seed := uint64(1); seed <= 12; seed++ {
		seen[baseFor(seed, "h", false)] = true
	}
	if len(seen) < 2 {
		t.Errorf("BoundedDrift rate identical across 12 seeds (%v); the seed must vary the rate", seen)
	}
}
