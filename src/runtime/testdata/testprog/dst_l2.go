// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// DST Level-2 test fixtures: the access-granularity yield substrate and the
// systematic interleaving explorer (simulation.Explore, Exhaustive + DPOR). These
// are driven by dst_test.go's harness (which shells out to a -tags=dst build), so
// they must run under the testing/simulation API rather than calling the runtime
// white-box. See docs/dst/exploration.md "Level 2 — access-granularity interleaving +
// DPOR".

package main

import (
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing/simulation"
	"time"
	"unsafe" // for go:linkname and access-yield addresses
)

func init() {
	register("DSTYieldSound", DSTYieldSound)
	register("DSTExplore", DSTExplore)
	register("DSTExploreOutcomes", DSTExploreOutcomes)
	register("DSTExploreSweep", DSTExploreSweep)
	register("DSTExploreRaceOracle", DSTExploreRaceOracle)
	register("DSTExploreRaceReplay", DSTExploreRaceReplay)
	register("DSTExploreBudgetPromotion", DSTExploreBudgetPromotion)
	register("DSTExploreAuto", DSTExploreAuto)
	register("DSTExploreSyncAuto", DSTExploreSyncAuto)
	register("DSTExploreAtomicAuto", DSTExploreAtomicAuto)
	register("DSTExploreTimerHB", DSTExploreTimerHB)
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

// dstAtomicYield announces a sync/atomic operation as a decision transition
// (kind: 0 load, 1 store, 2 RMW, 3 CAS — mirroring runtime's dstAtomic*
// constants) and records its conservative happens-before contribution. Placed
// manually here for the brain-validation sweep; the dst-race compiler mode
// emits it automatically before sync/atomic calls in instrumented code.
//
//go:linkname dstAtomicYield runtime.dstAtomicYield
func dstAtomicYield(addr unsafe.Pointer, size uintptr, kind uintptr)

// dstSyncAcquire announces a synchronization-object decision as a write-conflict on
// the object's identity and yields BEFORE the state decision/transition, so that
// decision order is a DPOR transition. Placed manually here; the dst-race compiler/
// runtime phase wires real sync primitives to it automatically.
//
//go:linkname dstSyncAcquire runtime.dstSyncAcquire
func dstSyncAcquire(id unsafe.Pointer)

//go:linkname dstFilterForceConservativeTP runtime.dstFilterForceConservativeFP
func dstFilterForceConservativeTP(on bool)

//go:linkname dstAccessYieldFP runtime.dstAccessYieldFP
func dstAccessYieldFP() uint64

//go:linkname dstAccessYieldReset runtime.dstAccessYieldReset
func dstAccessYieldReset()

//go:linkname dstSetPostGoYield runtime.dstSetPostGoYield
func dstSetPostGoYield(enabled bool) bool

func dstSeedEnv() uint64 {
	s, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	return s
}

// yieldLockedSUT is the access-granularity SOUNDNESS SUT (DST-L2-1): G goroutines do
// K mutex-protected non-atomic increments, with an access-yield WHILE the lock is held
// — the load-bearing case (sync.Mutex does not bump m.locks, so the safe-point guard
// permits yielding inside a user critical section). A sound seam never runs a
// goroutine blocked on Lock, so mutual exclusion holds and the counter reaches exactly
// G*K on EVERY interleaving; returns true (bug) only on a lost update. dstSyncAcquire
// makes the lock-acquisition order a transition so Explore visits both orders (else
// the space would be a single schedule and the test vacuous).
func yieldLockedSUT() bool {
	const G, K = 2, 2
	var mu sync.Mutex
	count := 0
	var wg sync.WaitGroup
	wg.Add(G)
	for i := 0; i < G; i++ {
		go func() {
			defer wg.Done()
			for k := 0; k < K; k++ {
				dstSyncAcquire(unsafe.Pointer(&mu))
				mu.Lock()
				t := count
				dstAccessYield(unsafe.Pointer(&count), true) // yield WHILE holding the lock
				count = t + 1
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return count != G*K
}

// DSTYieldSound EXPLORES yieldLockedSUT (scheduled strategy) and prints "yieldsound
// schedules=<n> failures=<f> exhausted=<bool>". Access-granularity yielding is a
// scheduled-strategy mechanism (inert under Random/PCT, where a plain simulation.Run
// would land), so the soundness of yield-while-holding-a-lock must be checked under
// Explore: failures==0 means a blocked goroutine was never run inside a critical
// section (DST-L2-1); schedules>1 confirms the yield actually drove interleavings (not
// a vacuous pass).
func DSTYieldSound() {
	res := simulation.Explore(dstSeedEnv(), simulation.DPOR, yieldLockedSUT)
	os.Stdout.WriteString("yieldsound schedules=" + strconv.Itoa(res.Schedules) +
		" failures=" + strconv.Itoa(len(res.Failures)) +
		" exhausted=" + strconv.FormatBool(res.Exhausted) +
		" budgethit=" + strconv.FormatBool(res.BudgetHit) +
		" overflow=" + strconv.FormatBool(res.Overflow) +
		" uninstrumented=" + strconv.FormatBool(res.Uninstrumented) + "\n")
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

// twoPairSUT runs two independent producer/consumer pairs concurrently. Within
// each pair the shared access (producer writes xN, consumer reads xN after the
// channel handoff) is happens-before-ordered, so it is NOT a race; the two pairs
// touch different addresses, so there is no cross-pair conflict. There is genuine
// scheduling freedom (the pairs interleave), but no two CONCURRENT conflicting
// accesses exist. A DPOR with happens-before pruning recognizes this and explores
// a minimal number of schedules; the address-only relation treats each pair's
// ordered write/read as a dependency and over-explores it. Returns true (bug) only
// if a consumer fails to see its producer's value — which never happens.
func twoPairSUT() bool {
	var x1, x2 int
	ch1 := make(chan int)
	ch2 := make(chan int)
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); dstAccessYield(unsafe.Pointer(&x1), true); x1 = 1; ch1 <- 1 }()
	go func() { defer wg.Done(); <-ch1; dstAccessYield(unsafe.Pointer(&x1), false); _ = x1 }()
	go func() { defer wg.Done(); dstAccessYield(unsafe.Pointer(&x2), true); x2 = 1; ch2 <- 1 }()
	go func() { defer wg.Done(); <-ch2; dstAccessYield(unsafe.Pointer(&x2), false); _ = x2 }()
	wg.Wait()
	return x1 != 1 || x2 != 1
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
	case "twopair":
		sut = twoPairSUT
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

// raceOracleSUT has two goroutines write a shared int with NO synchronization — an
// unconditional data race the -race detector (D5 oracle) must report. There is no SUT
// assertion (it returns false), so the ONLY failure signal is the race: it proves
// Explore surfaces data races, not just assertion failures. The access-yields make
// the two writes interleavable DPOR transitions.
func raceOracleSUT() bool {
	var x int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); dstAccessYield(unsafe.Pointer(&x), true); x = 1 }()
	go func() { defer wg.Done(); dstAccessYield(unsafe.Pointer(&x), true); x = 2 }()
	wg.Wait()
	return false
}

// raceCondSUT has an INTERLEAVING-CONDITIONAL data race: the race manifests only on
// the schedule where the reader acquires the mutex first. The writer sets x then a
// done flag under the lock; the reader reads done under the lock and, only if the
// writer has not run yet (done==0, i.e. the reader acquired first), reads x OUTSIDE
// any lock — racing the writer's later x=1. When the writer acquires first the reader
// sees done==1 and never touches x: no race. So a coarse scheduler that happens to run
// the writer first misses the race entirely; the explorer, which explores both mutex
// acquisition orders (dstSyncAcquire on the mutex — the sync-decision machinery),
// reaches the reader-first schedule and the -race oracle reports it. No
// SUT assertion: the race is the finding.
func raceCondSUT() bool {
	var x, done int
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		dstSyncAcquire(unsafe.Pointer(&mu))
		mu.Lock()
		x = 1
		done = 1
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		dstSyncAcquire(unsafe.Pointer(&mu))
		mu.Lock()
		d := done
		mu.Unlock()
		if d == 0 {
			dstAccessYield(unsafe.Pointer(&x), false)
			_ = x // unsynchronized read, reached only when the reader acquired first
		}
	}()
	wg.Wait()
	return false
}

// raceMultiSUT has two independent unsynchronized write-write races. They manifest
// in the same explored schedule, so Explore must append one Race failure for each
// new RaceErrors increment, not collapse the whole schedule to one race failure.
func raceMultiSUT() bool {
	var x, y int
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); x = 1 }()
	go func() { defer wg.Done(); x = 2 }()
	go func() { defer wg.Done(); y = 1 }()
	go func() { defer wg.Done(); y = 2 }()
	wg.Wait()
	_, _ = x, y
	return false
}

