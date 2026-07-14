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
// switch is measured on the bubble's virtual clock, not the wall clock —
// mutex waits are not durably blocking, so virtual time cannot pass while a
// waiter is pending and in-bubble handoff order is a pure function of the
// seed. On the wall clock the switch fires once a waiter has waited 1ms,
// which under this program's ~300us critical sections happens at a
// wall-jittered iteration: the waiter's acquisition slot then varies between
// identical same-seed runs (demonstrated 19-vs-1 over 20 runs pre-fix), and
// with load or GC pauses any contended mutex in any SUT is exposed. The pin
// asserts both cross-run equality and the deterministic outcome itself: the
// barging holder is never preempted by a wall-clock starvation handoff, so
// the waiter acquires only after the holder's final unlock.
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
