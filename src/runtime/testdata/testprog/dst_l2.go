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
	register("DSTExploreSweep", DSTExploreSweep)
	register("DSTExploreRaceOracle", DSTExploreRaceOracle)
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

// dstSyncAcquire announces a synchronization-object acquisition (mutex Lock,
// channel rendezvous) as a write-conflict on the object's identity and yields
// BEFORE the blocking op, so the acquisition ORDER is a DPOR transition (the order
// in which contending goroutines acquire is a real scheduling choice that can
// change the outcome). Placed manually here; the dst-race compiler/runtime phase
// wires real sync primitives to it automatically.
//
//go:linkname dstSyncAcquire runtime.dstSyncAcquire
func dstSyncAcquire(id unsafe.Pointer)

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
// the writer first misses the race entirely; the explorer, which explores both
// acquisition orders (dstSyncAcquire on the mutex — the sync-acquisition-order
// machinery), reaches the reader-first schedule and the -race oracle reports it. No
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

// DSTExploreRaceOracle runs a race SUT (DSTRACE=uncond|cond, default uncond) under
// Explore and prints "raceoracle schedules=<n> races=<m> exhausted=<bool>
// firstrace=[g,g,...]". races counts the Failures the -race oracle flagged
// (Failure.Race); firstrace is the schedule that reproduces the first one
// (comma-separated, no spaces, so it is a single output token). Meaningful only in a
// -race build; in a non-race build races=0.
func DSTExploreRaceOracle() {
	sut := raceOracleSUT
	if os.Getenv("DSTRACE") == "cond" {
		sut = raceCondSUT
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

// --- Generated-family equivalence validator (DST-L2-3 completeness guard) ------
//
// The micro-SUTs above are a weak net for DPOR completeness: each pins one shape.
// DSTExploreSweep generates a *family* of small concurrent programs (varying
// goroutine count, accesses, vars, and mutex synchronization) and, for every
// member, asserts that DPOR reaches the IDENTICAL set of observable outcomes as
// brute-force Exhaustive enumeration — the real DST-L2-3 guard, especially for the
// optimal-DPOR (sleep-set) work, whose failure mode is silently dropping a
// Mazurkiewicz class while still reporting Exhausted=true. See docs/dst/design.md
// (Level 2, increment 5, "Validator first").

// spOp is one instruction of a generated program: a read/write of shared var arg,
// or a lock/unlock of mutex arg.
type spOp struct {
	kind byte // 'R' read, 'W' write, 'L' lock, 'U' unlock
	arg  int  // var index (R/W) or mutex index (L/U)
}

// spProg is a generated program: nVars shared ints, nMu mutexes, and one
// instruction sequence per goroutine.
type spProg struct {
	nVars int
	nMu   int
	gor   [][]spOp
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
					}
				}
			}()
		}
		wg.Wait()
		sweepSeen[encodeObs(vars, logs)] = true
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

// namedSweepSUT is a hand-written hard SUT for the equivalence sweep, beyond the
// generated interpreter family (which models only reads/writes/mutexes).
type namedSweepSUT struct {
	name string
	sut  func() bool
}

func namedSweepSUTs() []namedSweepSUT {
	return []namedSweepSUT{
		{"chan-choice", chanChoiceSUT},
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