// DSTExploreRaceOracle runs a race SUT (DSTRACE=uncond|cond, default uncond) under
// Explore and prints "raceoracle schedules=<n> races=<m> exhausted=<bool>
// firstrace=[g,g,...]". races counts the Failures the -race oracle flagged
// (Failure.Race); firstrace is the schedule that reproduces the first one
// (comma-separated, no spaces, so it is a single output token). Meaningful only in a
// -race build; in a non-race build races=0.
func DSTExploreRaceOracle() {
	sut := raceOracleSUT
	switch os.Getenv("DSTRACE") {
	case "cond":
		sut = raceCondSUT
	case "multi":
		sut = raceMultiSUT
	}
	res := simulation.Explore(dstSeedEnv(), simulation.DPOR, sut)
	races := 0
	firstRace := "[]"
	for _, f := range res.Failures {
		if !f.Race {
			continue
		}
		races++
		if races == 1 {
			s := "["
			for i, g := range f.Schedule {
				if i > 0 {
					s += ","
				}
				s += strconv.FormatUint(g, 10)
			}
			firstRace = s + "]"
		}
	}
	os.Stdout.WriteString("raceoracle schedules=" + strconv.Itoa(res.Schedules) +
		" races=" + strconv.Itoa(races) +
		" exhausted=" + strconv.FormatBool(res.Exhausted) +
		" firstrace=" + firstRace + "\n")
}

func encodeSchedule(schedule []uint64) string {
	if len(schedule) == 0 {
		return "_"
	}
	parts := make([]string, len(schedule))
	for i, g := range schedule {
		parts[i] = strconv.FormatUint(g, 10)
	}
	return strings.Join(parts, ",")
}

func decodeSchedule(s string) []uint64 {
	if s == "" || s == "_" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]uint64, len(parts))
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			panic(err)
		}
		out[i] = v
	}
	return out
}

func encodeAccessForces(forces []simulation.AccessForce) string {
	if len(forces) == 0 {
		return "_"
	}
	parts := make([]string, len(forces))
	for i, f := range forces {
		parts[i] = strconv.FormatUint(f.Seq, 10) + ":" + strconv.FormatUint(f.Count, 10) + ":" + strconv.FormatUint(uint64(f.PCKey), 16)
	}
	return strings.Join(parts, ",")
}

func decodeAccessForces(s string) []simulation.AccessForce {
	if s == "" || s == "_" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]simulation.AccessForce, len(parts))
	for i, p := range parts {
		fields := strings.Split(p, ":")
		if len(fields) != 3 {
			panic("bad force token: " + p)
		}
		seq, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			panic(err)
		}
		count, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			panic(err)
		}
		pc, err := strconv.ParseUint(fields[2], 16, 0)
		if err != nil {
			panic(err)
		}
		out[i] = simulation.AccessForce{Seq: seq, Count: count, PCKey: uintptr(pc)}
	}
	return out
}

func firstRaceFailure(res simulation.ExploreResult) (simulation.Failure, bool) {
	var first simulation.Failure
	found := false
	for _, f := range res.Failures {
		if !f.Race {
			continue
		}
		if len(f.AccessForces) != 0 {
			return f, true
		}
		if !found {
			first, found = f, true
		}
	}
	return first, found
}

// DSTExploreRaceReplay prints a race failure's full replay token, or replays a token
// in a fresh process. It uses the auto-instrumented R/W/R SUT because its filtered
// access stream exercises replay-promoted access forces.
func DSTExploreRaceReplay() {
	seed := dstSeedEnv()
	if os.Getenv("DSTREPLAY") == "1" {
		failure := simulation.Failure{
			Schedule:     decodeSchedule(os.Getenv("DSTSCHEDULE")),
			AccessForces: decodeAccessForces(os.Getenv("DSTFORCES")),
		}
		_, raced := simulation.Replay(seed, failure, unmodifiedRWRSUT)
		os.Stdout.WriteString("racereplay raced=" + strconv.FormatBool(raced) + "\n")
		return
	}

	failure, ok := firstRaceFailure(simulation.Explore(seed, simulation.DPOR, unmodifiedRWRSUT))
	if !ok {
		os.Stdout.WriteString("racereplay races=0 schedule=_ forces=_ forcecount=0\n")
		return
	}
	os.Stdout.WriteString("racereplay races=1 schedule=" + encodeSchedule(failure.Schedule) +
		" forces=" + encodeAccessForces(failure.AccessForces) +
		" forcecount=" + strconv.Itoa(len(failure.AccessForces)) + "\n")
}

var budgetPromotionRuns int

// DSTExploreBudgetPromotion verifies MaxSchedules is a public ExploreWith-call
// budget, not a per-access-force-promotion-pass budget. unmodifiedRWRSUT exercises
// replay-promoted access forces under -tags dst -race; with MaxSchedules=1, Explore
// must stop after the first actual SUT run even if that run discovers a promotion.
func DSTExploreBudgetPromotion() {
	budgetPromotionRuns = 0
	res := simulation.ExploreWith(dstSeedEnv(), simulation.ExploreOptions{Mode: simulation.DPOR, MaxSchedules: 1}, func() bool {
		budgetPromotionRuns++
		return unmodifiedRWRSUT()
	})
	os.Stdout.WriteString("budgetpromotion schedules=" + strconv.Itoa(res.Schedules) +
		" runs=" + strconv.Itoa(budgetPromotionRuns) +
		" budget=" + strconv.FormatBool(res.BudgetHit) +
		" exhausted=" + strconv.FormatBool(res.Exhausted) + "\n")
}

// unmodifiedRMWSUT is a plain, UNINSTRUMENTED SUT: two goroutines do an
// unsynchronized read-modify-write of a shared counter. It carries NO manual
// dstAccessYield/dstSyncAcquire. Under -tags dst -race the compiler auto-inserts a
// yield before each memory-access race hook (increment 1), so the explorer can
// interleave the reads and writes and reach the lost update (final counter == 1, not
// 2). On a coarse scheduler the two RMWs run to completion uninterrupted (final == 2)
// and the bug is invisible. This is the end-to-end proof that compiler
// auto-instrumentation feeds the explorer with no hand-annotation — the lost update
// surfaces as a non-race SUT-assertion failure. (The unsynchronized access is also a
// data race the -race oracle reports, as a separate Race failure.) Returns true on
// the lost update.
var autoOutcomes = map[int]bool{}

func unmodifiedRMWSUT() bool {
	c := 0
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			t := c
			c = t + 1
		}()
	}
	wg.Wait()
	autoOutcomes[c] = true
	return c != 2
}

var autoNoiseOutcomes = map[int]bool{}

func unmodifiedPrivateNoiseRMWSUT() bool {
	c := 0
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		id := i
		go func() {
			defer wg.Done()
			private := make([]int, 8)
			for k := range private {
				private[k] = id + k
				private[k]++
			}
			t := c
			for k := range private {
				private[k] += t
			}
			c = t + 1
		}()
	}
	wg.Wait()
	autoNoiseOutcomes[c] = true
	return c != 2
}

var autoRWROutcomes = map[string]bool{}

func unmodifiedRWRSUT() bool {
	x := 0
	read := [2]int{-1, -1}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		read[0] = x
	}()
	go func() {
		defer wg.Done()
		x = 1
	}()
	go func() {
		defer wg.Done()
		read[1] = x
	}()
	wg.Wait()
	autoRWROutcomes[strconv.Itoa(read[0])+","+strconv.Itoa(read[1])] = true
	return false
}

type autoRangePair struct {
	A int
	B int
}

var autoRangeFieldOutcomes = map[string]bool{}

func unmodifiedRangeFieldSUT() bool {
	var p autoRangePair
	read := -1
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		p = autoRangePair{A: 1, B: 1}
	}()
	go func() {
		defer wg.Done()
		read = p.B
	}()
	wg.Wait()
	autoRangeFieldOutcomes[strconv.Itoa(read)] = true
	return false
}

var filteredManualRWROutcomes = map[string]bool{}
var filteredManualRWRX int
var filteredManualRWRRead [2]int
var filteredManualRWRWG sync.WaitGroup

//go:norace
func filteredManualRWRRead0() {
	defer filteredManualRWRWG.Done()
	dstAccessYield(unsafe.Pointer(&filteredManualRWRX), false)
	filteredManualRWRRead[0] = filteredManualRWRX
}

//go:norace
func filteredManualRWRWrite() {
	defer filteredManualRWRWG.Done()
	dstAccessYield(unsafe.Pointer(&filteredManualRWRX), true)
	filteredManualRWRX = 1
}

//go:norace
func filteredManualRWRRead1() {
	defer filteredManualRWRWG.Done()
	dstAccessYield(unsafe.Pointer(&filteredManualRWRX), false)
	filteredManualRWRRead[1] = filteredManualRWRX
}

func filteredManualRWRSUT() bool {
	filteredManualRWRX = 0
	filteredManualRWRRead = [2]int{-1, -1}
	filteredManualRWRWG.Add(3)
	go filteredManualRWRRead0()
	go filteredManualRWRWrite()
	go filteredManualRWRRead1()
	filteredManualRWRWG.Wait()
	filteredManualRWROutcomes[strconv.Itoa(filteredManualRWRRead[0])+","+strconv.Itoa(filteredManualRWRRead[1])] = true
	return false
}

var autoCreateOutcomes = map[int]bool{}
var autoWakeOutcomes = map[int]bool{}

