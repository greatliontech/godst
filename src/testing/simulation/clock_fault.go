// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"time"
	_ "unsafe" // for go:linkname
)

// Clock faults over the per-host clock seam (docs/dst/faults.md "Clock faults"). A
// step is a sudden wall-clock jump — an NTP slew/correction — injected during a run,
// the clock-axis counterpart to the network targeting API (Partition / Reset): it
// names a host (the same name passed to Host), interns it to the host id the per-host
// clock table is keyed by, and drives the step through runtime (always linked, so
// simulation needs no extra dependency). A step shifts only what time.Now reads on the
// host — its whole subtree (every process and goroutine) moves together; timer
// deadlines (time.After, time.NewTimer, context deadlines) read the base clock and are
// untouched, so they still fire at the same base time. Calls outside a run are no-ops.

//go:linkname dstStepHostClock runtime.dstStepHostClock
func dstStepHostClock(host uint32, delta int64) bool

//go:linkname dstDriftHostClock runtime.dstDriftHostClock
func dstDriftHostClock(host uint32, ppb int64)

// StepClock applies an instantaneous wall-clock step to the named host's clock,
// modeling an NTP slew/correction during the run. A positive delta jumps the host's
// time.Now forward, a negative delta backward; a backward step makes wall-time
// arithmetic on the host go backward — exactly the adversary a hybrid logical clock is
// built to tolerate. The step shifts only what time.Now reads on the host (across all
// its processes and goroutines), not the rate of time: monotonic durations measured by
// timers and relative deadlines (time.After, context.WithTimeout) are unaffected,
// because an NTP step corrects the wall clock, not the oscillator (drift — a clock
// running fast or slow — is the separate fault that perturbs the rate). Note that a
// duration computed by subtracting two wall readings (time.Since across the step)
// reflects the step, as it would for code that reads the wall clock on real hardware;
// for step-immune deadlines use a timer or context, which the step does not move.
// Steps accumulate. Panics during a run on a host name no Host declaration has
// established (a typo'd victim must fail loud, never silently test nothing).
// Calls outside a run are no-ops. Call from within a Run.
// A step that would take the host's wall before the epoch panics: settimeofday
// rejects a pre-epoch wall clock, so no real machine can hold one (the
// wall-representability boundary, docs/dst/faults.md "Clock faults").
func StepClock(host string, delta time.Duration) {
	requireBubbleFaultCaller("StepClock")
	if !dstStepHostClock(lookupHost(host), int64(delta)) {
		panic("testing/simulation: StepClock takes the host's wall clock before the epoch (no real kernel accepts a pre-epoch wall clock)")
	}
}

// DriftClock changes the named host's clock rate to a departure of ppb parts-per-billion
// (rate 1 + ppb/1e9) mid-run — the dynamic complement of the declared Drift: a host can
// start drifting at one point and re-sync (DriftClock(host, 0)) at another, modeling a
// crystal whose error appears over a window or an NTP discipline that corrects the rate.
// From the change instant the host's wall advances at the new rate and its relative
// timers convert at the new rate; the wall stays continuous across the change (no jump),
// and every timer already pending on the host is re-mapped so it still fires after the
// host-perceived time it was set for. ppb must be in (-1e9, 1e9] (rate in (0, 2]);
// DriftClock panics otherwise (a non-positive or reversed rate is a step, not drift).
// Affects exactly the named host, panicking during a run on an undeclared host name.
// Calls outside a run are no-ops. Call from within a Run.
func DriftClock(host string, ppb int64) {
	requireBubbleFaultCaller("DriftClock")
	if ppb <= -driftPPBBase || ppb > maxDriftPPB {
		panic("testing/simulation: DriftClock ppb out of range (-1e9, 1e9]; rate must be in (0, 2]")
	}
	dstDriftHostClock(lookupHost(host), ppb)
}
