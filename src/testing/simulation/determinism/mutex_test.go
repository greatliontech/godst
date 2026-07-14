// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package determinism

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/simulation"
	"time"
)

var spinSink int

// spin burns a fixed instruction count (roughly the wall duration the caller
// calibrated it to), the shape of an ordinary CPU-bound critical section.
func spin(iters int) {
	s := spinSink
	for i := 0; i < iters; i++ {
		s += i ^ (s << 1)
	}
	spinSink = s
}

// mutexProgram: a holder repeatedly barges on a contended mutex around a
// wall-measurable critical section while a waiter sits parked on the lock,
// re-attempting at every cooperative yield. The transcript records the
// waiter's acquisition slot among the holder's critical sections.
func mutexProgram(seed uint64, itersPerSlice int) string {
	var b strings.Builder
	simulation.Run(seed, func() {
		var mu sync.Mutex
		done := make(chan struct{})
		mu.Lock() // held before the waiter ever attempts
		go func() {
			mu.Lock()
			b.WriteString("W;")
			mu.Unlock()
			close(done)
		}()
		runtime.Gosched() // the waiter parks on the held lock
		for k := 0; k < 12; k++ {
			spin(itersPerSlice) // ~300us of wall inside the critical section
			runtime.Gosched()   // the waiter re-attempts while mu is held
			b.WriteString(fmt.Sprintf("H%d;", k))
			mu.Unlock()
			mu.Lock() // barge: retaken before the woken waiter runs
		}
		mu.Unlock()
		<-done
	})
	return b.String()
}

// TestMutexStarvationHandoffDeterministic: sync.Mutex's starvation-mode
// switch never reads the wall clock in-bubble — the flip is the waiter's
// deterministic lost-wakeup count (design.md, the sync.Mutex row), a pure
// function of the seed. On the wall clock the switch fires once a waiter has
// waited 1ms, which under this program's ~300us critical sections happens at
// a wall-jittered iteration: the waiter's acquisition slot then varies
// between identical same-seed runs (demonstrated 19-vs-1 over 20 runs
// pre-fix), and with load or GC pauses any contended mutex in any SUT is
// exposed. The pin asserts both cross-run equality and the deterministic
// outcome itself: this program's 12 barge rounds sit far below the
// lost-wakeup threshold, so the holder is never preempted by a starvation
// handoff — wall-timed OR count-timed — and the waiter acquires only after
// the holder's final unlock.
func TestMutexStarvationHandoffDeterministic(t *testing.T) {
	// Calibrate the spin to ~300us of wall time per critical section, so the
	// waiter's cumulative wall wait crosses the 1ms starvation threshold
	// mid-run — the regression's firing shape — on any machine speed.
	start := time.Now()
	spin(1 << 22)
	perNs := float64(1<<22) / float64(time.Since(start).Nanoseconds())
	iters := int(perNs * 300_000)

	want := ""
	for k := 0; k < 12; k++ {
		want += fmt.Sprintf("H%d;", k)
	}
	want += "W;"
	for run := 0; run < 8; run++ {
		got := mutexProgram(5, iters)
		if got != want {
			t.Fatalf("run %d: wall-clock state reached the mutex handoff order:\n got=%s\nwant=%s", run, got, want)
		}
	}
}

// starvationHandoffProgram: the recorded false-positive livelock shape — a
// holder loops Lock / yield / Unlock / re-Lock, re-barging on every round
// (the woken waiter resumes only at the holder's yield, attempts the CAS
// against a held lock, and requeues), while the parked waiter must acquire
// the mutex for the program to terminate. Under a pure barging
// determinization this livelocks in-sim forever — undetectably, since the
// non-durable mutex wait never advances fake time and the bubble-deadlock
// panic cannot fire — where production always terminates once the waiter's
// wall wait crosses the 1ms starvation threshold. Returns the number of
// holder rounds until the waiter's acquisition ended the loop.
func starvationHandoffProgram(seed uint64) int {
	rounds := 0
	simulation.Run(seed, func() {
		var mu sync.Mutex
		done := false
		waiterDone := make(chan struct{})
		mu.Lock() // held before the waiter ever attempts
		go func() {
			mu.Lock() // must succeed for the program to advance
			done = true
			mu.Unlock()
			close(waiterDone)
		}()
		runtime.Gosched() // the waiter parks on the held lock
		for {
			rounds++
			runtime.Gosched() // the waiter re-attempts while mu is held
			mu.Unlock()
			mu.Lock() // barge: retaken before the woken waiter runs
			if done {
				break
			}
		}
		mu.Unlock()
		<-waiterDone
	})
	return rounds
}

// TestMutexStarvationHandoffLiveness: a production-legal SUT whose
// termination depends on the starvation-mode handoff terminates in-sim. The
// waiter's lost-wakeup count crosses the deterministic threshold, the mutex
// flips to starvation mode, and the next unlock hands off directly — exactly
// the liveness production's 1ms wall flip guarantees. The round count proves
// the handoff was the count-triggered flip, not a lucky barge loss: the
// waiter lost well over the threshold's worth of consecutive wakeups first.
func TestMutexStarvationHandoffLiveness(t *testing.T) {
	rounds := starvationHandoffProgram(7)
	// The flip fires once the waiter has lost more than 64 consecutive
	// wakeups (internal/sync's threshold); a couple of rounds of handoff
	// and teardown follow. Bound loosely above so scheduler details don't
	// make the pin brittle; the load-bearing bounds are termination itself
	// and >64 (the flip, not luck).
	if rounds <= 64 {
		t.Fatalf("starvation-dependent program terminated after %d rounds, want >64: the waiter must acquire via the count-triggered starvation handoff, not a barge loss", rounds)
	}
	if rounds > 256 {
		t.Fatalf("starvation-dependent program took %d rounds, want prompt handoff shortly past the 64-loss threshold", rounds)
	}
}

// TestMutexStarvationHandoffCountDeterministic: the lost-wakeup starvation
// flip is a pure function of the seed — the same seed yields the identical
// round count on every run.
func TestMutexStarvationHandoffCountDeterministic(t *testing.T) {
	for seed := uint64(1); seed <= 4; seed++ {
		first := starvationHandoffProgram(seed)
		for run := 1; run < 4; run++ {
			if got := starvationHandoffProgram(seed); got != first {
				t.Fatalf("seed %d run %d: %d rounds, want %d (same-seed stability)", seed, run, got, first)
			}
		}
	}
}