func unmodifiedCreateThenWriteSUT() bool {
	x := 0
	read := -1
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		read = x
	}()
	x = 1
	wg.Wait()
	autoCreateOutcomes[read] = true
	return false
}

func unmodifiedWakeThenWriteSUT() bool {
	x := 0
	read := -1
	ch := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ch
		read = x
	}()
	close(ch)
	x = 1
	wg.Wait()
	autoWakeOutcomes[read] = true
	return false
}

// DSTExploreAuto runs unmodifiedRMWSUT under BOTH Exhaustive and DPOR and prints
// "auto exh=<n> dpor=<m> outcomes=<k> assertfail=<a> racefail=<r> complete=<bool>".
// It also runs a private-noise RMW and prints "noiseExh/noiseDpor/noiseOutcomes/
// noiseComplete". It is the increment-1 acceptance and a guard for filtered
// access replay promotion:
//   - assertfail counts non-race DPOR Failures (the lost update — reachable ONLY
//     because auto-instrumentation made the RMW accesses interleavable, with NO manual
//     hooks: the headline proof that the compiler feeds the explorer). racefail counts
//     -race-oracle Failures on the same auto-instrumented accesses.
//   - complete is DPOR's reachable-outcome set == Exhaustive's. unmodifiedRMWSUT's
//     dense auto-instrumentation exercises filtered inline accesses that may be
//     promoted to replay yield points — a case the manual-hook family sweep does not
//     reach.
//   - rangeComplete/rangeOutcomes guard range/composite access identity: a struct
//     assignment's racewriterange interval must conflict with a scalar field read
//     inside that interval, not only with the composite base address.
//
// Meaningful only in a -tags dst -race build (else no auto-instrumentation).
func DSTExploreAuto() {
	seed := dstSeedEnv()
	runCompare := func(outcomes map[int]bool, sut func() bool) (exh, dpor simulation.ExploreResult, exhSet, dporSet map[string]bool) {
		for k := range outcomes {
			delete(outcomes, k)
		}
		dpor = simulation.Explore(seed, simulation.DPOR, sut)
		dporSet = map[string]bool{}
		for v := range outcomes {
			dporSet[strconv.Itoa(v)] = true
		}
		for k := range outcomes {
			delete(outcomes, k)
		}
		exh = simulation.Explore(seed, simulation.Exhaustive, sut)
		exhSet = map[string]bool{}
		for v := range outcomes {
			exhSet[strconv.Itoa(v)] = true
		}
		return exh, dpor, exhSet, dporSet
	}
	runStringCompare := func(outcomes map[string]bool, sut func() bool) (exh, dpor simulation.ExploreResult, exhSet, dporSet map[string]bool) {
		for k := range outcomes {
			delete(outcomes, k)
		}
		dpor = simulation.Explore(seed, simulation.DPOR, sut)
		dporSet = map[string]bool{}
		for v := range outcomes {
			dporSet[v] = true
		}
		for k := range outcomes {
			delete(outcomes, k)
		}
		exh = simulation.Explore(seed, simulation.Exhaustive, sut)
		exhSet = map[string]bool{}
		for v := range outcomes {
			exhSet[v] = true
		}
		return exh, dpor, exhSet, dporSet
	}
	// DPOR first, so its -race-oracle count (racefail) is not pre-empted by the race
	// detector's dedup during the Exhaustive pass over the same racy SUT.
	exh, dpor, exhSet, dporSet := runCompare(autoOutcomes, unmodifiedRMWSUT)
	noiseExh, noiseDpor, noiseExhSet, noiseDporSet := runCompare(autoNoiseOutcomes, unmodifiedPrivateNoiseRMWSUT)
	rwrExh, rwrDpor, rwrExhSet, rwrDporSet := runStringCompare(autoRWROutcomes, unmodifiedRWRSUT)
	rangeExh, rangeDpor, rangeExhSet, rangeDporSet := runStringCompare(autoRangeFieldOutcomes, unmodifiedRangeFieldSUT)
	manualRWRExh, manualRWRDpor, manualRWRExhSet, manualRWRDporSet := runStringCompare(filteredManualRWROutcomes, filteredManualRWRSUT)
	createExh, createDpor, createExhSet, createDporSet := runCompare(autoCreateOutcomes, unmodifiedCreateThenWriteSUT)
	wakeExh, wakeDpor, wakeExhSet, wakeDporSet := runCompare(autoWakeOutcomes, unmodifiedWakeThenWriteSUT)
	// Unfiltered cross-check leg: re-explore the primary RMW SUT with the
	// shared-address filter's YIELD GATE forced into its conservative
	// yield-everything mode. The filtered DPOR==Exhaustive equivalence runs
	// the same filter on both sides, so a filter defect that drops an outcome
	// class cancels out of it; this leg anchors the committed ground-truth
	// outcome set to an observation that bypasses the yield gate. (The
	// stack-locality classifier still applies to the access log; a classifier
	// defect can only shrink the explored set, which the ==2 assertion
	// catches rather than cancels.)
	dstFilterForceConservativeTP(true)
	for k := range autoOutcomes {
		delete(autoOutcomes, k)
	}
	unf := func() simulation.ExploreResult {
		defer dstFilterForceConservativeTP(false)
		return simulation.Explore(seed, simulation.DPOR, unmodifiedRMWSUT)
	}()
	unfOutcomes := len(autoOutcomes)
	assertfail, racefail := 0, 0
	for _, f := range dpor.Failures {
		if f.Race {
			racefail++
		} else {
			assertfail++
		}
	}
	os.Stdout.WriteString("auto exh=" + strconv.Itoa(exh.Schedules) +
		" dpor=" + strconv.Itoa(dpor.Schedules) +
		" outcomes=" + strconv.Itoa(len(exhSet)) +
		" assertfail=" + strconv.Itoa(assertfail) +
		" racefail=" + strconv.Itoa(racefail) +
		" complete=" + strconv.FormatBool(sameSet(exhSet, dporSet)) +
		" unfOutcomes=" + strconv.Itoa(unfOutcomes) +
		" unfExhausted=" + strconv.FormatBool(unf.Exhausted) +
		" noiseExh=" + strconv.Itoa(noiseExh.Schedules) +
		" noiseDpor=" + strconv.Itoa(noiseDpor.Schedules) +
		" noiseOutcomes=" + strconv.Itoa(len(noiseExhSet)) +
		" noiseComplete=" + strconv.FormatBool(sameSet(noiseExhSet, noiseDporSet)) +
		" rwrExh=" + strconv.Itoa(rwrExh.Schedules) +
		" rwrDpor=" + strconv.Itoa(rwrDpor.Schedules) +
		" rwrOutcomes=" + strconv.Itoa(len(rwrExhSet)) +
		" rwrComplete=" + strconv.FormatBool(sameSet(rwrExhSet, rwrDporSet)) +
		" rangeExh=" + strconv.Itoa(rangeExh.Schedules) +
		" rangeDpor=" + strconv.Itoa(rangeDpor.Schedules) +
		" rangeOutcomes=" + strconv.Itoa(len(rangeExhSet)) +
		" rangeComplete=" + strconv.FormatBool(sameSet(rangeExhSet, rangeDporSet)) +
		" manualRWRExh=" + strconv.Itoa(manualRWRExh.Schedules) +
		" manualRWRDpor=" + strconv.Itoa(manualRWRDpor.Schedules) +
		" manualRWROutcomes=" + strconv.Itoa(len(manualRWRExhSet)) +
		" manualRWRComplete=" + strconv.FormatBool(sameSet(manualRWRExhSet, manualRWRDporSet)) +
		" createExh=" + strconv.Itoa(createExh.Schedules) +
		" createDpor=" + strconv.Itoa(createDpor.Schedules) +
		" createOutcomes=" + strconv.Itoa(len(createExhSet)) +
		" createComplete=" + strconv.FormatBool(sameSet(createExhSet, createDporSet)) +
		" wakeExh=" + strconv.Itoa(wakeExh.Schedules) +
		" wakeDpor=" + strconv.Itoa(wakeDpor.Schedules) +
		" wakeOutcomes=" + strconv.Itoa(len(wakeExhSet)) +
		" wakeComplete=" + strconv.FormatBool(sameSet(wakeExhSet, wakeDporSet)) + "\n")
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

// --- Runtime sync-decision auto-hook acceptance (deferral 1) -------------------
//
// The memory-access compiler auto-instrumentation (increment 1) makes an
// unmodified SUT's shared READS/WRITES explorable, but NOT its lock/rendezvous/
// release/close decision order: that decision is an addr=0 transition (a goroutine
// reaching its Lock records no memory access), and the in-section accesses are
// mutex-SERIALIZED (happens-before-ordered, not a reorderable race), so DPOR keeps
// only one decision outcome while Exhaustive — which branches on which goroutine runs
// first — finds both. That is the prog#257 completeness gap (DST-L2-3). Wiring
// the runtime sync primitives to dstSyncAcquire (internal/sync.Mutex Lock/TryLock/
// Unlock, sync.RWMutex reader/writer admission and release, chan.go chansend/
// chanrecv/closechan, and selectgo's blocking and non-blocking channel cases, gated
// to a -tags dst -race build) records each sync-object decision
// as a write-conflict on the object's identity. Matching decisions on the same object
// by different goroutines are then co-enabled, concurrent, conflicting pairs DPOR
// explores BOTH ways — with NO manual annotation. The SUTs below pin the primitives
// the acceptance covers.

// syncAutoSeen accumulates the distinct observable outcomes of the unmodified
// sync-order SUT currently under Explore (reset before each Explore call; written
// once per interleaving). Package-global for the same reason as sweepSeen.
var syncAutoSeen map[string]bool

// autoMutexOrderSUT is an UNMODIFIED mutex SUT (no manual dstAccessYield /
// dstSyncAcquire): two goroutines each acquire a shared mutex and write their id to a
// shared int under it. The final value is the id of whichever acquired LAST, so it
// pins the lock-ACQUISITION order — a free scheduling choice with two reachable
// outcomes (1 and 2). The in-section write is mutex-serialized (HB-ordered, not a
// reorderable race), so ONLY the runtime mutex auto-hook (internal/sync.Mutex.Lock →
// dstSyncAcquire on the mutex identity) makes the acquisition order a DPOR
// transition; without it DPOR keeps one order (the prog#257 gap). Kept to a single
// shared write per goroutine so brute-force Exhaustive stays tractable under the
// dense -race memory auto-instrumentation (shared-address filtering is a later
// increment). Records into syncAutoSeen; always returns false.
func autoMutexOrderSUT() bool {
	var mu sync.Mutex
	last := 0
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 1; i <= 2; i++ {
		id := i
		go func() {
			defer wg.Done()
			mu.Lock()
			last = id
			mu.Unlock()
		}()
	}
	wg.Wait()
	syncAutoSeen[strconv.Itoa(last)] = true
	return false
}

// autoChanOrderSUT is an UNMODIFIED channel SUT (no manual hooks): two senders send
// distinct values to one unbuffered channel; a receiver receives twice into two
// scalars. The recorded pair is the rendezvous order — a free scheduling choice with
// two reachable outcomes ("1,2" and "2,1"). Only the runtime channel auto-hook
// (chan.go chansend/chanrecv → dstSyncAcquire on the hchan identity) makes the
// rendezvous order a DPOR transition; without it DPOR drops one order (DST-L2-3),
// exactly as for mutex acquisition. The receiver writes two scalars (no append /
// growslice) so Exhaustive stays tractable under -race memory auto-instrumentation.
// Records into syncAutoSeen; always returns false.
func autoChanOrderSUT() bool {
	ch := make(chan int)
	var first, second int
	var wg sync.WaitGroup
	wg.Add(3)
	for s := 1; s <= 2; s++ {
		s := s
		go func() {
			defer wg.Done()
			ch <- s
		}()
	}
	go func() {
		defer wg.Done()
		first = <-ch
		second = <-ch
	}()
	wg.Wait()
	syncAutoSeen[strconv.Itoa(first)+","+strconv.Itoa(second)] = true
	return false
}

// autoRWMutexRLockOrderSUT distinguishes whether a reader admits before or after a
// writer. The writer's Lock is already covered by the embedded writer mutex hook; the
// reader side must announce the same identity before readerCount admission.
func autoRWMutexRLockOrderSUT() bool {
	var rw sync.RWMutex
	x := 0
	read := -1
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rw.Lock()
		x = 1
		rw.Unlock()
	}()
	go func() {
		defer wg.Done()
		rw.RLock()
		read = x
		rw.RUnlock()
	}()
	wg.Wait()
	syncAutoSeen[strconv.Itoa(read)] = true
	return false
}

