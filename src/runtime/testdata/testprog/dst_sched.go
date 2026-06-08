// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"runtime"
	"runtime/dst"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Seq-5 derisk probes. Each scenario records the order in which participating
// goroutines reach a shared observation point, inside dst.Run, and prints the
// resulting interleaving signature (one digit per goroutine id). Running a
// scenario across same-seed runs characterizes determinism; across different
// seeds characterizes seed-variation (seed-invariant == the schedule explores a
// single interleaving == the gap Seq 5 closes). DSTSCENARIO selects the scenario.
func init() {
	register("DSTSchedScenario", DSTSchedScenario)
}

// schedRecorder is a race-free interleaving recorder: each goroutine claims a
// distinct slot via an atomic counter, so concurrent writes never overlap.
type schedRecorder struct {
	seq []int32
	idx int64
}

func newSchedRecorder(n int) *schedRecorder { return &schedRecorder{seq: make([]int32, n)} }

func (r *schedRecorder) record(id int) {
	i := atomic.AddInt64(&r.idx, 1) - 1
	r.seq[i] = int32(id)
}

func (r *schedRecorder) String() string {
	b := make([]byte, 0, len(r.seq))
	for _, v := range r.seq[:atomic.LoadInt64(&r.idx)] {
		b = append(b, byte('0'+v))
	}
	return string(b)
}

func DSTSchedScenario() {
	seed, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	scenario := os.Getenv("DSTSCENARIO")
	// DSTPCT=<depth> selects the PCT strategy with the given bug depth; unset/0
	// uses the default random strategy.
	pctDepth, _ := strconv.Atoi(os.Getenv("DSTPCT"))
	pctSteps, _ := strconv.Atoi(os.Getenv("DSTPCTSTEPS"))
	run := dst.Run
	if pctDepth > 0 {
		run = func(s uint64, f func()) {
			dst.RunWith(s, dst.Options{Strategy: dst.PCT, Depth: pctDepth, Steps: pctSteps}, f)
		}
	}
	var sig string
	run(seed, func() {
		switch scenario {
		case "gosched":
			sig = schedGosched()
		case "spawn":
			sig = schedSpawn()
		case "mutex":
			sig = schedMutex()
		case "broadcast":
			sig = schedBroadcast()
		case "chanring":
			sig = schedChanRing()
		case "mutexcount":
			sig = schedMutexCount()
		default:
			sig = "UNKNOWN_SCENARIO"
		}
	})
	os.Stdout.WriteString(sig + "\n")
}

// schedGosched: G goroutines each run K rounds of (record, Gosched). After the
// first round every goroutine reaches the P through the global run queue
// (goschedImpl -> globrunqput), so this exercises the get-side selection point.
func schedGosched() string {
	const G, K = 5, 8
	r := newSchedRecorder(G * K)
	var wg sync.WaitGroup
	wg.Add(G)
	for g := 0; g < G; g++ {
		go func(id int) {
			defer wg.Done()
			for k := 0; k < K; k++ {
				r.record(id)
				runtime.Gosched()
			}
		}(g)
	}
	wg.Wait()
	return r.String()
}

// schedSpawn: R rounds, each spawning G goroutines that record once and exit.
// Freshly created goroutines reach the P through newproc -> runqput (runnext +
// local ring), so this exercises the put-side selection point (the same hooks
// randomizeScheduler sits on).
func schedSpawn() string {
	const R, G = 5, 6
	r := newSchedRecorder(R * G)
	for round := 0; round < R; round++ {
		var wg sync.WaitGroup
		wg.Add(G)
		for g := 0; g < G; g++ {
			go func(id int) {
				defer wg.Done()
				r.record(id)
			}(g)
		}
		wg.Wait()
	}
	return r.String()
}

