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
func dstStepHostClock(host uint32, delta int64)

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
// Steps accumulate. Calls outside a run are no-ops. Call from within a Run.
func StepClock(host string, delta time.Duration) {
	dstStepHostClock(internHost(host), int64(delta))
}