// autoTryRLockFailedDecisionSUT distinguishes a TryRLock attempt that races before a
// writer Lock from one that reaches the already-write-locked rejection path. That
// failed decision must still announce the RWMutex identity so DPOR can reverse it.
func autoTryRLockFailedDecisionSUT() bool {
	var rw sync.RWMutex
	attempted := make(chan struct{}, 1)
	success := false
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rw.Lock()
		<-attempted
		rw.Unlock()
	}()
	go func() {
		defer wg.Done()
		if rw.TryRLock() {
			success = true
			rw.RUnlock()
		}
		attempted <- struct{}{}
	}()
	wg.Wait()
	syncAutoSeen[strconv.FormatBool(success)] = true
	return false
}

// autoTryRLockReleaseDecisionSUT distinguishes a TryRLock attempt before a writer
// releases from one after it releases. The writer Unlock must record the same RWMutex
// identity as the read attempt.
func autoTryRLockReleaseDecisionSUT() bool {
	var rw sync.RWMutex
	rw.Lock()
	success := false
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rw.Unlock()
	}()
	go func() {
		defer wg.Done()
		if rw.TryRLock() {
			success = true
			rw.RUnlock()
		}
	}()
	wg.Wait()
	syncAutoSeen[strconv.FormatBool(success)] = true
	return false
}

// autoRWMutexTryLockReleaseDecisionSUT distinguishes a writer TryLock attempt before
// the last reader releases from one after it releases. RUnlock must therefore record
// the writer-mutex identity too.
func autoRWMutexTryLockReleaseDecisionSUT() bool {
	var rw sync.RWMutex
	rw.RLock()
	success := false
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rw.RUnlock()
	}()
	go func() {
		defer wg.Done()
		if rw.TryLock() {
			success = true
			rw.Unlock()
		}
	}()
	wg.Wait()
	syncAutoSeen[strconv.FormatBool(success)] = true
	return false
}

// autoTryLockOrderSUT distinguishes which goroutine wins a non-blocking TryLock on
// an initially-unlocked mutex. The winner writes only its own flag (no shared write
// conflict for DPOR to reverse) and yields while holding the lock so the other
// goroutine can observe failure. A successful TryLock is an acquisition just like
// Lock; the hook must fire before the CAS that chooses the winner.
func autoTryLockOrderSUT() bool {
	var mu sync.Mutex
	got := [2]bool{}
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		id := i
		go func() {
			defer wg.Done()
			if mu.TryLock() {
				got[id] = true
				runtime.Gosched()
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	winner := 0
	if got[0] {
		winner = 1
	} else if got[1] {
		winner = 2
	}
	syncAutoSeen[strconv.Itoa(winner)] = true
	return false
}

// autoTryLockFailedDecisionSUT distinguishes a TryLock attempt that races before a
// Lock from one that reaches the already-locked rejection path. A hook only on the
// successful CAS misses the failed-attempt outcome class.
func autoTryLockFailedDecisionSUT() bool {
	var mu sync.Mutex
	attempted := make(chan struct{}, 1)
	success := false
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		mu.Lock()
		<-attempted
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		if mu.TryLock() {
			success = true
			mu.Unlock()
		}
		attempted <- struct{}{}
	}()
	wg.Wait()
	syncAutoSeen[strconv.FormatBool(success)] = true
	return false
}

// autoTryLockReleaseDecisionSUT distinguishes a TryLock attempt before a mutex
// releases from one after it releases. Unlock must share the TryLock identity.
func autoTryLockReleaseDecisionSUT() bool {
	var mu sync.Mutex
	mu.Lock()
	success := false
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		if mu.TryLock() {
			success = true
			mu.Unlock()
		}
	}()
	wg.Wait()
	syncAutoSeen[strconv.FormatBool(success)] = true
	return false
}

// autoSelectSendOrderSUT uses a two-case non-blocking select so the compiler routes
// through selectgo, not the one-case selectnbsend helper. Both cases use the shared
// channel, so the SUT can only pass if selectgo announces that selected identity.
func autoSelectSendOrderSUT() bool {
	ch := make(chan int, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 1; i <= 2; i++ {
		id := i
		go func() {
			defer wg.Done()
			select {
			case ch <- id:
			case ch <- id:
			default:
			}
		}()
	}
	wg.Wait()
	winner := 0
	select {
	case winner = <-ch:
	default:
	}
	syncAutoSeen[strconv.Itoa(winner)] = true
	return false
}

// autoSelectBlockSendOrderSUT is the blocking selectgo send twin.
func autoSelectBlockSendOrderSUT() bool {
	ch := make(chan int)
	first, second := 0, 0
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		first = <-ch
		second = <-ch
	}()
	for i := 1; i <= 2; i++ {
		id := i
		go func() {
			defer wg.Done()
			select {
			case ch <- id:
			case ch <- id:
			}
		}()
	}
	wg.Wait()
	syncAutoSeen[strconv.Itoa(first)+","+strconv.Itoa(second)] = true
	return false
}