// schedMutex: G goroutines each acquire a shared sync.Mutex K times, recording
// id inside the critical section. Mutex handoff goes through the sema path
// (semrelease -> goready); the acquisition order characterizes whether that path
// is seed-varied.
func schedMutex() string {
	const G, K = 5, 6
	r := newSchedRecorder(G * K)
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(G)
	for g := 0; g < G; g++ {
		go func(id int) {
			defer wg.Done()
			for k := 0; k < K; k++ {
				mu.Lock()
				r.record(id)
				mu.Unlock()
				runtime.Gosched()
			}
		}(g)
	}
	wg.Wait()
	return r.String()
}

// schedBroadcast: R rounds; each round G goroutines block on a shared channel
// and main close()s it, waking all G at once (goready fan-out). The order they
// then run characterizes the ordering among simultaneously-readied goroutines.
// The barrier is synctest durability: main's time.Sleep advances virtual time
// only when every bubble goroutine is durably blocked, so on return all G are
// guaranteed parked on <-start (gap-free, unlike an atomic-counter spin, which
// cannot detect the instant a goroutine actually parks).
func schedBroadcast() string {
	const R, G = 5, 6
	r := newSchedRecorder(R * G)
	for round := 0; round < R; round++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(G)
		for g := 0; g < G; g++ {
			go func(id int) {
				defer wg.Done()
				<-start
				r.record(id)
			}(g)
		}
		time.Sleep(time.Millisecond) // durable barrier: all G now parked on <-start
		close(start)
		wg.Wait()
	}
	return r.String()
}

// schedMutexCount is the soundness probe: G goroutines each do K rounds of
// (Lock; non-atomic read-modify-write of a shared counter; Unlock; Gosched).
// Mutual exclusion is provided only by the mutex — a goroutine blocked on Lock
// is *not* runnable, so a sound seam (which selects only among runnable
// goroutines) can never run it inside another's critical section, and the final
// count must equal G*K exactly. If the seam ever ran a blocked goroutine or
// corrupted the runq so two goroutines interleaved in the critical section, the
// non-atomic increment would lose updates and the count would be < G*K. Prints
// "ok" iff the count is exact, else "BAD <count>". The result must be "ok" for
// every seed.
func schedMutexCount() string {
	const G, K = 5, 40
	var mu sync.Mutex
	shared := 0
	var wg sync.WaitGroup
	wg.Add(G)
	for g := 0; g < G; g++ {
		go func() {
			defer wg.Done()
			for k := 0; k < K; k++ {
				mu.Lock()
				tmp := shared
				runtime.Gosched() // widen the window an unsound seam could exploit
				shared = tmp + 1
				mu.Unlock()
				runtime.Gosched()
			}
		}()
	}
	wg.Wait()
	if shared == G*K {
		return "ok"
	}
	return "BAD " + strconv.Itoa(shared)
}

// schedChanRing: a ring of G goroutines passing one token in a fixed channel
// topology. The token's path through the ring is fixed by channel happens-before
// (0->1->...->G-1->0 each lap), so the *values* are causally ordered. The record
// signature is a weaker observable — after each unbuffered rendezvous both sender
// and receiver are runnable, which is itself a sound scheduling choice, so this
// characterizes a channel-driven schedule rather than serving as a strict
// soundness control. (The soundness test asserts value-ordering invariance, not
// record order.)
func schedChanRing() string {
	const G, Laps = 6, 5
	r := newSchedRecorder(G * Laps)
	chans := make([]chan int, G)
	for i := range chans {
		chans[i] = make(chan int)
	}
	var wg sync.WaitGroup
	wg.Add(G)
	for g := 0; g < G; g++ {
		go func(id int) {
			defer wg.Done()
			in := chans[id]
			out := chans[(id+1)%G]
			for lap := 0; lap < Laps; lap++ {
				if id == 0 {
					out <- lap // inject token for this lap
					r.record(id)
					<-in // drain it after a full circulation
				} else {
					v := <-in
					r.record(id)
					out <- v
				}
			}
		}(g)
	}
	wg.Wait()
	return r.String()
}
