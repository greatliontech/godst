// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing/simulation"
	"time"
)

func init() {
	register("DSTWedgeSpinCallFree", DSTWedgeSpinCallFree)
	register("DSTWedgeParkLoop", DSTWedgeParkLoop)
}

// DSTWedgeSpinCallFree wedges a run with a CALL-FREE in-bubble spin loop: the
// spinner never blocks, yields, or calls into the scheduler, so with P=1 and
// preemption disabled nothing else can ever run — the decision-free hang mode
// only the wedge detector's sysmon wall arm can see. The run must die with the
// loud DST-WEDGE fatal error naming the goroutine, not hang until an external
// kill. The wall bound is shrunk so the test is fast.
func DSTWedgeSpinCallFree() {
	simulation.RunWith(1, simulation.Options{WedgeWallLimit: 500 * time.Millisecond}, func() {
		var flag atomic.Bool
		go func() {
			for !flag.Load() {
			}
		}()
		// Durably park the main goroutine so the spinner is scheduled; the
		// flag is never set, so the spinner never exits and the sleep never
		// completes (virtual time cannot advance under the spin).
		time.Sleep(time.Hour)
		flag.Store(true)
	})
	println("unreachable")
}

// DSTWedgeParkLoop wedges a run with a NON-DURABLE park loop: two goroutines
// ping-pong a mutex with Gosched yields, so scheduler decisions keep flowing
// but the bubble never has every goroutine durably blocked — durable
// quiescence never holds, virtual time cannot advance, and the deadlock
// detector never fires. The wedge detector's quiescence arm must fail the run
// loudly at its (shrunk) decision bound.
func DSTWedgeParkLoop() {
	simulation.RunWith(1, simulation.Options{WedgeDecisionLimit: 20000}, func() {
		var mu sync.Mutex
		go func() {
			for {
				mu.Lock()
				runtime.Gosched()
				mu.Unlock()
			}
		}()
		for {
			mu.Lock()
			runtime.Gosched()
			mu.Unlock()
		}
	})
}