// autoSelectNBSendOrderSUT exercises the compiler's one-case select+default rewrite
// through selectnbsend/chansend(block=false). It is the helper-path twin of the
// selectgo SUT above.
func autoSelectNBSendOrderSUT() bool {
	ch := make(chan int, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 1; i <= 2; i++ {
		id := i
		go func() {
			defer wg.Done()
			select {
			case ch <- id:
			default:
			}
		}()
	}
	wg.Wait()
	winner := 0
	select {
	case winner = <-ch:
	default:
	}
	syncAutoSeen[strconv.Itoa(winner)] = true
	return false
}

// autoSelectRecvOrderSUT is the receive twin: one buffered value, two goroutines
// using a two-case non-blocking select on the same shared channel, and the receiving
// goroutine's id records the decision outcome.
func autoSelectRecvOrderSUT() bool {
	ch := make(chan int, 1)
	ch <- 1
	got := [2]int{}
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		idx := i
		go func() {
			defer wg.Done()
			select {
			case got[idx] = <-ch:
			case got[idx] = <-ch:
			default:
			}
		}()
	}
	wg.Wait()
	winner := 0
	if got[0] != 0 {
		winner = 1
	} else if got[1] != 0 {
		winner = 2
	}
	syncAutoSeen[strconv.Itoa(winner)] = true
	return false
}

// autoSelectBlockRecvOrderSUT is the blocking selectgo receive twin. The first value
// sent on ch identifies which receiver was queued first by its blocking select.
func autoSelectBlockRecvOrderSUT() bool {
	ch := make(chan int)
	got := [2]int{}
	var wg sync.WaitGroup
	wg.Add(3)
	for i := 0; i < 2; i++ {
		idx := i
		go func() {
			defer wg.Done()
			select {
			case got[idx] = <-ch:
			case got[idx] = <-ch:
			}
		}()
	}
	go func() {
		defer wg.Done()
		ch <- 1
		ch <- 2
	}()
	wg.Wait()
	winner := 0
	if got[0] == 1 {
		winner = 1
	} else if got[1] == 1 {
		winner = 2
	}
	syncAutoSeen[strconv.Itoa(winner)] = true
	return false
}

// autoSelectNBRecvOrderSUT exercises the compiler's one-case select+default rewrite
// through selectnbrecv/chanrecv(block=false).
func autoSelectNBRecvOrderSUT() bool {
	ch := make(chan int, 1)
	ch <- 1
	got := [2]int{}
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		idx := i
		go func() {
			defer wg.Done()
			select {
			case got[idx] = <-ch:
			default:
			}
		}()
	}
	wg.Wait()
	winner := 0
	if got[0] != 0 {
		winner = 1
	} else if got[1] != 0 {
		winner = 2
	}
	syncAutoSeen[strconv.Itoa(winner)] = true
	return false
}

// autoChanCloseRecvDecisionSUT distinguishes a non-blocking receive before close
// from one after close. closechan must announce the same channel identity as recv.
func autoChanCloseRecvDecisionSUT() bool {
	ch := make(chan int)
	outcome := "default"
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		close(ch)
	}()
	go func() {
		defer wg.Done()
		select {
		case _, ok := <-ch:
			if ok {
				outcome = "value"
			} else {
				outcome = "closed"
			}
		default:
		}
	}()
	wg.Wait()
	syncAutoSeen[outcome] = true
	return false
}

// autoOnceOrderSUT pins sync.Once's successful first execution path. Once is covered
// transitively by its internal Mutex.Lock hook; this SUT prevents that coverage from
// being accidentally lost.
func autoOnceOrderSUT() bool {
	var once sync.Once
	winner := 0
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 1; i <= 2; i++ {
		id := i
		go func() {
			defer wg.Done()
			once.Do(func() { winner = id })
		}()
	}
	wg.Wait()
	syncAutoSeen[strconv.Itoa(winner)] = true
	return false
}

// DSTExploreSyncAuto is the acceptance for runtime sync-decision auto-hooks
// (deferral 1): an UNMODIFIED SUT whose outcome depends on lock / rendezvous /
// release / close decision order, built -tags dst -race, must have DPOR reach BOTH
// decision outcomes with NO manual dstSyncAcquire. It runs the sync-decision SUTs
// under DPOR and prints, per SUT: "<name>Dpor=<n> <name>Outcomes=<k>
// <name>Exhausted=<bool>".
//
// The oracle is DPOR-only, NOT a DPOR-vs-Exhaustive comparison: under -race the
// compiler auto-instruments EVERY memory access, so brute-force Exhaustive enumerates
// the access-granularity explosion (a trivial unsynchronized RMW already hits ~19k
// schedules) and is intractable for a mutex/channel SUT until shared-address
// filtering lands. So the DPOR-vs-Exhaustive auto-completeness cross-check belongs
// with that filtering increment. Here the ground truth is known by construction —
// two symmetric goroutines contending over one sync-object decision have EXACTLY two
// outcomes — so Outcomes==2 (both outcomes reached) is the completeness signal and
// Exhausted==true confirms a clean finish. Without the runtime auto-hook the
// decision order is an addr=0 transition DPOR cannot reverse (the in-section accesses
// are object-serialized / HB-ordered, not a reorderable race), so DPOR finds only 1
// outcome — the teeth. (DPOR's completeness on sync-decision transitions, auto or
// manual, is the same relation TestDSTExploreSweep proves against Exhaustive on the
// hand-annotated family.) Meaningful only in a -tags dst -race build.
func DSTExploreSyncAuto() {
	seed := dstSeedEnv()
	check := func(name string, sut func() bool) {
		syncAutoSeen = map[string]bool{}
		dpor := simulation.Explore(seed, simulation.DPOR, sut)
		os.Stdout.WriteString(name + "Dpor=" + strconv.Itoa(dpor.Schedules) +
			" " + name + "Outcomes=" + strconv.Itoa(len(syncAutoSeen)) +
			" " + name + "Exhausted=" + strconv.FormatBool(dpor.Exhausted && !dpor.Overflow) + "\n")
	}
	os.Stdout.WriteString("syncauto\n")
	check("mutex", autoMutexOrderSUT)
	check("chan", autoChanOrderSUT)
	check("rwmutex", autoRWMutexRLockOrderSUT)
	check("tryrlockfail", autoTryRLockFailedDecisionSUT)
	check("tryrlockrelease", autoTryRLockReleaseDecisionSUT)
	check("trywlockrelease", autoRWMutexTryLockReleaseDecisionSUT)
	check("trylock", autoTryLockOrderSUT)
	check("trylockfail", autoTryLockFailedDecisionSUT)
	check("trylockrelease", autoTryLockReleaseDecisionSUT)
	check("selectsend", autoSelectSendOrderSUT)
	check("selectblocksend", autoSelectBlockSendOrderSUT)
	check("selectnbsend", autoSelectNBSendOrderSUT)
	check("selectrecv", autoSelectRecvOrderSUT)
	check("selectblockrecv", autoSelectBlockRecvOrderSUT)
	check("selectnbrecv", autoSelectNBRecvOrderSUT)
	check("chanclose", autoChanCloseRecvDecisionSUT)
	check("once", autoOnceOrderSUT)
}

// --- Generated-family equivalence validator (DST-L2-3 completeness guard) ------
//
// The micro-SUTs above are a weak net for DPOR completeness: each pins one shape.
// DSTExploreSweep generates a *family* of small concurrent programs (varying
// goroutine count, accesses, vars, and mutex synchronization) and, for every
// member, asserts that DPOR reaches the IDENTICAL set of observable outcomes as
// brute-force Exhaustive enumeration — the real DST-L2-3 guard, especially for the
// optimal-DPOR (sleep-set) work, whose failure mode is silently dropping a
// Mazurkiewicz class while still reporting Exhausted=true. See docs/dst/exploration.md
// (Level 2, increment 5, "Validator first").

// spOp is one instruction of a generated program: a read/write of shared var arg,
// a lock/unlock of mutex arg, a sync/atomic operation on atomic var arg
// ('l' Load, 's' Store, 'a' Add, 'C' CompareAndSwap — announced via
// dstAtomicYield, the manual analog of the dst-race emission), or a PLAIN
// read/write of atomic var arg ('r'/'w' — announced via dstAccessYield), so
// atomic-plain mixed dependencies are generated too.
type spOp struct {
	kind byte // 'R' read, 'W' write, 'L' lock, 'U' unlock, 'l'/'s'/'a'/'C' atomic, 'r'/'w' plain-on-atomic-var
	arg  int  // var index (R/W), mutex index (L/U), or atomic var index (the rest)
}

// spProg is a generated program: nVars shared ints, nAVars shared atomic
// int32s, nMu mutexes, and one instruction sequence per goroutine.
type spProg struct {
	nVars  int
	nAVars int
	nMu    int
	gor    [][]spOp
}

