// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// DST Level-2 test fixtures: the access-granularity yield substrate and the
// systematic interleaving explorer (simulation.Explore, Exhaustive + DPOR). These
// are driven by dst_test.go's harness (which shells out to a -tags=dst build), so
// they must run under the testing/simulation API rather than calling the runtime
// white-box. See docs/dst/design.md "Level 2 — access-granularity interleaving +
// DPOR".

package main

import (
	"os"
	"sort"
	"strconv"
	"sync"
	"testing/simulation"
	"unsafe" // for go:linkname and access-yield addresses
)

func init() {
	register("DSTYieldSound", DSTYieldSound)
	register("DSTExplore", DSTExplore)
	register("DSTExploreOutcomes", DSTExploreOutcomes)
}

// dstYieldPoint is a cooperative yield with no recorded access; dstAccessYield
// records a pending memory access (addr, write) for DPOR's dependency relation
// then yields. Both are the Level-2 access-granularity transition boundary,
// placed manually here (the dst-race compiler mode will insert them automatically
// at every shared access).
//
//go:linkname dstYieldPoint runtime.dstYieldPoint
func dstYieldPoint()

//go:linkname dstAccessYield runtime.dstAccessYield
func dstAccessYield(addr unsafe.Pointer, write bool)

//go:linkname dstAccessYieldFP runtime.dstAccessYieldFP
func dstAccessYieldFP() uint64

//go:linkname dstAccessYieldReset runtime.dstAccessYieldReset
func dstAccessYieldReset()

func dstSeedEnv() uint64 {
	s, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	return s
}

// DSTYieldSound is the access-granularity soundness probe (DST-L2-1). G goroutines
// each do K rounds of (Lock; read; YIELD-while-holding-the-lock; write; Unlock;
// YIELD). The mid-critical-section yield is the load-bearing case: it yields while
// a USER mutex is held (sync.Mutex does not bump m.locks, so the guard permits it).
// A sound seam never runs a goroutine blocked on Lock, so mutual exclusion holds
// and the non-atomic counter must reach exactly G*K; if yield-at-access ran a
// blocked G inside the critical section, updates would be lost and the count would
// be < G*K. Prints "ok <count> yields=<n>" iff exact, else "BAD <count>". Replayed
// across same-seed runs it is identical (determinism, DST-L2-2); yields is the
// per-run yield magnitude.
func DSTYieldSound() {
	const G, K = 5, 40
	count := 0
	var yields uint64
	simulation.Run(dstSeedEnv(), func() {
		dstAccessYieldReset()
		var mu sync.Mutex
		var wg sync.WaitGroup
		wg.Add(G)
		for g := 0; g < G; g++ {
			go func() {
				defer wg.Done()
				for k := 0; k < K; k++ {
					mu.Lock()
					tmp := count
					dstYieldPoint() // yield while holding the lock (must stay sound)
					count = tmp + 1
					mu.Unlock()
					dstYieldPoint() // yield outside the lock
				}
			}()
		}
		wg.Wait()
		yields = dstAccessYieldFP()
	})
	if count == G*K {
		os.Stdout.WriteString("ok " + strconv.Itoa(count) + " yields=" + strconv.FormatUint(yields, 10) + "\n")
	} else {
		os.Stdout.WriteString("BAD " + strconv.Itoa(count) + "\n")
	}
}

// atomicityViolSUT is an Explore SUT: two withdrawals of 100 from a balance of
// 100, each a check-then-act with the lock released between read and write.
// Returns true iff the lost update manifested (final balance != -100). The
// violating interleaving — one withdrawal reads the stale balance between the
// other's two critical sections — needs a switch in the gap, which coarse DST
// never yields at (random/PCT find it in 0/200 seeds). Under Explore that
// interleaving is in the tree, so it is found deterministically for any seed.
func atomicityViolSUT() bool {
	var mu sync.Mutex
	balance := 100
	var wg sync.WaitGroup
	wg.Add(2)
	withdraw := func() {
		defer wg.Done()
		dstAccessYield(unsafe.Pointer(&balance), false) // next transition: read balance
		mu.Lock()
		b := balance
		mu.Unlock()
		dstAccessYield(unsafe.Pointer(&balance), true) // next transition: write balance
		mu.Lock()
		balance = b - 100
		mu.Unlock()
	}
	go withdraw()
	go withdraw()
	wg.Wait()
	return balance != -100
}