// sweepFamily deterministically enumerates the validation corpus. Each shape
// stresses a different part of the dependency/HB machinery; the standing families
// (1)-(4) + named SUTs stay within a small exhaustive budget so brute-force is
// feasible (~8s). With heavy==true it also appends the slow stress families (5)-(6)
// (~140s) — opt-in via DSTSWEEP=heavy, not run every build.
func sweepFamily(heavy bool) []spProg {
	var fam []spProg
	rw2 := []spOp{{'R', 0}, {'W', 0}, {'R', 1}, {'W', 1}} // 2 vars, read/write
	rw1 := []spOp{{'R', 0}, {'W', 0}}                     // 1 var, read/write
	// (1) 2 goroutines, 2 ops each over 2 vars, NO sync — exercises the pure
	// address dependency relation (races, lost updates, read-of-stale-write).
	for _, a := range rw2 {
		for _, b := range rw2 {
			for _, c := range rw2 {
				for _, d := range rw2 {
					fam = append(fam, spProg{nVars: 2, gor: [][]spOp{{a, b}, {c, d}}})
				}
			}
		}
	}
	// (2) 2 goroutines, 2 ops each over 1 var, each goroutine's body bracketed by a
	// SHARED mutex — exercises happens-before pruning (the in-section accesses of
	// the two goroutines are mutex-serialized, so their conflicting pairs are
	// HB-ordered and must be pruned, not explored both ways).
	for _, a := range rw1 {
		for _, b := range rw1 {
			for _, c := range rw1 {
				for _, d := range rw1 {
					g0 := []spOp{{'L', 0}, a, b, {'U', 0}}
					g1 := []spOp{{'L', 0}, c, d, {'U', 0}}
					fam = append(fam, spProg{nVars: 1, nMu: 1, gor: [][]spOp{g0, g1}})
				}
			}
		}
	}
	// (3) 3 goroutines, 1 op each over 1 var — higher concurrency, more classes
	// (write-write-write gives 3! distinguishable orderings; reads split fewer).
	for _, a := range rw1 {
		for _, b := range rw1 {
			for _, c := range rw1 {
				fam = append(fam, spProg{nVars: 1, gor: [][]spOp{{a}, {b}, {c}}})
			}
		}
	}
	// (4) 3 goroutines, each [L op U] on ONE shared mutex — multi-way (3!)
	// acquisition-order contention, the strongest stress on the sync-order
	// dependency (the 2-goroutine mutex case is degenerate).
	for _, a := range rw1 {
		for _, b := range rw1 {
			for _, c := range rw1 {
				ga := []spOp{{'L', 0}, a, {'U', 0}}
				gb := []spOp{{'L', 0}, b, {'U', 0}}
				gc := []spOp{{'L', 0}, c, {'U', 0}}
				fam = append(fam, spProg{nVars: 1, nMu: 1, gor: [][]spOp{ga, gb, gc}})
			}
		}
	}
	at1 := []spOp{{'l', 0}, {'s', 0}, {'a', 0}, {'C', 0}} // 1 atomic var, all atomic op kinds
	pl1 := []spOp{{'r', 0}, {'w', 0}}                     // plain ops on the SAME atomic var
	// (A1) 2 goroutines, 2 atomic ops each over 1 atomic var — the atomic
	// decision-point dependency relation end to end: same-address atomic pairs
	// (CAS winners, store orders, add chains) must explore both orders, while
	// the conservative HB events the hook records (load:acq, store:rel,
	// RMW:acq+rel, CAS:acq-only) must prune only truly ordered pairs — an
	// over-claimed edge (e.g. a failed CAS publishing a release) loses an
	// outcome here vs Exhaustive.
	for _, a := range at1 {
		for _, b := range at1 {
			for _, c := range at1 {
				for _, d := range at1 {
					fam = append(fam, spProg{nAVars: 1, gor: [][]spOp{{a, b}, {c, d}}})
				}
			}
		}
	}
	// (A2) atomic-plain mixed: one goroutine's atomic ops against another's
	// PLAIN accesses of the same memory — atomics must pair with plain
	// accesses in the byte-interval dependency relation (a data race the
	// manual build runs deterministically; the -race oracle is separate).
	for _, a := range at1 {
		for _, b := range at1 {
			for _, c := range pl1 {
				for _, d := range pl1 {
					fam = append(fam, spProg{nAVars: 1, gor: [][]spOp{{a, b}, {c, d}}})
				}
			}
		}
	}
	// (A3) 3 goroutines, 1 atomic op each — multi-way same-address contention
	// (3-way CAS/store orders).
	for _, a := range at1 {
		for _, b := range at1 {
			for _, c := range at1 {
				fam = append(fam, spProg{nAVars: 1, gor: [][]spOp{{a}, {b}, {c}}})
			}
		}
	}
	// (A4) plain and atomic MIXED WITHIN each goroutine: g0 plain-then-atomic,
	// g1 atomic-then-plain, one shared var. Exercises atomic-HB pruning over
	// plain accesses interleaved with the atomics in program order (e.g.
	// g0=[w,C] vs g1=[l,r]). Like (A5), it enforces the modeling's soundness
	// envelope via DPOR==Exhaustive; an over-claimed edge is outcome-MASKED
	// here too (the same-address announce pair recovers the class), so the
	// acquire-only CAS choice is held by design, not by a failing member.
	for _, p0 := range pl1 {
		for _, a0 := range at1 {
			for _, a1 := range at1 {
				for _, p1 := range pl1 {
					fam = append(fam, spProg{nAVars: 1, gor: [][]spOp{{p0, a0}, {a1, p1}}})
				}
			}
		}
	}
	// (A5) the two-variable form of (A4): plain ops target avar 1, atomics
	// avar 0 — the PLAIN pair is the only dependent pair between the
	// goroutines and the atomic pair their only sync channel. Stresses
	// pruning driven purely by recorded atomic HB (e.g. g0=[w(1), CAS(0)]
	// vs g1=[l(0), r(1)]: whether r-before-w survives depends on the atomic
	// edges alone). Note an over-claimed edge (a failed CAS publishing a
	// release) is outcome-MASKED in the current explorer — the same-address
	// announce pair is always reorderable and re-analysis of the reordered
	// trace drops the claimed edge — so this family enforces the modeling's
	// soundness envelope, not the acquire-only choice itself (which is held
	// to the memory model by design, not by a failing test).
	pl2 := []spOp{{'r', 1}, {'w', 1}}
	for _, p0 := range pl2 {
		for _, a0 := range at1 {
			for _, a1 := range at1 {
				for _, p1 := range pl2 {
					fam = append(fam, spProg{nAVars: 2, gor: [][]spOp{{p0, a0}, {a1, p1}}})
				}
			}
		}
	}
	if !heavy {
		// Standing sweep stops here (~8s). The heavy families below add no bug class
		// the standing families miss — the trace-HB source-set regression is already
		// caught by family (3) (a sync-HB source set drops a class there, e.g.
		// prog#274/276) — but they exercise the weak-initial/sleep interaction (and the
		// all-enabled fallback) far harder, so DSTSWEEP=heavy runs them on demand.
		return fam
	}
	// (5) 3 goroutines, 2 ops each over 1 var, NO sync — a write among independent
	// MULTI-op readers: the hardest stress on the trace-happens-before weak-initials
	// (exhaustive up to ~2520/program). Validated complete (mismatches=0).
	for _, a := range rw1 {
		for _, b := range rw1 {
			for _, c := range rw1 {
				for _, d := range rw1 {
					for _, e := range rw1 {
						for _, f := range rw1 {
							fam = append(fam, spProg{nVars: 1, gor: [][]spOp{{a, b}, {c, d}, {e, f}}})
						}
					}
				}
			}
		}
	}
	// (6) 4 goroutines, 1 op each over 1 var — higher-multiplicity contention.
	for _, a := range rw1 {
		for _, b := range rw1 {
			for _, c := range rw1 {
				for _, d := range rw1 {
					fam = append(fam, spProg{nVars: 1, gor: [][]spOp{{a}, {b}, {c}, {d}}})
				}
			}
		}
	}
	return fam
}

// sweepSeen accumulates the distinct observable outcomes of the program currently
// under Explore (reset by DSTExploreSweep before each Explore call; written by the
// interpreter SUT once per interleaving). Package-global because the SUT signature
// is func() bool — the same closure is invoked once per bubble re-execution within
// one Explore call, so it accumulates the whole run's outcome set here.
var sweepSeen map[string]bool

// makeSweepSUT builds the interpreter SUT for one program: every Run re-creates the
// program's shared state fresh, runs each goroutine's instructions (an access-yield
// before each shared read/write so the access is a DPOR transition; mutexes are
// coarse points, no yield), then records the observable outcome into sweepSeen. The
// outcome — final var values plus every goroutine's read log, with writes storing a
// (goroutine,step)-unique value — distinguishes every interleaving whose ordering
// of dependent accesses differs, so distinct outcomes is a lower bound on the
// number of Mazurkiewicz classes. Always returns false (the sweep checks
// completeness via the outcome set, not a bug assertion).
func makeSweepSUT(p spProg) func() bool {
	return func() bool {
		vars := make([]int, p.nVars)
		avars := make([]int32, p.nAVars)
		mus := make([]sync.Mutex, p.nMu)
		logs := make([][]int, len(p.gor))
		var wg sync.WaitGroup
		wg.Add(len(p.gor))
		for gi := range p.gor {
			gi := gi
			instrs := p.gor[gi]
			go func() {
				defer wg.Done()
				for si, ins := range instrs {
					switch ins.kind {
					case 'R':
						dstAccessYield(unsafe.Pointer(&vars[ins.arg]), false)
						logs[gi] = append(logs[gi], vars[ins.arg])
					case 'W':
						dstAccessYield(unsafe.Pointer(&vars[ins.arg]), true)
						vars[ins.arg] = 1000*(gi+1) + si
					case 'L':
						// Announce the impending acquisition as a write-conflict on the
						// mutex identity BEFORE Lock, so two acquisitions of the same mutex
						// by different goroutines are a co-enabled, concurrent, conflicting
						// pair DPOR explores both orders. Without it the acquisition-order
						// classes are silently dropped (DST-L2-3): TestDSTExploreSweep
						// loses these classes if acquisitions are not announced.
						dstSyncAcquire(unsafe.Pointer(&mus[ins.arg]))
						mus[ins.arg].Lock()
					case 'U':
						mus[ins.arg].Unlock()
					case 'l':
						dstAtomicYield(unsafe.Pointer(&avars[ins.arg]), 4, 0)
						logs[gi] = append(logs[gi], int(atomic.LoadInt32(&avars[ins.arg])))
					case 's':
						dstAtomicYield(unsafe.Pointer(&avars[ins.arg]), 4, 1)
						atomic.StoreInt32(&avars[ins.arg], int32(1000*(gi+1)+si))
					case 'a':
						// The returned new value distinguishes the op's position in
						// the addition order (the final sum alone would not).
						dstAtomicYield(unsafe.Pointer(&avars[ins.arg]), 4, 2)
						logs[gi] = append(logs[gi], int(atomic.AddInt32(&avars[ins.arg], int32(10*(gi+1)+si+1))))
					case 'C':
						// CAS from zero: succeeds only for the first CAS the schedule
						// commits (and fails after any store/add), so both the success
						// bit and the final value are order observables. Exercises the
						// acquire-only HB modeling on both outcomes.
						dstAtomicYield(unsafe.Pointer(&avars[ins.arg]), 4, 3)
						ok := atomic.CompareAndSwapInt32(&avars[ins.arg], 0, int32(1000*(gi+1)+si))
						v := 0
						if ok {
							v = 1
						}
						logs[gi] = append(logs[gi], v)
					case 'r':
						dstAccessYield(unsafe.Pointer(&avars[ins.arg]), false)
						logs[gi] = append(logs[gi], int(avars[ins.arg]))
					case 'w':
						dstAccessYield(unsafe.Pointer(&avars[ins.arg]), true)
						avars[ins.arg] = int32(1000*(gi+1) + si)
					}
				}
			}()
		}
		wg.Wait()
		obs := append([]int{}, vars...)
		for _, v := range avars {
			obs = append(obs, int(v))
		}
		sweepSeen[encodeObs(obs, logs)] = true
		return false
	}
}

// encodeObs serializes a program outcome (final var values + per-goroutine read
// logs) into a canonical string key.
func encodeObs(vars []int, logs [][]int) string {
	b := make([]byte, 0, 32)
	for _, v := range vars {
		b = strconv.AppendInt(b, int64(v), 10)
		b = append(b, ',')
	}
	b = append(b, '|')
	for _, lg := range logs {
		for _, v := range lg {
			b = strconv.AppendInt(b, int64(v), 10)
			b = append(b, ',')
		}
		b = append(b, ';')
	}
	return string(b)
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// encodeInts serializes an ordered []int (e.g. a channel-rendezvous order) into a
// canonical string key.
func encodeInts(xs []int) string {
	b := make([]byte, 0, 16)
	for _, v := range xs {
		b = strconv.AppendInt(b, int64(v), 10)
		b = append(b, ',')
	}
	return string(b)
}

// chanChoiceSUT exercises a synchronization-ACQUISITION order that is not a memory
// access: two senders send distinct values to one unbuffered channel; a receiver
// receives twice and records the rendezvous order. WHICH sender rendezvous first is
// a free scheduling choice → two reachable outcomes ([1,2] and [2,1]). Each sender
// announces the channel identity (dstSyncAcquire) before its send so the order is a
// DPOR transition; without the announce DPOR drops one order (DST-L2-3), exactly as
// for mutex acquisition — channels are the second sync primitive the fix covers.
func chanChoiceSUT() bool {
	ch := make(chan int)
	var got []int
	var wg sync.WaitGroup
	wg.Add(3)
	for s := 1; s <= 2; s++ {
		s := s
		go func() {
			defer wg.Done()
			dstSyncAcquire(unsafe.Pointer(&ch)) // rendezvous order is a scheduling choice
			ch <- s
		}()
	}
	go func() {
		defer wg.Done()
		for i := 0; i < 2; i++ {
			got = append(got, <-ch)
		}
	}()
	wg.Wait()
	sweepSeen[encodeInts(got)] = true
	return false
}

var timerHBSeen map[string]bool

// timerHBSUT gates both sides of an unsynchronized read/write behind fake timers.
// When virtual time advances, both timers fire before either goroutine resumes, so
// the post-sleep read and write are co-enabled. DPOR must therefore reach both read
// outcomes; treating timer-fire wakeups as ordering the two goroutines would silently
// drop one class (DST-L2-3).
func timerHBSUT() bool {
	x := 0
	read := -1
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		time.Sleep(time.Nanosecond)
		dstAccessYield(unsafe.Pointer(&x), false)
		read = x
	}()
	go func() {
		defer wg.Done()
		time.Sleep(time.Nanosecond)
		dstAccessYield(unsafe.Pointer(&x), true)
		x = 1
	}()
	wg.Wait()
	outcome := strconv.Itoa(read)
	if timerHBSeen != nil {
		timerHBSeen[outcome] = true
	}
	if sweepSeen != nil {
		sweepSeen[outcome] = true
	}
	return false
}

func timerHBCompare(seed uint64) (exh, dpor simulation.ExploreResult, exhSet, dporSet map[string]bool) {
	timerHBSeen = map[string]bool{}
	dpor = simulation.Explore(seed, simulation.DPOR, timerHBSUT)
	dporSet = map[string]bool{}
	for v := range timerHBSeen {
		dporSet[v] = true
	}
	timerHBSeen = map[string]bool{}
	exh = simulation.Explore(seed, simulation.Exhaustive, timerHBSUT)
	exhSet = map[string]bool{}
	for v := range timerHBSeen {
		exhSet[v] = true
	}
	return exh, dpor, exhSet, dporSet
}

func DSTExploreTimerHB() {
	exh, dpor, exhSet, dporSet := timerHBCompare(dstSeedEnv())
	os.Stdout.WriteString("timerhb exh=" + strconv.Itoa(exh.Schedules) +
		" dpor=" + strconv.Itoa(dpor.Schedules) +
		" outcomes=" + strconv.Itoa(len(exhSet)) +
		" complete=" + strconv.FormatBool(sameSet(exhSet, dporSet)) +
		" exhExhausted=" + strconv.FormatBool(exh.Exhausted) +
		" dporExhausted=" + strconv.FormatBool(dpor.Exhausted) +
		" overflow=" + strconv.FormatBool(exh.Overflow || dpor.Overflow) + "\n")
}

// namedSweepSUT is a hand-written hard SUT for the equivalence sweep, beyond the
// generated interpreter family (which models only reads/writes/mutexes).
type namedSweepSUT struct {
	name string
	sut  func() bool
}

func namedSweepSUTs() []namedSweepSUT {
	return []namedSweepSUT{
		{"chan-choice", chanChoiceSUT},
		{"timer-hb", timerHBSUT},
	}
}

// sweepStats accumulates the equivalence-sweep results across all checked SUTs.
type sweepStats struct {
	checks                           int
	mismatches                       int
	totExh, totDpor, maxExh, maxDpor int
	optimal                          int
	firstBad, badList                string
}

// sweepCheck runs one SUT under Exhaustive and DPOR and folds the verdict into st.
// A mismatch is a DST-L2-3 violation: DPOR's observable-outcome set differs from
// Exhaustive's, either mode failed to cleanly exhaust, or DPOR explored MORE
// schedules than Exhaustive.
func sweepCheck(st *sweepStats, seed uint64, label string, sut func() bool) {
	oldPostGo := dstSetPostGoYield(false)
	defer dstSetPostGoYield(oldPostGo)
	st.checks++
	sweepSeen = map[string]bool{}
	exhRes := simulation.Explore(seed, simulation.Exhaustive, sut)
	exhSet := sweepSeen
	nObs := len(exhSet)
	sweepSeen = map[string]bool{}
	dporRes := simulation.Explore(seed, simulation.DPOR, sut)
	dporSet := sweepSeen
	bad := func(why string) {
		st.mismatches++
		st.badList += " " + label
		if st.firstBad == "" {
			st.firstBad = label + ": " + why
		}
	}
	switch {
	case !exhRes.Exhausted || exhRes.Overflow || !dporRes.Exhausted || dporRes.Overflow:
		bad("not-exhausted-or-overflow")
	case !sameSet(exhSet, dporSet):
		bad("set-mismatch exhObs=" + strconv.Itoa(len(exhSet)) + " dporObs=" + strconv.Itoa(len(dporSet)))
	case dporRes.Schedules > exhRes.Schedules:
		bad("dpor=" + strconv.Itoa(dporRes.Schedules) + ">exh=" + strconv.Itoa(exhRes.Schedules))
	default:
		// Optimality stats (totDpor/maxDpor/optimal) accumulate only for mismatch-free
		// programs — they are a metric layered behind the completeness gate, not a
		// completeness signal themselves (a dropped class shows up as a mismatch above).
		st.totExh += exhRes.Schedules
		st.totDpor += dporRes.Schedules
		if exhRes.Schedules > st.maxExh {
			st.maxExh = exhRes.Schedules
		}
		if dporRes.Schedules > st.maxDpor {
			st.maxDpor = dporRes.Schedules
		}
		if dporRes.Schedules == nObs {
			st.optimal++
		}
	}
}