// mutexCountSUT is the soundness SUT: G goroutines each do K mutex-protected
// non-atomic increments of a shared counter, with an access-yield before each
// critical section. A sound explorer never runs a goroutine blocked on Lock, so
// EVERY interleaving reaches exactly G*K; returns true (bug) only if mutual
// exclusion was violated. Explore must report ZERO failures over the whole
// interleaving space (DST-L2-1 at exploration scope).
func mutexCountSUT() bool {
	const G, K = 2, 2
	var mu sync.Mutex
	count := 0
	var wg sync.WaitGroup
	wg.Add(G)
	for i := 0; i < G; i++ {
		go func() {
			defer wg.Done()
			for k := 0; k < K; k++ {
				dstAccessYield(unsafe.Pointer(&count), true) // next transition: read+write count under the lock
				mu.Lock()
				t := count
				count = t + 1
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return count != G*K
}

// DSTExplore runs simulation.Explore on a named SUT (DSTEXPLORE) under a mode
// (DSTMODE=dpor|exhaustive) and prints "schedules=<n> failures=<m>
// exhausted=<bool> overflow=<bool>" plus the first failing schedule.
func DSTExplore() {
	var sut func() bool
	switch os.Getenv("DSTEXPLORE") {
	case "atomicity":
		sut = atomicityViolSUT
	case "mutexcount":
		sut = mutexCountSUT
	default:
		os.Stdout.WriteString("UNKNOWN_SUT\n")
		return
	}
	mode := simulation.Exhaustive
	if os.Getenv("DSTMODE") == "dpor" {
		mode = simulation.DPOR
	}
	res := simulation.Explore(dstSeedEnv(), mode, sut)
	os.Stdout.WriteString("schedules=" + strconv.Itoa(res.Schedules) +
		" failures=" + strconv.Itoa(len(res.Failures)) +
		" exhausted=" + strconv.FormatBool(res.Exhausted) +
		" overflow=" + strconv.FormatBool(res.Overflow) + "\n")
	if len(res.Failures) > 0 {
		s := "first-failing-schedule=["
		for i, g := range res.Failures[0].Schedule {
			if i > 0 {
				s += " "
			}
			s += strconv.FormatUint(g, 10)
		}
		os.Stdout.WriteString(s + "]\n")
	}
}

// dstOutcomes collects the distinct final values multiOutcomeSUT produces across
// every interleaving Explore visits — the completeness oracle: DPOR's outcome set
// must equal Exhaustive's, or DPOR is silently missing reachable states.
var dstOutcomes = map[int]bool{}

// multiOutcomeSUT: G goroutines each do an unsynchronized read-modify-write of a
// shared counter, with access-yields at the read and write, so the final counter
// lands on several distinct values depending on the interleaving (lost updates).
// It records the final value into dstOutcomes. Exhaustive enumerates every
// reachable final value; a COMPLETE DPOR must reach the identical set (DST-L2-3).
func multiOutcomeSUT() bool {
	const G = 2
	counter := 0
	var wg sync.WaitGroup
	wg.Add(G)
	for i := 0; i < G; i++ {
		go func() {
			defer wg.Done()
			dstAccessYield(unsafe.Pointer(&counter), false)
			t := counter
			dstAccessYield(unsafe.Pointer(&counter), true)
			counter = t + 1
		}()
	}
	wg.Wait()
	dstOutcomes[counter] = true
	return false
}

// DSTExploreOutcomes runs multiOutcomeSUT under Explore (DSTMODE) and prints the
// sorted set of distinct final values reached: "schedules=<n> outcomes=[...]".
// Exhaustive and DPOR must print the IDENTICAL outcome set (DST-L2-3 completeness).
func DSTExploreOutcomes() {
	mode := simulation.Exhaustive
	if os.Getenv("DSTMODE") == "dpor" {
		mode = simulation.DPOR
	}
	for k := range dstOutcomes {
		delete(dstOutcomes, k)
	}
	res := simulation.Explore(dstSeedEnv(), mode, multiOutcomeSUT)
	vals := make([]int, 0, len(dstOutcomes))
	for v := range dstOutcomes {
		vals = append(vals, v)
	}
	sort.Ints(vals)
	s := "schedules=" + strconv.Itoa(res.Schedules) + " outcomes=["
	for i, v := range vals {
		if i > 0 {
			s += " "
		}
		s += strconv.Itoa(v)
	}
	os.Stdout.WriteString(s + "]\n")
}