// DSTExploreSweep runs the generated-family + named-SUT equivalence validator and
// prints a one-line summary: "sweep programs=<n> mismatches=<m> totExh=.. totDpor=..
// maxExh=.. maxDpor=.. optimal=<k>". mismatches counts SUTs where DPOR's
// observable-outcome set differs from Exhaustive's, or either mode failed to
// cleanly exhaust, or DPOR explored MORE schedules than Exhaustive — any nonzero is
// a DST-L2-3 violation. optimal counts SUTs where DPOR explored exactly as many
// schedules as there are distinct outcomes (a proxy for "no class twice").
// totDpor/totExh quantify the reduction. On a mismatch it also prints a "firstBad="
// detail line and the list of failing labels for debugging.
func DSTExploreSweep() {
	fam := sweepFamily(os.Getenv("DSTSWEEP") == "heavy")
	named := namedSweepSUTs()
	seed := dstSeedEnv()
	st := &sweepStats{}
	for pi, p := range fam {
		sweepCheck(st, seed, "prog#"+strconv.Itoa(pi), makeSweepSUT(p))
	}
	for _, ns := range named {
		sweepCheck(st, seed, ns.name, ns.sut)
	}
	os.Stdout.WriteString("sweep programs=" + strconv.Itoa(st.checks) +
		" mismatches=" + strconv.Itoa(st.mismatches) +
		" totExh=" + strconv.Itoa(st.totExh) + " totDpor=" + strconv.Itoa(st.totDpor) +
		" maxExh=" + strconv.Itoa(st.maxExh) + " maxDpor=" + strconv.Itoa(st.maxDpor) +
		" optimal=" + strconv.Itoa(st.optimal) + "\n")
	if st.firstBad != "" {
		os.Stdout.WriteString("firstBad=" + st.firstBad + "\n")
		os.Stdout.WriteString("badLabels=[" + st.badList + " ]\n")
	}
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

// DSTExploreAtomicAuto is the acceptance for the dst-race sync/atomic
// decision-point emission: an UNMODIFIED SUT
// whose outcome depends on the order of same-address atomic operations (the
// CAS winner, the last store, an add racing a load) — or on a len(ch)
// observed concurrently with a send — built -tags dst -race, must have DPOR
// reach BOTH outcomes with NO manual hooks. The compiler emits dstAtomicYield
// before each static sync/atomic call in instrumented code; chanlen announces
// via dstSyncObserve. Same DPOR-only oracle as DSTExploreSyncAuto: each SUT
// is two goroutines contending over one decision with EXACTLY two outcomes by
// construction, so Outcomes==2 with Exhausted==true is the completeness
// signal; with the emission (or a hook) missing, the decision commits inside
// TSan's NOSPLIT atomic assembly with no recorded transition and DPOR finds 1.
// The typed-API SUT proves the out-of-line method classification (sync/atomic
// is noRaceFunc, so its methods are not inlinable under -race; the emission
// must land at the instrumented call site by classifying the method symbol).
// Meaningful only in a -tags dst -race build.
func DSTExploreAtomicAuto() {
	seed := dstSeedEnv()
	check := func(name string, sut func() bool) {
		syncAutoSeen = map[string]bool{}
		dpor := simulation.Explore(seed, simulation.DPOR, sut)
		os.Stdout.WriteString(name + "Dpor=" + strconv.Itoa(dpor.Schedules) +
			" " + name + "Outcomes=" + strconv.Itoa(len(syncAutoSeen)) +
			" " + name + "Exhausted=" + strconv.FormatBool(dpor.Exhausted && !dpor.Overflow) + "\n")
	}
	os.Stdout.WriteString("atomicauto\n")
	check("caswinner", autoAtomicCASWinnerSUT)
	check("storeorder", autoAtomicStoreOrderSUT)
	check("addload", autoAtomicAddLoadSUT)
	check("swaporder", autoAtomicSwapOrderSUT)
	check("orand", autoAtomicOrAndSUT)
	check("store64", autoAtomicStore64OrderSUT)
	check("casptr", autoAtomicCASPointerSUT)
	check("typedcas", autoAtomicTypedCASSUT)
	check("lenobserve", autoChanLenObserveSUT)
}

// autoAtomicCASWinnerSUT: both goroutines CAS the same zeroed int32 to their
// id; exactly one wins, and which is the decision (outcomes "1"/"2").
func autoAtomicCASWinnerSUT() bool {
	var x int32
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 1; i <= 2; i++ {
		id := int32(i)
		go func() {
			defer wg.Done()
			atomic.CompareAndSwapInt32(&x, 0, id)
		}()
	}
	wg.Wait()
	syncAutoSeen[strconv.Itoa(int(atomic.LoadInt32(&x)))] = true
	return false
}

// autoAtomicStoreOrderSUT: last store wins (outcomes "1"/"2").
func autoAtomicStoreOrderSUT() bool {
	var x int32
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 1; i <= 2; i++ {
		id := int32(i)
		go func() {
			defer wg.Done()
			atomic.StoreInt32(&x, id)
		}()
	}
	wg.Wait()
	syncAutoSeen[strconv.Itoa(int(atomic.LoadInt32(&x)))] = true
	return false
}

// autoAtomicAddLoadSUT: a load racing an add observes 0 or 1.
func autoAtomicAddLoadSUT() bool {
	var x, seen int32
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		atomic.AddInt32(&x, 1)
	}()
	go func() {
		defer wg.Done()
		atomic.StoreInt32(&seen, atomic.LoadInt32(&x))
	}()
	wg.Wait()
	syncAutoSeen[strconv.Itoa(int(atomic.LoadInt32(&seen)))] = true
	return false
}

// autoAtomicSwapOrderSUT: last swap wins (outcomes "1"/"2").
func autoAtomicSwapOrderSUT() bool {
	var x int32
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 1; i <= 2; i++ {
		id := int32(i)
		go func() {
			defer wg.Done()
			atomic.SwapInt32(&x, id)
		}()
	}
	wg.Wait()
	syncAutoSeen[strconv.Itoa(int(atomic.LoadInt32(&x)))] = true
	return false
}

// autoAtomicOrAndSUT: Or(1) vs And(0) — final value is 0 if the Or commits
// first, 1 if the And does (outcomes "0"/"1"); covers the And/Or hook names.
func autoAtomicOrAndSUT() bool {
	var x int32
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		atomic.OrInt32(&x, 1)
	}()
	go func() {
		defer wg.Done()
		atomic.AndInt32(&x, 0)
	}()
	wg.Wait()
	syncAutoSeen[strconv.Itoa(int(atomic.LoadInt32(&x)))] = true
	return false
}

// autoAtomicStore64OrderSUT: the 64-bit widths route through the same
// emission (outcomes "1"/"2").
func autoAtomicStore64OrderSUT() bool {
	var x int64
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 1; i <= 2; i++ {
		id := int64(i)
		go func() {
			defer wg.Done()
			atomic.StoreInt64(&x, id)
		}()
	}
	wg.Wait()
	syncAutoSeen[strconv.FormatInt(atomic.LoadInt64(&x), 10)] = true
	return false
}

// autoAtomicCASPointerSUT: pointer-width CAS winner (outcomes "1"/"2").
func autoAtomicCASPointerSUT() bool {
	var p unsafe.Pointer
	var a, b int32 = 1, 2
	var wg sync.WaitGroup
	wg.Add(2)
	for _, t := range []*int32{&a, &b} {
		t := t
		go func() {
			defer wg.Done()
			atomic.CompareAndSwapPointer(&p, nil, unsafe.Pointer(t))
		}()
	}
	wg.Wait()
	syncAutoSeen[strconv.Itoa(int(*(*int32)(atomic.LoadPointer(&p))))] = true
	return false
}

// autoAtomicTypedCASSUT: the typed API (atomic.Int32 methods) — out-of-line
// under -race (noRaceFunc methods are not inlinable), so the emission must
// come from classifying the method symbol at the user call site (outcomes
// "1"/"2").
func autoAtomicTypedCASSUT() bool {
	var x atomic.Int32
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 1; i <= 2; i++ {
		id := int32(i)
		go func() {
			defer wg.Done()
			x.CompareAndSwap(0, id)
		}()
	}
	wg.Wait()
	syncAutoSeen[strconv.Itoa(int(x.Load()))] = true
	return false
}

// autoChanLenObserveSUT: len(ch) observed concurrently with a send sees 0 or
// 1 — the chanlen runtime hook (dstSyncObserve) makes the observation a
// decision DPOR reverses against the send.
func autoChanLenObserveSUT() bool {
	ch := make(chan int, 1)
	var n int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ch <- 1
	}()
	go func() {
		defer wg.Done()
		n = len(ch)
	}()
	wg.Wait()
	syncAutoSeen[strconv.Itoa(n)] = true
	return false
}
