// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"context"
	"internal/synctest"
	"internal/testenv"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

//go:linkname dstAccessYield runtime.dstAccessYield
func dstAccessYield(addr unsafe.Pointer, write bool)

//go:linkname dstAccessYieldRange runtime.dstAccessYieldRange
func dstAccessYieldRange(addr unsafe.Pointer, size uintptr, write bool)

//go:linkname dstAccessYieldFP runtime.dstAccessYieldFP
func dstAccessYieldFP() uint64

//go:linkname dstAccessYieldReset runtime.dstAccessYieldReset
func dstAccessYieldReset()

//go:linkname dstYieldPoint runtime.dstYieldPoint
func dstYieldPoint()

//go:linkname dstSetPostGoYield runtime.dstSetPostGoYield
func dstSetPostGoYield(enabled bool) bool

//go:linkname dstRunningPanicDefersFP runtime.dstRunningPanicDefersFP
func dstRunningPanicDefersFP() uint32

//go:linkname dstCurrentSeqFP runtime.dstCurrentSeqFP
func dstCurrentSeqFP() uint64

//go:linkname dstSyncEventOverflowProbe runtime.dstSyncEventOverflowFP
func dstSyncEventOverflowProbe() bool

func assertUniqueEnabledSeqs(t *testing.T, tr exploreTrace) {
	t.Helper()
	for i, enabled := range tr.enabled {
		seen := map[uint64]bool{}
		for _, seq := range enabled {
			if seq == 0 {
				t.Fatalf("decision %d has unassigned seq in enabled set %v", i, enabled)
			}
			if seen[seq] {
				t.Fatalf("decision %d has duplicate seq %d in enabled set %v", i, seq, enabled)
			}
			seen[seq] = true
		}
	}
}

func TestExploreAccessLogOverflowReportsIncomplete(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	dstExploreInit(16, 64, 16, 0)
	x := 0
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		dstAccessYield(unsafe.Pointer(&x), true)
		x = 1
		return false
	})
	if !tr.overflow {
		t.Fatalf("access-log overflow did not mark trace incomplete")
	}
}

func TestReplayInstallsAccessForces(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("requires -race so manual dstAccessYield uses the shared-address filter")
	}

	x := 0
	sut := func() bool {
		dstAccessYield(unsafe.Pointer(&x), true)
		x = 1
		return false
	}

	dstExploreInit(64, 64, 64, 64)
	dstAccessYieldReset()
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, sut)
	unforced := dstAccessYieldFP()
	force := AccessForce{}
	for i := range tr.accSeq {
		if tr.accCount[i] != 0 && tr.accPC[i] != 0 {
			force = AccessForce{Seq: tr.accSeq[i], Count: tr.accCount[i], PCKey: tr.accPC[i]}
			break
		}
	}
	if force.Count == 0 {
		t.Fatalf("missing replayable access metadata: %#v", tr)
	}

	dstAccessYieldReset()
	failed, raced := Replay(1, Failure{AccessForces: []AccessForce{force}}, sut)
	if failed || raced {
		t.Fatalf("replay reported unexpected failure: failed=%v raced=%v", failed, raced)
	}
	if got := dstAccessYieldFP(); got <= unforced {
		t.Fatalf("Replay did not install the forced access yield: unforced=%d forced=%d", unforced, got)
	}
}

// TestExploreForeignSpinner: exploration under a persistently-runnable
// foreign goroutine. The scheduled strategy's fairness hand-off skips
// infrastructure candidates, so the spinner neither starves episodes (the
// exploration completes and exhausts) nor enters the recorded schedules and
// DPOR enabled sets (the exploration covers exactly the interleavings and
// failures a spinner-free exploration covers).
func TestExploreForeignSpinner(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if dstRaceEnabledFP() {
		// The sut's intentional data race fails tRunner under -race, and the
		// dst-race yield placement is foreign-sensitive (ForeignSched reports
		// it; TestExploreForeignSchedReported covers the -race behavior).
		t.Skip("intentionally racy sut; non-race trace regression")
	}
	sut := func() bool {
		x := 0
		read := -1
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			read = x
		}()
		x = 1
		// Route this simulation goroutine through the GLOBAL runq: a Gosched'd
		// bubble goroutine re-enqueues at the global tail — BEHIND a foreign
		// spinner already parked there — so the recorded enabled sets are
		// pinned against a foreign entry enumerated AHEAD of a simulation
		// candidate (the ordering where an unfiltered recording loop would
		// leak the spinner into the enabled-set window).
		runtime.Gosched()
		wg.Wait()
		// A LATE goroutine, first enabled only after mixed sets have already
		// occurred: its stable index is assigned at first appearance, so a
		// spinner wrongly consuming the seq counter shifts this goroutine's
		// index and diverges the recorded trace from the spinner-free one.
		var wg2 sync.WaitGroup
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			runtime.Gosched()
		}()
		wg2.Wait()
		return read == 0
	}
	alone := Explore(1, Exhaustive, sut)
	_, _, trAlone := runOnce(1, nil, map[accessForce]bool{}, sut)
	// TWO spinners: at a fairness decision the just-picked spinner has always
	// re-enqueued at the global tail (behind every simulation candidate), so a
	// single spinner can never occupy the leading position an enabled-set
	// recording leak needs — the second spinner is the one already queued
	// AHEAD of the Gosched'd simulation goroutine when the decision records.
	stop := make(chan struct{})
	var done sync.WaitGroup
	for i := 0; i < 2; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				runtime.Gosched()
			}
		}()
	}
	// The traced episode runs FIRST, while the spinners are fresh (dstSeq 0 —
	// gdestroy clears scheduled identity, so a recycled g cannot smuggle a
	// stale index in): a stable-index assignment wrongly reaching a spinner
	// happens at the first mixed decision, deterministically before the late
	// goroutine's first appearance, and shifts its index. Tracing after the
	// spun Explore would mask that — its first episode would stamp the
	// spinners, and a stamped spinner consumes nothing on later episodes.
	_, _, trSpun := runOnce(1, nil, map[accessForce]bool{}, sut)
	spun := Explore(1, Exhaustive, sut)
	close(stop)
	done.Wait()
	if alone.ForeignSched || !alone.Exhausted {
		t.Fatalf("foreign-free exploration misreported: foreignSched=%v exhausted=%v", alone.ForeignSched, alone.Exhausted)
	}
	// Under churn the exploration must complete and cover the same
	// interleavings, but its exhaustion claim is downgraded and the foreign
	// presence reported — coverage under churn is best-effort, never a silent
	// claim (the dst-race yield placement is demonstrably foreign-sensitive).
	if !spun.ForeignSched || spun.Exhausted || spun.Overflow {
		t.Fatalf("exploration under a foreign spinner misreported: foreignSched=%v exhausted=%v overflow=%v", spun.ForeignSched, spun.Exhausted, spun.Overflow)
	}
	if spun.Schedules != alone.Schedules || len(spun.Failures) != len(alone.Failures) {
		t.Fatalf("foreign spinner changed exploration coverage: schedules %d vs %d, failures %d vs %d",
			spun.Schedules, alone.Schedules, len(spun.Failures), len(alone.Failures))
	}
	// Direct trace pin: the spinner must be invisible in the recorded
	// schedule — same decisions chosen, same enabled sets, no unassigned or
	// duplicate seq (a spinner leaking into the recording would carry seq 0,
	// and one consuming the seq counter would shift every later seq).
	assertUniqueEnabledSeqs(t, trSpun)
	if !reflect.DeepEqual(trAlone.procs, trSpun.procs) || !reflect.DeepEqual(trAlone.enabled, trSpun.enabled) {
		t.Fatalf("foreign spinner visible in the recorded schedule:\nalone procs=%v enabled=%v\nspun  procs=%v enabled=%v",
			trAlone.procs, trAlone.enabled, trSpun.procs, trSpun.enabled)
	}
}

// TestDSTForeignSeqRefused pins the membership chokepoint (dstEnsureSeq)
// directly: during an active run, only a goroutine of the simulation's own
// bubble receives a stable index — a bare pre-run goroutine and a FOREIGN
// synctest bubble's goroutine (which passes any mere bubble-ness check) both
// get 0, so they can neither consume the per-episode seq counter nor carry a
// stale index into later episodes.
func TestDSTForeignSeqRefused(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	probe := make(chan func())
	result := make(chan uint64)
	go func() {
		for f := range probe {
			f()
		}
	}()
	defer close(probe)

	var member uint64
	Run(1, func() {
		probe <- func() { result <- dstEnsureSeqSelfFP() } // bare foreign goroutine
		bare := <-result
		probe <- func() {
			var got uint64
			synctest.Run(func() { got = dstEnsureSeqSelfFP() }) // foreign bubble
			result <- got
		}
		foreignBubble := <-result
		if bare != 0 || foreignBubble != 0 {
			panic("foreign goroutine received a stable index: bare=" +
				strconv.FormatUint(bare, 10) + " foreignBubble=" + strconv.FormatUint(foreignBubble, 10))
		}
		member = dstEnsureSeqSelfFP() // the run body: a sim-bubble member
	})
	if member == 0 {
		t.Fatal("a simulation-bubble goroutine was refused a stable index")
	}
}

// TestExploreForeignBubbleSyncChurn pins the membership chokepoint
// (dstEnsureSeq): a FOREIGN synctest bubble live concurrently with an
// exploration — whose goroutines pass any mere bubble-ness check — must not
// perturb recorded schedules, stable-index assignment, or the offline HB
// state. The foreign bubble churns mutex lock/unlock (sync release/acquire
// events that reach the recording surfaces) and Gosched (candidacy at
// simulation decisions); before the chokepoint, its goroutines drew stable
// indices from the per-episode seq counter at foreign-timing-dependent
// points — shifting the late simulation goroutine's index — and carried
// stale indices into later episodes while the counter reset, colliding with
// fresh simulation indices in the HB clocks. The SUT and assertions mirror
// TestExploreForeignSpinner: identical coverage and byte-identical traces
// alone vs churned, with the foreign presence reported, never silent.
func TestExploreForeignBubbleSyncChurn(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if dstRaceEnabledFP() {
		t.Skip("intentionally racy sut; non-race trace regression (ForeignSched covers -race)")
	}
	sut := func() bool {
		x := 0
		read := -1
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			read = x
		}()
		x = 1
		runtime.Gosched()
		wg.Wait()
		var wg2 sync.WaitGroup
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			runtime.Gosched()
		}()
		wg2.Wait()
		return read == 0
	}
	alone := Explore(1, Exhaustive, sut)
	_, _, trAlone := runOnce(1, nil, map[accessForce]bool{}, sut)

	// The foreign bubble: never durably blocked (Gosched loop on an external
	// flag), so its synctest.Run does not return until stopped; each
	// iteration's mutex ops hit the sync-event recording surface from a
	// goroutine whose bubble is non-nil but NOT the simulation's.
	var stop, started atomic.Bool
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		synctest.Run(func() {
			// UNBUFFERED rendezvous between two foreign-bubble goroutines:
			// each wake is a foreign-internal goready, which reaches
			// dstRecordReadyEdge in EVERY build — the non-race door to the
			// membership chokepoint (the sync-event and access-log doors are
			// race-instrumentation-gated; TestExploreForeignBubbleSyncChurnRace
			// covers those). The starvation-fairness alternation gives these
			// persistently-runnable goroutines slots throughout the episode,
			// so the churn's wakes record BEFORE the late simulation
			// goroutine draws its index.
			ch := make(chan int)
			go func() {
				for range ch {
				}
			}()
			for !stop.Load() {
				started.Store(true)
				ch <- 1
				runtime.Gosched()
			}
			close(ch)
		})
	}()
	for !started.Load() { // the churn must be live before the traced episode
		runtime.Gosched()
	}

	_, _, trChurned := runOnce(1, nil, map[accessForce]bool{}, sut)
	churned := Explore(1, Exhaustive, sut)
	stop.Store(true)
	done.Wait()

	if alone.ForeignSched || !alone.Exhausted {
		t.Fatalf("foreign-free exploration misreported: foreignSched=%v exhausted=%v", alone.ForeignSched, alone.Exhausted)
	}
	if !churned.ForeignSched || churned.Exhausted || churned.Overflow {
		t.Fatalf("exploration under a foreign bubble misreported: foreignSched=%v exhausted=%v overflow=%v",
			churned.ForeignSched, churned.Exhausted, churned.Overflow)
	}
	if churned.Schedules != alone.Schedules || len(churned.Failures) != len(alone.Failures) {
		t.Fatalf("foreign bubble changed exploration coverage: schedules %d vs %d, failures %d vs %d",
			churned.Schedules, alone.Schedules, len(churned.Failures), len(alone.Failures))
	}
	assertUniqueEnabledSeqs(t, trChurned)
	// The churn's rendezvous wakes are FOREIGN-INTERNAL goready calls, which
	// reach dstRecordReadyEdge in every build (the goready hook is gated on
	// the scheduled kind only): the membership chokepoint refuses both ends
	// and the degrade must buffer nothing — a buffered (0,0) edge would mint
	// a phantom proc in the offline clocks.
	for i := range trChurned.edgeFrom {
		if trChurned.edgeFrom[i] == 0 || trChurned.edgeTo[i] == 0 {
			t.Fatalf("edge %d has a zero endpoint (%d -> %d): a foreign wake was buffered",
				i, trChurned.edgeFrom[i], trChurned.edgeTo[i])
		}
	}
	// Defensive net (vacuous in non-race builds: no foreign path reaches the
	// access log here even ungated; the race-build door is pinned by
	// TestExploreForeignBubbleSyncChurnRace).
	for i, q := range trChurned.accSeq {
		if q == 0 {
			t.Fatalf("access log entry %d has seq 0: a foreign access was recorded", i)
		}
	}
	if !reflect.DeepEqual(trAlone.procs, trChurned.procs) || !reflect.DeepEqual(trAlone.enabled, trChurned.enabled) {
		t.Fatalf("foreign bubble visible in the recorded schedule:\nalone   procs=%v enabled=%v\nchurned procs=%v enabled=%v",
			trAlone.procs, trAlone.enabled, trChurned.procs, trChurned.enabled)
	}
}

func TestExploreDoesNotConsumeForeignBubbleFailure(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	mode := os.Getenv("DST_FOREIGN_FAILURE")
	if mode != "" {
		var trigger, done, propagated atomic.Bool
		go func() {
			defer done.Store(true)
			defer func() { propagated.Store(recover() != nil) }()
			synctest.Run(func() {
				for !trigger.Load() {
					runtime.Gosched()
				}
				if mode == "panic" {
					go func() { panic("foreign panic") }()
					for {
						runtime.Gosched()
					}
				}
				select {}
			})
		}()
		result := Explore(1, Exhaustive, func() bool {
			trigger.Store(true)
			for !done.Load() {
				runtime.Gosched()
			}
			return false
		})
		if !propagated.Load() {
			t.Fatal("foreign bubble failure was consumed")
		}
		if len(result.Failures) != 0 {
			t.Fatalf("foreign bubble failure attributed to exploration: %v", result.Failures)
		}
		return
	}
	for _, mode := range []string{"panic", "deadlock"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExploreDoesNotConsumeForeignBubbleFailure$")
			cmd.Env = append(os.Environ(), "DST_FOREIGN_FAILURE="+mode)
			out, err := cmd.CombinedOutput()
			if mode == "panic" {
				if ctx.Err() != nil || err == nil || !strings.Contains(string(out), "foreign panic") {
					t.Fatalf("foreign panic did not propagate: err=%v output=%s", err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("foreign deadlock propagation helper failed: %v\n%s", err, out)
			}
		})
	}
}

// TestExploreForeignBubbleSyncChurnRace is the RACE-BUILD arm of the
// membership nets: under -race the auto-instrumented access path and the
// sync-event recording sites are live while a foreign bubble churns. The log
// cleanliness assertions guard the resulting trace, but the membership mutants
// do not themselves have reachable log-producing doors, as explained below.
// Trace-byte equality is NOT asserted (the -race
// yield placement is foreign-sensitive; ForeignSched reports it) — the pins
// are the logs' cleanliness. The SUT is race-free so tRunner survives.
//
// The access-gate mutant has no log-producing door: race instrumentation omits
// nonescaping stack locals, while every instrumented foreign access is outside
// the current stack. dstAccessMaybeShared therefore returns true, seq 0 forces
// conservative pending mode, and an infrastructure-picked goroutine never
// commits that pending access. The membership guard remains structural defense;
// the access-log assertions below are nets, not a killing arm. The sync-event
// seq-zero fallback is likewise unreachable from foreign work after the sticky
// membership gate: a non-member returns before indexing, while dstEnsureSeq
// returns nonzero for every member. It remains defensive conservative handling
// for an internal invariant break, not a separately reachable foreign door.
func TestExploreForeignBubbleSyncChurnRace(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("requires -race (the instrumented access and sync-event doors)")
	}
	// foreignProgress advances only AFTER the foreign bubble has executed an
	// instrumented write and completed a rendezvous. The SUT waits for TWO
	// advances from its episode-start reading: a blocking rendezvous can
	// straddle the episode start (write before activation, wake and store
	// after), but the second advance's whole iteration — instrumented write
	// included — provably lies between two mid-episode stores. Mid-episode
	// foreign arrival at the recording surfaces is proven by causality, not
	// assumed from scheduling analysis.
	var foreignProgress atomic.Int64
	sut := func() bool {
		c0 := foreignProgress.Load()
		for foreignProgress.Load() < c0+2 {
			runtime.Gosched()
		}
		ch := make(chan int, 1)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch <- 1
		}()
		runtime.Gosched()
		wg.Wait()
		var wg2 sync.WaitGroup
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			runtime.Gosched()
		}()
		wg2.Wait()
		return <-ch == 1
	}
	var stop, started atomic.Bool
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		synctest.Run(func() {
			ch := make(chan int)
			go func() {
				for range ch {
				}
			}()
			// foreignChurnSink is a package-level variable written only by
			// this goroutine: the write is race-instrumented (locals whose
			// address never escapes are not) yet UNSHARED, so under a
			// membership regression it takes dstAccessShouldYield's early
			// false into the inline filtered commit — the exact door that
			// would put a seq-0 entry in the access log.
			for !stop.Load() {
				started.Store(true)
				foreignChurnSink++
				ch <- 1
				foreignProgress.Add(1) // after the write AND a rendezvous
				runtime.Gosched()
			}
			close(ch)
		})
	}()
	for !started.Load() {
		runtime.Gosched()
	}
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, sut)
	res := Explore(1, Exhaustive, sut)
	stop.Store(true)
	done.Wait()

	if !res.ForeignSched {
		t.Error("foreign bubble runnable at decisions was not reported")
	}
	for i, q := range tr.accSeq {
		if q == 0 {
			t.Errorf("access log entry %d has seq 0: a foreign instrumented access was recorded", i)
			break
		}
	}
	for i := range tr.edgeFrom {
		if tr.edgeFrom[i] == 0 || tr.edgeTo[i] == 0 {
			t.Errorf("edge %d has a zero endpoint: a foreign wake was buffered", i)
			break
		}
	}
	for i, q := range tr.syncSeq {
		if q == 0 {
			t.Errorf("sync event %d has seq 0: a foreign sync event was buffered", i)
			break
		}
	}
}

var foreignChurnSink int // keeps the race-variant churn's counter accesses live

// TestExploreForeignGCWorkloadInsensitive extends the foreign-invisibility
// contract to the workload class that used to break it, in BOTH build modes:
// a race-free finalizer/GC workload whose main blocks in runtime.GC() and on
// finalizer-driven channels. The GC assist paths temporarily nil gp.bubble
// ("disassociate"), and while the classification keyed on that live field an
// assist-parked simulation goroutine transiently became INFRASTRUCTURE —
// resumed RNG-free, foreign-reported (ForeignSched true with zero foreign
// work, Exhausted never claimable), and racing real foreign churn for the
// infra alternation slots, so two spinners shrank -race coverage (12
// schedules to 6) and diverged single-episode traces. With membership sticky
// (g.dstSimG), coverage and traces are churn-invariant and the foreign-free
// run claims exhaustion.
func TestExploreForeignGCWorkloadInsensitive(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool {
		ch1 := make(chan struct{})
		ch2 := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ch2
		}()
		func() {
			x := new(int)
			runtime.SetFinalizer(x, func(*int) {
				ch1 <- struct{}{}
				runtime.Gosched()
				ch2 <- struct{}{}
			})
		}()
		runtime.GC()
		<-ch1
		wg.Wait()
		return false
	}
	alone := Explore(1, Exhaustive, sut)
	_, _, trAlone := runOnce(1, nil, map[accessForce]bool{}, sut)
	if alone.ForeignSched || !alone.Exhausted {
		t.Fatalf("foreign-free GC workload misreported: foreignSched=%v exhausted=%v (an assist-parked simulation goroutine was classified foreign)", alone.ForeignSched, alone.Exhausted)
	}
	stop := make(chan struct{})
	var done sync.WaitGroup
	for i := 0; i < 2; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				runtime.Gosched()
			}
		}()
	}
	_, _, trSpun := runOnce(1, nil, map[accessForce]bool{}, sut)
	spun := Explore(1, Exhaustive, sut)
	close(stop)
	done.Wait()
	for name, tr := range map[string]exploreTrace{"alone": trAlone, "spun": trSpun} {
		for step, enabled := range tr.enabled {
			for i := 1; i < len(enabled); i++ {
				if enabled[i-1] >= enabled[i] {
					t.Fatalf("%s enabled set %d is not canonical: %v", name, step, enabled)
				}
			}
		}
	}
	if !spun.ForeignSched || spun.Exhausted {
		t.Fatalf("exploration under churn misreported: foreignSched=%v exhausted=%v", spun.ForeignSched, spun.Exhausted)
	}
	if spun.Schedules != alone.Schedules {
		t.Fatalf("foreign churn changed GC-workload coverage: %d vs %d schedules", spun.Schedules, alone.Schedules)
	}
	if !reflect.DeepEqual(trAlone.procs, trSpun.procs) || !reflect.DeepEqual(trAlone.enabled, trSpun.enabled) {
		t.Fatalf("foreign churn diverged the GC-workload trace:\nalone procs=%v enabled=%v\nspun  procs=%v enabled=%v",
			trAlone.procs, trAlone.enabled, trSpun.procs, trSpun.enabled)
	}
}

func TestExploreCoalescesSingletonStutter(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool {
		for range 8 {
			runtime.Gosched()
		}
		return false
	}
	Explore(1, Exhaustive, sut)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, sut)
	if len(tr.procs) != 1 || len(tr.enabled) != 1 || len(tr.enabled[0]) != 1 || tr.procs[0] != tr.enabled[0][0] {
		t.Fatalf("single-goroutine stutter was not coalesced to one attributed transition: procs=%v enabled=%v", tr.procs, tr.enabled)
	}
}

// TestExploreForeignPriorRootSpinner: simulation membership must not leak
// across runs through the ROOT — the goroutine that called Run carries the
// sticky membership bit for its run's duration, and the run teardown must
// clear it: a prior run's root later spinning as an ordinary foreign
// goroutine during a NEW exploration would otherwise be classified a
// simulation candidate of a run it is not part of (consuming seed draws,
// entering enabled sets with no assignable index).
func TestExploreForeignPriorRootSpinner(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool {
		ch := make(chan struct{}, 1)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ch
		}()
		ch <- struct{}{}
		runtime.Gosched()
		wg.Wait()
		return false
	}
	alone := Explore(1, Exhaustive, sut)
	_, _, trAlone := runOnce(1, nil, map[accessForce]bool{}, sut)

	// A goroutine that was a RUN ROOT and now spins as plain foreign work.
	ranRun := make(chan struct{})
	stop := make(chan struct{})
	spun := make(chan struct{})
	go func() {
		defer close(spun)
		Run(7, func() {})
		close(ranRun)
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.Gosched()
		}
	}()
	<-ranRun

	_, _, trSpun := runOnce(1, nil, map[accessForce]bool{}, sut)
	res := Explore(1, Exhaustive, sut)
	close(stop)
	<-spun

	if res.Schedules != alone.Schedules {
		t.Fatalf("a prior run's root perturbed exploration coverage: %d vs %d schedules", res.Schedules, alone.Schedules)
	}
	if !res.ForeignSched {
		t.Error("the prior root spinning at decisions was not reported as foreign")
	}
	assertUniqueEnabledSeqs(t, trSpun)
	if !reflect.DeepEqual(trAlone.procs, trSpun.procs) || !reflect.DeepEqual(trAlone.enabled, trSpun.enabled) {
		t.Fatalf("a prior run's root is visible in the recorded schedule")
	}
}

// TestExploreForeignSpinnerDrainCallback pins the drain-displacement leg of
// the starvation fairness: the bubble's finalizer drain is infrastructure
// under the scheduled strategy but has sim-visible effects, so it must run at
// the same logical points with foreign churn as without. A finalizer that
// sends and then calls Gosched re-queues the drain's continuation at the
// global-runq tail — BEHIND a parked foreign spinner — where an
// unprioritized infra pick would run the spinner first and shift every later
// enabled set. The drain outranks foreign infrastructure and is transparent
// to the alternation, so the recorded traces must be identical.
func TestExploreForeignSpinnerDrainCallback(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if dstRaceEnabledFP() {
		// The dst-race auto-instrumentation's yield placement is
		// foreign-sensitive (ForeignSched reports it), so exact trace
		// equality holds only without it; the -race behavior is covered by
		// TestExploreForeignSchedReported.
		t.Skip("non-race trace regression")
	}
	var mainRan atomic.Bool
	var drainInterrupted atomic.Bool
	sut := func() bool {
		mainRan.Store(false)
		ch1 := make(chan struct{}, 1)
		ch2 := make(chan struct{}, 1)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ch2
		}()
		func() {
			x := new(int)
			runtime.SetFinalizer(x, func(*int) {
				// Wake ONE simulation goroutine, yield (re-queuing the drain
				// at the global tail, behind any foreign spinner), then wake
				// the second: if foreign work displaces the drain's
				// continuation, the first sim decision sees enabled {main}
				// instead of {main, helper} and the traces diverge.
				ch1 <- struct{}{}
				runtime.Gosched()
				if mainRan.Load() {
					drainInterrupted.Store(true)
				}
				ch2 <- struct{}{}
			})
		}()
		runtime.GC()
		<-ch1
		mainRan.Store(true)
		wg.Wait()
		return false
	}
	Explore(1, Exhaustive, sut) // also initializes the trace buffers
	_, _, trAlone := runOnce(1, nil, map[accessForce]bool{}, sut)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.Gosched()
		}
	}()
	_, _, trSpun := runOnce(1, nil, map[accessForce]bool{}, sut)
	close(stop)
	<-done
	assertUniqueEnabledSeqs(t, trSpun)
	if !reflect.DeepEqual(trAlone.procs, trSpun.procs) || !reflect.DeepEqual(trAlone.enabled, trSpun.enabled) {
		t.Fatalf("foreign spinner displaced the drain in the recorded schedule:\nalone procs=%v enabled=%v\nspun  procs=%v enabled=%v",
			trAlone.procs, trAlone.enabled, trSpun.procs, trSpun.enabled)
	}
	if drainInterrupted.Load() {
		t.Fatal("a woken simulation goroutine ran between the drain callback's two halves")
	}
	// Drain atomicity between its yields: the drain must not be interrupted
	// by the fairness hand-off, so the callback's two wakes land before either
	// woken goroutine runs. Consecutive singleton no-choice selections coalesce
	// after their first transition; the startup and post-callback decisions must
	// both contain main and helper. If the drain is interrupted after waking
	// main, main runs and blocks before helper is woken, so no second joint set
	// appears.
	contains := func(e []uint64, s uint64) bool {
		for _, v := range e {
			if v == s {
				return true
			}
		}
		return false
	}
	mainSeq := trAlone.procs[0]
	var helperSeq uint64
	for _, e := range trAlone.enabled {
		if len(e) >= 2 {
			for _, s := range e {
				if s != mainSeq {
					helperSeq = s
				}
			}
			break
		}
	}
	if helperSeq == 0 {
		t.Fatalf("no multi-candidate startup decision found: enabled=%v", trAlone.enabled)
	}
	joint := 0
	for _, e := range trAlone.enabled {
		if contains(e, mainSeq) && contains(e, helperSeq) {
			joint++
		}
	}
	if joint < 2 {
		t.Fatalf("drain tail did not wake main and helper before either ran: enabled=%v", trAlone.enabled)
	}
}

// TestExploreForeignSchedReported: exploration under foreign churn reports
// the churn and downgrades its exhaustion claim — coverage under foreign
// activity is best-effort, never a silent claim (the dst-race yield
// placement is demonstrably foreign-sensitive: fewer schedules explored with
// a spinner than without under -race). Race-free sut, so this runs under
// -race too — the one configuration where the sensitivity is live.
func TestExploreForeignSchedReported(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool {
		ch := make(chan struct{}, 1)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ch
		}()
		ch <- struct{}{}
		wg.Wait()
		return false
	}
	alone := Explore(1, Exhaustive, sut)
	if alone.ForeignSched || !alone.Exhausted {
		t.Fatalf("foreign-free exploration misreported: foreignSched=%v exhausted=%v", alone.ForeignSched, alone.Exhausted)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.Gosched()
		}
	}()
	spun := Explore(1, Exhaustive, sut)
	close(stop)
	<-done
	if !spun.ForeignSched {
		t.Fatalf("foreign goroutine runnable at simulation decisions was not reported: %+v", spun)
	}
	if spun.Exhausted {
		t.Fatalf("exhaustion claimed under foreign churn (coverage is best-effort there): %+v", spun)
	}
}

func TestExploreRaceAttributionRequiresForeignFreeRun(t *testing.T) {
	var foreign ExploreResult
	appendRunFailures(&foreign, []uint64{1}, nil, runResult{
		raceCount: 2,
		tr:        exploreTrace{foreignSched: true},
	})
	if foreign.UnattributedRaces != 2 || len(foreign.Failures) != 0 {
		t.Fatalf("foreign race aggregation = unattributed %d, failures %v", foreign.UnattributedRaces, foreign.Failures)
	}

	var isolated ExploreResult
	appendRunFailures(&isolated, []uint64{1}, nil, runResult{raceCount: 2})
	if isolated.UnattributedRaces != 0 || len(isolated.Failures) != 2 || !isolated.Failures[0].Race || !isolated.Failures[1].Race {
		t.Fatalf("isolated race aggregation = unattributed %d, failures %v", isolated.UnattributedRaces, isolated.Failures)
	}
}

func TestExploreUnattributedRacesAccumulateAcrossPasses(t *testing.T) {
	total, unattributed := 0, 0
	foreign := false
	first := ExploreResult{Schedules: 2, ForeignSched: true, UnattributedRaces: 3}
	mergeExplorePass(&first, &total, &foreign, &unattributed)
	second := ExploreResult{Schedules: 4, UnattributedRaces: 2}
	mergeExplorePass(&second, &total, &foreign, &unattributed)
	if second.Schedules != 6 || !second.ForeignSched || second.UnattributedRaces != 5 {
		t.Fatalf("merged pass = schedules %d, foreign %v, unattributed %d", second.Schedules, second.ForeignSched, second.UnattributedRaces)
	}
	budget := exploreBudgetResult(total, nil, foreign, unattributed)
	if !budget.BudgetHit || budget.UnattributedRaces != 5 {
		t.Fatalf("budget result = %+v", budget)
	}
}

func TestExploreResetsScheduledIdentityAcrossRuns(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			dstYieldPoint()
		}()
		time.Sleep(time.Millisecond)
		wg.Wait()
		return false
	}
	for i := 0; i < 3; i++ {
		_, _, tr := runOnce(1, nil, map[accessForce]bool{}, sut)
		assertUniqueEnabledSeqs(t, tr)
		if seq := dstCurrentSeqFP(); seq != 0 {
			t.Fatalf("run %d left scheduled identity %d on the reused synctest root", i, seq)
		}
	}
}

func TestPublicExploreGuardsBeforeTraceInit(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	t.Cleanup(func() {
		dstExploreInit(exploreMaxDecisions, exploreMaxEnabledTotal, exploreMaxEdges, exploreMaxAccesses)
	})

	for _, tt := range []struct {
		name string
		call func()
	}{
		{
			name: "ExploreWith",
			call: func() {
				ExploreWith(1, ExploreOptions{MaxSchedules: 1}, func() bool { return false })
			},
		},
		{
			name: "Replay",
			call: func() {
				Replay(1, Failure{}, func() bool { return false })
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dstExploreInit(0, 0, 0, 0)
			panicked := false
			// Direct store, deliberately bypassing callerGate's write side: this
			// constructs the overlap precondition with no concurrent guarded ops
			// in flight, so the gate's exclusion invariant is not in play.
			runActive.Store(true)
			func() {
				defer func() {
					runActive.Store(false)
					if v := recover(); v != nil {
						panicked = strings.Contains(panicString(v), "called while another simulation operation is active")
					}
				}()
				tt.call()
			}()
			if !panicked {
				t.Fatalf("%s did not reject overlap", tt.name)
			}

			x := 0
			_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
				dstAccessYield(unsafe.Pointer(&x), true)
				x++
				return false
			})
			if !tr.overflow {
				t.Fatalf("%s mutated trace buffers before rejecting overlap", tt.name)
			}
		})
	}
}

func TestRunWithRejectsInvalidOptionsBeforeActivation(t *testing.T) {
	type testCase struct {
		name string
		opts Options
		want string
	}
	cases := []testCase{
		{
			name: "unknown strategy",
			opts: Options{Strategy: Strategy(99)},
			want: "unknown Strategy",
		},
	}
	if strconv.IntSize > 32 {
		tooLarge := int(maxStrategyParam)
		tooLarge++
		cases = append(cases,
			testCase{
				name: "pct depth overflow",
				opts: Options{Strategy: PCT, Depth: tooLarge},
				want: "PCT Depth overflows",
			},
			testCase{
				name: "pct steps overflow",
				opts: Options{Strategy: PCT, Steps: tooLarge},
				want: "PCT Steps overflows",
			},
		)
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			var got string
			func() {
				defer func() {
					got = panicString(recover())
				}()
				RunWith(1, tt.opts, func() { called = true })
			}()
			if !strings.Contains(got, tt.want) {
				t.Fatalf("RunWith panic = %q, want substring %q", got, tt.want)
			}
			if called {
				t.Fatalf("RunWith called the SUT after rejecting %s", tt.name)
			}
			if runActive.Load() {
				t.Fatalf("RunWith left simulation active after rejecting %s", tt.name)
			}
		})
	}
}

func TestRunFatalExitsCaller(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	const helperEnv = "GO_WANT_SIMULATION_FATAL_HELPER=1"
	if os.Getenv("GO_WANT_SIMULATION_FATAL_HELPER") == "1" {
		Run(1, func() {
			t.Fatal("fatal inside simulation")
		})
		t.Fatal("simulation.Run returned after Fatal")
		return
	}

	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestRunFatalExitsCaller$", "-test.count=1")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, helperEnv)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper test passed unexpectedly:\n%s", out)
	}
	if !strings.Contains(string(out), "fatal inside simulation") {
		t.Fatalf("helper output missing simulation fatal:\n%s", out)
	}
	if strings.Contains(string(out), "panic:") {
		t.Fatalf("simulation.Run aborted by panic, want testing Goexit:\n%s", out)
	}
	if strings.Contains(string(out), "simulation.Run returned after Fatal") {
		t.Fatalf("simulation.Run returned after Fatal:\n%s", out)
	}
}

// TestRunRejectsFIPSMode verifies the FIPS guard: under GODEBUG=fips140=on,
// crypto/rand routes through the process-global FIPS DRBG, which the
// simulation cannot make deterministic — entering a simulation must fail loud
// instead of running with silently nondeterministic crypto/rand.
func TestRunRejectsFIPSMode(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	const helperEnv = "GO_WANT_SIMULATION_FIPS_HELPER"
	if os.Getenv(helperEnv) == "1" {
		defer func() {
			if v := recover(); v != nil && strings.Contains(panicString(v), "unsupported in FIPS 140 mode") {
				os.Stdout.WriteString("fips-rejected\n")
				os.Exit(0)
			}
			os.Exit(1)
		}()
		Run(1, func() {})
		os.Exit(1)
		return
	}

	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestRunRejectsFIPSMode$", "-test.count=1")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, helperEnv+"=1", "GODEBUG=fips140=on")
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "fips-rejected") {
		t.Fatalf("Run under GODEBUG=fips140=on was not rejected:\n%s", out)
	}
}

// TestTestWithChainAbortPropagates pins TestWith's chain-abort semantics: a
// FailNow on an ANCESTOR T from inside the simulation child aborts the whole
// subtest chain (the runtime.Goexit is re-issued to TestWith's caller, like
// nested t.Run), so code after the enclosing t.Run in the root must NOT run.
// This is deliberately stronger than testing/synctest.Test, whose ok=false
// path lets the root continue; the design doc records the difference.
//
// Teeth: with runLocked's propagateGoexit flipped to false for TestWith, the
// abort degenerates to FailNow on the subtest and the root continues — the
// helper prints the after-run marker and this test fails.
func TestTestWithChainAbortPropagates(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	const helperEnv = "GO_WANT_SIMULATION_CHAIN_ABORT_HELPER"
	if os.Getenv(helperEnv) == "1" {
		root := t
		t.Run("sub", func(sub *testing.T) {
			TestWith(sub, 1, Options{}, func(*testing.T) {
				root.FailNow() // grandparent abort from inside the simulation
			})
		})
		os.Stdout.WriteString("after-run-executed\n")
		return
	}

	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestTestWithChainAbortPropagates$", "-test.count=1")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, helperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper passed unexpectedly:\n%s", out)
	}
	if strings.Contains(string(out), "after-run-executed") {
		t.Fatalf("grandparent FailNow did not abort the chain (root continued past t.Run):\n%s", out)
	}
	// Positive evidence of a CLEAN abort: the helper test fails through the
	// testing framework, not through a runtime crash or timeout.
	if !strings.Contains(string(out), "--- FAIL: TestTestWithChainAbortPropagates") ||
		strings.Contains(string(out), "fatal error:") ||
		strings.Contains(string(out), "panic:") {
		t.Fatalf("chain abort did not fail cleanly through the testing framework:\n%s", out)
	}
}

// TestTestPanicsDuringCleanup verifies the reentry panic names the simulation
// API when Test is called from a t.Cleanup function.
func TestTestPanicsDuringCleanup(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	t.Run("sub", func(sub *testing.T) {
		sub.Cleanup(func() {
			var got string
			func() {
				defer func() { got = panicString(recover()) }()
				Test(sub, 1, func(*testing.T) {})
			}()
			if !strings.Contains(got, "testing/simulation: TestWith called during t.Cleanup") {
				t.Errorf("cleanup-reentry panic = %q, want it to name the simulation API", got)
			}
		})
	})
}

func TestTestFatalExitsCaller(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	const helperEnv = "GO_WANT_SIMULATION_TEST_FATAL_HELPER=1"
	if os.Getenv("GO_WANT_SIMULATION_TEST_FATAL_HELPER") == "1" {
		Test(t, 1, func(t *testing.T) {
			t.Fatal("fatal inside simulation test")
		})
		t.Fatal("simulation.Test returned after Fatal")
		return
	}

	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestTestFatalExitsCaller$", "-test.count=1")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, helperEnv)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper test passed unexpectedly:\n%s", out)
	}
	if !strings.Contains(string(out), "fatal inside simulation test") {
		t.Fatalf("helper output missing simulation Test fatal:\n%s", out)
	}
	if strings.Contains(string(out), "panic:") {
		t.Fatalf("simulation.Test aborted by panic, want testing Goexit:\n%s", out)
	}
	if strings.Contains(string(out), "simulation.Test returned after Fatal") {
		t.Fatalf("simulation.Test returned after Fatal:\n%s", out)
	}
}

func TestTestProvidesBubbleScopedT(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	cleanupDone := make(chan struct{}, 1)
	contextDone := make(chan struct{}, 1)
	Test(t, 1, func(t *testing.T) {
		cleanupCh := make(chan struct{})
		t.Cleanup(func() {
			close(cleanupCh)
		})
		go func() {
			<-cleanupCh
			cleanupDone <- struct{}{}
		}()
		go func() {
			<-t.Context().Done()
			contextDone <- struct{}{}
		}()
	})
	select {
	case <-cleanupDone:
	default:
		t.Fatalf("simulation.Test cleanup did not run inside the bubble")
	}
	select {
	case <-contextDone:
	default:
		t.Fatalf("simulation.Test context was not canceled before returning")
	}
}

func TestTestWithOptions(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	TestWith(t, 1, Options{Hostname: "sim-test", PID: 123, NumCPU: 2}, func(t *testing.T) {
		hostname, err := os.Hostname()
		if err != nil {
			t.Fatal(err)
		}
		if hostname != "sim-test" {
			t.Fatalf("os.Hostname() = %q, want sim-test", hostname)
		}
		if pid := os.Getpid(); pid != 123 {
			t.Fatalf("os.Getpid() = %d, want 123", pid)
		}
		if numCPU := runtime.NumCPU(); numCPU != 2 {
			t.Fatalf("runtime.NumCPU() = %d, want 2", numCPU)
		}
	})
}

func TestTestWithRejectsInvalidOptionsBeforeActivation(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "unknown strategy",
			opts: Options{Strategy: Strategy(99)},
			want: "TestWith unknown Strategy",
		},
	}
	if strconv.IntSize > 32 {
		tooLarge := int(maxStrategyParam)
		tooLarge++
		cases = append(cases,
			struct {
				name string
				opts Options
				want string
			}{name: "pct depth overflow", opts: Options{Strategy: PCT, Depth: tooLarge}, want: "TestWith PCT Depth overflows"},
			struct {
				name string
				opts Options
				want string
			}{name: "pct steps overflow", opts: Options{Strategy: PCT, Steps: tooLarge}, want: "TestWith PCT Steps overflows"},
		)
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			var got string
			func() {
				defer func() {
					got = panicString(recover())
				}()
				TestWith(t, 1, tt.opts, func(*testing.T) { called = true })
			}()
			if !strings.Contains(got, tt.want) {
				t.Fatalf("TestWith panic = %q, want substring %q", got, tt.want)
			}
			if called {
				t.Fatalf("TestWith called the SUT after rejecting %s", tt.name)
			}
			if runActive.Load() {
				t.Fatalf("TestWith left simulation active after rejecting %s", tt.name)
			}
		})
	}
}

func TestExploreClocksModelStepZeroAccesses(t *testing.T) {
	tr := exploreTrace{
		accSeq:   []uint64{1, 2},
		accAddr:  []uintptr{0x1000, 0x1000},
		accWrite: []bool{true, false},
		accStep:  []int{0, 0},
	}
	clk, pidx := dporClocks(tr)
	if len(clk[0]) == 0 || len(clk[1]) == 0 {
		t.Fatalf("step-0 accesses were not clocked: %#v", clk)
	}
	if !dporConcurrent(clk, pidx, tr, 0, 1) {
		t.Fatalf("independent step-0 goroutines should be concurrent: %#v", clk)
	}

	traceClk, tracePidx := dporTraceClocks(tr)
	if dporConcurrent(traceClk, tracePidx, tr, 0, 1) {
		t.Fatalf("trace clocks did not order step-0 conflicting accesses: %#v", traceClk)
	}
}

func TestExploreRangeOverlapConflicts(t *testing.T) {
	tr := exploreTrace{
		accSeq:   []uint64{1, 2},
		accAddr:  []uintptr{0x1000, 0x1008},
		accSize:  []uintptr{16, 1},
		accPC:    []uintptr{1, 2},
		accCount: []uint64{1, 1},
		accWrite: []bool{true, false},
		accStep:  []int{0, 0},
	}
	clk, pidx := dporClocks(tr)
	if !dporConcurrent(clk, pidx, tr, 0, 1) {
		t.Fatalf("sync clocks should leave overlapping accesses concurrent: %#v", clk)
	}
	traceClk, tracePidx := dporTraceClocks(tr)
	if dporConcurrent(traceClk, tracePidx, tr, 0, 1) {
		t.Fatalf("trace clocks did not order overlapping range/scalar conflict: %#v", traceClk)
	}
	forces := map[accessForce]bool{}
	if !promoteAccessForces(tr, forces) {
		t.Fatalf("overlapping range/scalar conflict did not promote replay boundaries")
	}
	if len(forces) != 2 {
		t.Fatalf("overlapping conflict promoted %d forces, want 2: %#v", len(forces), forces)
	}
}

func TestExploreRangeAdjacentDoesNotConflict(t *testing.T) {
	tr := exploreTrace{
		accSeq:   []uint64{1, 2},
		accAddr:  []uintptr{0x1000, 0x1010},
		accSize:  []uintptr{16, 1},
		accPC:    []uintptr{1, 2},
		accCount: []uint64{1, 1},
		accWrite: []bool{true, false},
		accStep:  []int{0, 0},
	}
	traceClk, tracePidx := dporTraceClocks(tr)
	if !dporConcurrent(traceClk, tracePidx, tr, 0, 1) {
		t.Fatalf("adjacent range/scalar accesses should remain independent: %#v", traceClk)
	}
	forces := map[accessForce]bool{}
	if promoteAccessForces(tr, forces) {
		t.Fatalf("adjacent range/scalar accesses promoted replay boundaries: %#v", forces)
	}
}

func TestExploreClocksOrderSameStepEdgesAgainstAccesses(t *testing.T) {
	tr := exploreTrace{
		accSeq:   []uint64{1, 2},
		accAddr:  []uintptr{0x1000, 0x2000},
		accWrite: []bool{true, false},
		accStep:  []int{1, 2},
		edgeFrom: []uint64{1},
		edgeTo:   []uint64{2},
		edgeStep: []int{1},
		edgeAcc:  []int{0},
	}
	clk, pidx := dporClocks(tr)
	if !dporConcurrent(clk, pidx, tr, 0, 1) {
		t.Fatalf("edge before same-step parent access incorrectly ordered that access before child: %#v", clk)
	}
	traceClk, tracePidx := dporTraceClocks(tr)
	if !dporConcurrent(traceClk, tracePidx, tr, 0, 1) {
		t.Fatalf("trace edge before same-step parent access incorrectly ordered that access before child: %#v", traceClk)
	}

	tr.edgeAcc[0] = 1
	clk, pidx = dporClocks(tr)
	if dporConcurrent(clk, pidx, tr, 0, 1) {
		t.Fatalf("edge after same-step parent access did not order that access before child: %#v", clk)
	}
}

func TestExploreClocksModelSyncReleaseAcquire(t *testing.T) {
	tr := exploreTrace{
		accSeq:    []uint64{1, 2},
		accAddr:   []uintptr{0x1000, 0x1000},
		accWrite:  []bool{true, false},
		accStep:   []int{1, 3},
		syncKind:  []uint8{syncEventRelease, syncEventAcquire},
		syncID:    []uintptr{0x2000, 0x2000},
		syncSeq:   []uint64{1, 2},
		syncStep:  []int{2, 3},
		syncAcc:   []int{1, 1},
		syncOrd:   []int{1, 2},
		edgeOrder: nil,
	}
	clk, pidx := dporClocks(tr)
	if dporConcurrent(clk, pidx, tr, 0, 1) {
		t.Fatalf("release/acquire HB did not order protected accesses: %#v", clk)
	}
}

func TestExploreClocksUseReleaseSnapshot(t *testing.T) {
	tr := exploreTrace{
		accSeq:   []uint64{1, 1, 2},
		accAddr:  []uintptr{0x1000, 0x2000, 0x2000},
		accWrite: []bool{true, true, false},
		accStep:  []int{1, 3, 5},
		syncKind: []uint8{syncEventRelease, syncEventAcquire},
		syncID:   []uintptr{0x3000, 0x3000},
		syncSeq:  []uint64{1, 2},
		syncStep: []int{2, 4},
		syncAcc:  []int{1, 2},
		syncOrd:  []int{1, 2},
	}
	clk, pidx := dporClocks(tr)
	if dporConcurrent(clk, pidx, tr, 0, 2) {
		t.Fatalf("release snapshot did not carry pre-release access to acquire: %#v", clk)
	}
	if !dporConcurrent(clk, pidx, tr, 1, 2) {
		t.Fatalf("acquire incorrectly observed releaser's post-release access: %#v", clk)
	}
}

func TestExploreClocksDistinguishSyncObjectAux(t *testing.T) {
	tr := exploreTrace{
		accSeq:   []uint64{1, 2},
		accAddr:  []uintptr{0x1000, 0x1000},
		accWrite: []bool{true, false},
		accStep:  []int{1, 3},
		syncKind: []uint8{syncEventRelease, syncEventAcquire},
		syncID:   []uintptr{0x2000, 0x2000},
		syncAux:  []uintptr{2, 1},
		syncSeq:  []uint64{1, 2},
		syncStep: []int{2, 3},
		syncAcc:  []int{1, 1},
		syncOrd:  []int{1, 2},
	}
	clk, pidx := dporClocks(tr)
	if !dporConcurrent(clk, pidx, tr, 0, 1) {
		t.Fatalf("sync events with the same id but different aux were incorrectly ordered: %#v", clk)
	}
}

func TestExploreRecordsCreateHBEdge(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	dstExploreInit(64, 64, 64, 0)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
		}()
		wg.Wait()
		return false
	})
	for i := range tr.edgeFrom {
		for j := range tr.edgeFrom {
			if tr.edgeFrom[i] == tr.edgeTo[j] && tr.edgeTo[i] == tr.edgeFrom[j] && tr.edgeStep[i] < tr.edgeStep[j] {
				return
			}
		}
	}
	t.Fatalf("goroutine creation did not record a parent->child HB edge before child wake: steps=%v acc=%v from=%v to=%v", tr.edgeStep, tr.edgeAcc, tr.edgeFrom, tr.edgeTo)
}

func TestExploreRecordsBufferedChannelHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("buffered channel HB events are emitted by dst-race sync hooks")
	}
	x := new(int)
	addr := uintptr(unsafe.Pointer(x))
	dstExploreInit(128, 512, 128, 512)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		ch := make(chan struct{}, 1)
		var wg sync.WaitGroup
		dstAccessYield(unsafe.Pointer(x), true)
		*x = 1
		ch <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ch
			dstAccessYield(unsafe.Pointer(x), false)
			_ = *x
		}()
		wg.Wait()
		return false
	})
	release, acquire := false, false
	for _, k := range tr.syncKind {
		release = release || k == syncEventRelease
		acquire = acquire || k == syncEventAcquire
	}
	if !release || !acquire {
		t.Fatalf("buffered channel did not record sync release/acquire events: kind=%v", tr.syncKind)
	}
	write, read := -1, -1
	for i := range tr.accSeq {
		if tr.accAddr[i] != addr {
			continue
		}
		if tr.accWrite[i] && write < 0 {
			write = i
		}
		if !tr.accWrite[i] {
			read = i
		}
	}
	if write < 0 || read < 0 || tr.accSeq[write] == tr.accSeq[read] {
		t.Fatalf("missing buffered-channel protected access pair: write=%d read=%d seq=%v addr=%#x log=%#v", write, read, tr.accSeq, addr, tr)
	}
	clk, pidx := dporClocks(tr)
	if dporConcurrent(clk, pidx, tr, write, read) {
		t.Fatalf("buffered channel send/receive HB did not order protected accesses: write=%d read=%d sync=%#v", write, read, tr.syncKind)
	}
}

func TestExploreRecordsBufferedChannelZeroSizeSlotIDs(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("buffered channel HB events are emitted by dst-race sync hooks")
	}
	dstExploreInit(128, 512, 128, 512)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		ch := make(chan struct{}, 2)
		ch <- struct{}{}
		ch <- struct{}{}
		<-ch
		return false
	})
	releases := map[syncObjectKey]bool{}
	for i, k := range tr.syncKind {
		if k == syncEventRelease && tr.syncAux[i] != 0 {
			releases[syncObjectKey{id: tr.syncID[i], aux: tr.syncAux[i]}] = true
		}
	}
	if len(releases) < 2 {
		t.Fatalf("zero-sized buffered channel slots were not distinct sync objects: releases=%v ids=%v aux=%v kind=%v", releases, tr.syncID, tr.syncAux, tr.syncKind)
	}
}

// TestExploreRendezvousAndCloseDistinctSyncObjects verifies the unbuffered
// rendezvous and channel close record HB events under DISTINCT sync-object
// keys (different aux on the same channel id), mirroring TSan, which keys the
// rendezvous on chanbuf(c,0) and close on c.raceaddr(). Sharing a key would
// accumulate a rendezvous-participant -> later-closed-receiver edge the memory
// model does not order.
func TestExploreRendezvousAndCloseDistinctSyncObjects(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("channel HB events are emitted by dst-race sync hooks")
	}
	dstExploreInit(256, 1024, 256, 1024)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		ch := make(chan int)
		done := make(chan struct{})
		go func() {
			ch <- 1
			close(done)
		}()
		<-ch
		<-done
		close(ch)
		<-ch // closed receive
		return false
	})
	auxByKindRendezvous := false
	auxZeroRelease := false
	for i := range tr.syncKind {
		if tr.syncAux[i] == ^uintptr(0) {
			auxByKindRendezvous = true
		}
		if tr.syncKind[i] == syncEventRelease && tr.syncAux[i] == 0 {
			auxZeroRelease = true
		}
	}
	if !auxByKindRendezvous || !auxZeroRelease {
		t.Fatalf("rendezvous and close events not keyed as distinct sync objects: aux=%v kind=%v",
			tr.syncAux, tr.syncKind)
	}
}

// TestExploreRecordsBufferedSlotReuseEdges verifies buffered channel
// operations mirror racereleaseacquire on the slot: a receive also RELEASES
// the slot and a send also ACQUIRES it, giving DPOR the k'th-receive ->
// (k+C)'th-send edge of the memory model (missing it only over-explores, but
// over-exploration burns schedule budget).
func TestExploreRecordsBufferedSlotReuseEdges(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("buffered channel HB events are emitted by dst-race sync hooks")
	}
	dstExploreInit(256, 1024, 256, 1024)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		ch := make(chan int, 1)
		ch <- 1
		<-ch
		ch <- 2
		<-ch
		return false
	})
	slotReleases, slotAcquires := 0, 0
	for i := range tr.syncKind {
		if tr.syncAux[i] == 0 || tr.syncAux[i] == ^uintptr(0) {
			continue
		}
		switch tr.syncKind[i] {
		case syncEventRelease:
			slotReleases++
		case syncEventAcquire:
			slotAcquires++
		}
	}
	// Two sends + two receives, each recording both directions on slot 1:
	// four releases and four acquires. Without the reuse edges, only the two
	// send-releases and two receive-acquires exist.
	if slotReleases < 4 || slotAcquires < 4 {
		t.Fatalf("buffered slot-reuse edges missing: releases=%d acquires=%d aux=%v kind=%v",
			slotReleases, slotAcquires, tr.syncAux, tr.syncKind)
	}
}

func TestExploreRecordsFullBufferedChannelSenderRelease(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("buffered channel HB events are emitted by dst-race sync hooks")
	}
	x := new(int)
	addr := uintptr(unsafe.Pointer(x))
	sut := func() bool {
		ch := make(chan int, 1)
		ready := make(chan struct{})
		ch <- 0
		go func() {
			dstAccessYield(unsafe.Pointer(x), true)
			*x = 1
			ready <- struct{}{}
			ch <- 1
		}()
		<-ready
		time.Sleep(time.Nanosecond)
		if got := <-ch; got != 0 {
			t.Fatalf("first receive got %d, want initial buffered value", got)
		}
		if got := <-ch; got != 1 {
			t.Fatalf("second receive got %d, want blocked sender value", got)
		}
		return false
	}
	dstExploreInit(128, 512, 128, 512)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, sut)
	senderSeq := uint64(0)
	for i := range tr.accSeq {
		if tr.accAddr[i] == addr && tr.accWrite[i] {
			senderSeq = tr.accSeq[i]
			break
		}
	}
	if senderSeq == 0 {
		t.Fatalf("missing sender access in trace: %#v", tr)
	}
	for i, k := range tr.syncKind {
		// Only buffered SLOT releases (aux slot+1): close is aux 0 and the
		// unbuffered rendezvous carries the dedicated rendezvous key.
		if k != syncEventRelease || tr.syncSeq[i] != senderSeq || tr.syncAux[i] == 0 || tr.syncAux[i] == ^uintptr(0) {
			continue
		}
		step := tr.syncStep[i]
		if step > 0 && step-1 < len(tr.procs) && tr.procs[step-1] != senderSeq {
			return
		}
	}
	t.Fatalf("full-buffer receive did not record blocked sender release from receiver step: sender=%d procs=%v syncSeq=%v syncStep=%v syncAux=%v kind=%v", senderSeq, tr.procs, tr.syncSeq, tr.syncStep, tr.syncAux, tr.syncKind)
}

func TestExploreRecordsSelectBufferedChannelHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("buffered channel HB events are emitted by dst-race sync hooks")
	}
	dstExploreInit(128, 512, 128, 512)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		ch := make(chan int, 1)
		other := make(chan int)
		select {
		case ch <- 1:
		case other <- 1:
		}
		select {
		case <-ch:
		case <-other:
		}
		return false
	})
	release := map[syncObjectKey]bool{}
	for i, k := range tr.syncKind {
		if tr.syncAux[i] == 0 {
			continue
		}
		obj := syncObjectKey{id: tr.syncID[i], aux: tr.syncAux[i]}
		if k == syncEventRelease {
			release[obj] = true
		}
		if k == syncEventAcquire && release[obj] {
			return
		}
	}
	t.Fatalf("select buffered send/receive did not record matching release/acquire: id=%v aux=%v kind=%v", tr.syncID, tr.syncAux, tr.syncKind)
}

func TestExploreRecordsUnbufferedChannelHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("unbuffered channel HB events are emitted by dst-race sync hooks")
	}
	for _, tt := range []struct {
		name string
		sut  func(unsafe.Pointer) func() bool
	}{
		{
			name: "SendToReceive",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					ch := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						<-ch
						dstAccessYield(marker, false)
					}()
					go func() {
						defer wg.Done()
						runtime.Gosched()
						dstAccessYield(marker, true)
						ch <- struct{}{}
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "ReceiveToSendComplete",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					ch := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						ch <- struct{}{}
						dstAccessYield(marker, false)
					}()
					go func() {
						defer wg.Done()
						runtime.Gosched()
						dstAccessYield(marker, true)
						<-ch
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "ParkedSenderToReceive",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					ch := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						dstAccessYield(marker, true)
						ch <- struct{}{}
					}()
					go func() {
						defer wg.Done()
						runtime.Gosched()
						<-ch
						dstAccessYield(marker, false)
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "ParkedReceiverToSendComplete",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					ch := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						dstAccessYield(marker, true)
						<-ch
					}()
					go func() {
						defer wg.Done()
						runtime.Gosched()
						ch <- struct{}{}
						dstAccessYield(marker, false)
					}()
					wg.Wait()
					return false
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr, _, readSeq := assertChannelHBTrace(t, tt.sut)
			releases, acquires := 0, 0
			readAcquire := false
			for i, k := range tr.syncKind {
				// Rendezvous events carry the dedicated rendezvous key,
				// distinct from close (aux 0) and buffered slots (slot+1).
				if tr.syncAux[i] != ^uintptr(0) {
					continue
				}
				switch k {
				case syncEventRelease:
					releases++
				case syncEventAcquire:
					acquires++
					readAcquire = readAcquire || tr.syncSeq[i] == readSeq
				}
			}
			if releases < 2 || acquires < 2 {
				t.Fatalf("unbuffered channel rendezvous did not record both racesync halves: releases=%d acquires=%d syncKind=%v syncAux=%v", releases, acquires, tr.syncKind, tr.syncAux)
			}
			if !readAcquire {
				t.Fatalf("unbuffered channel rendezvous did not record acquire on parked goroutine %d: syncKind=%v syncSeq=%v syncAux=%v", readSeq, tr.syncKind, tr.syncSeq, tr.syncAux)
			}
		})
	}
}

func TestExploreRecordsChannelCloseHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("channel close HB events are emitted by dst-race sync hooks")
	}
	for _, tt := range []struct {
		name string
		sut  func(unsafe.Pointer) func() bool
	}{
		{
			name: "ReceiveAfterClose",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					ch := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						dstAccessYield(marker, true)
						close(ch)
					}()
					go func() {
						defer wg.Done()
						<-ch
						dstAccessYield(marker, false)
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "SelectReceiveAfterClose",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					ch := make(chan struct{})
					other := make(chan struct{})
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						dstAccessYield(marker, true)
						close(ch)
					}()
					go func() {
						defer wg.Done()
						select {
						case <-ch:
						case <-other:
						}
						dstAccessYield(marker, false)
					}()
					wg.Wait()
					return false
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertChannelHBTrace(t, tt.sut)
		})
	}
}

func assertChannelHBTrace(t *testing.T, sutForMarker func(unsafe.Pointer) func() bool) (exploreTrace, uint64, uint64) {
	t.Helper()
	marker := new(int)
	addr := uintptr(unsafe.Pointer(marker))
	sut := sutForMarker(unsafe.Pointer(marker))
	dstExploreInit(256, 4096, 512, 512)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, sut)
	write, read := -1, -1
	for i := range tr.accSeq {
		if tr.accAddr[i] != addr {
			continue
		}
		if tr.accWrite[i] {
			write = i
		} else {
			read = i
		}
	}
	if write < 0 || read < 0 || tr.accSeq[write] == tr.accSeq[read] || write > read {
		t.Fatalf("missing ordered channel HB marker accesses: write=%d read=%d seq=%v addr=%#x log=%#v", write, read, tr.accSeq, addr, tr)
	}
	clk, pidx := dporClocks(tr)
	if dporConcurrent(clk, pidx, tr, write, read) {
		t.Fatalf("channel HB did not order marker accesses: write=%d read=%d syncKind=%v syncSeq=%v syncID=%v syncAux=%v", write, read, tr.syncKind, tr.syncSeq, tr.syncID, tr.syncAux)
	}
	return tr, tr.accSeq[write], tr.accSeq[read]
}

func TestExploreRecordsMutexHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("mutex HB events are emitted by dst-race sync hooks")
	}
	mu := new(sync.Mutex)
	x := new(int)
	addr := uintptr(unsafe.Pointer(x))
	dstExploreInit(128, 512, 128, 512)
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		mu.Lock()
		dstAccessYield(unsafe.Pointer(x), true)
		*x = 1
		mu.Unlock()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			dstAccessYield(unsafe.Pointer(x), false)
			_ = *x
			mu.Unlock()
		}()
		wg.Wait()
		return false
	})
	release, acquire := false, false
	for _, k := range tr.syncKind {
		release = release || k == syncEventRelease
		acquire = acquire || k == syncEventAcquire
	}
	if !release || !acquire {
		t.Fatalf("mutex did not record sync release/acquire events: kind=%v", tr.syncKind)
	}
	write, read := -1, -1
	for i := range tr.accSeq {
		if tr.accAddr[i] != addr {
			continue
		}
		if tr.accWrite[i] && write < 0 {
			write = i
		}
		if !tr.accWrite[i] {
			read = i
		}
	}
	if write < 0 || read < 0 || tr.accSeq[write] == tr.accSeq[read] {
		t.Fatalf("missing mutex protected access pair: write=%d read=%d seq=%v addr=%#x log=%#v", write, read, tr.accSeq, addr, tr)
	}
	clk, pidx := dporClocks(tr)
	if dporConcurrent(clk, pidx, tr, write, read) {
		t.Fatalf("mutex Unlock/Lock HB did not order protected accesses: write=%d read=%d sync=%#v", write, read, tr.syncKind)
	}
}

func TestExploreLiveSyncHBFiltersProtectedAccesses(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("live sync HB filtering is active under dst-race access hooks")
	}
	for _, tt := range []struct {
		name string
		sut  func(unsafe.Pointer, bool) func() bool
	}{
		{
			name: "Mutex",
			sut: func(marker unsafe.Pointer, record bool) func() bool {
				return func() bool {
					var mu sync.Mutex
					var wg sync.WaitGroup
					wg.Add(2)
					for g := 0; g < 2; g++ {
						go func() {
							defer wg.Done()
							for i := 0; i < 3; i++ {
								mu.Lock()
								if record {
									dstAccessYield(marker, true)
								}
								mu.Unlock()
							}
						}()
					}
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "ChannelToken",
			sut: func(marker unsafe.Pointer, record bool) func() bool {
				return func() bool {
					ch := make(chan struct{}, 1)
					ch <- struct{}{}
					var wg sync.WaitGroup
					wg.Add(2)
					for g := 0; g < 2; g++ {
						go func() {
							defer wg.Done()
							for i := 0; i < 3; i++ {
								<-ch
								if record {
									dstAccessYield(marker, true)
								}
								ch <- struct{}{}
							}
						}()
					}
					wg.Wait()
					return false
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			marker := new(int)
			count := func(record bool) uint64 {
				dstAccessYieldReset()
				dstExploreInit(512, 8192, 1024, 4096)
				_, _, tr := runOnce(1, nil, map[accessForce]bool{}, tt.sut(unsafe.Pointer(marker), record))
				if tr.overflow {
					t.Fatalf("trace overflowed while measuring %s live HB filtering: %#v", tt.name, tr)
				}
				return dstAccessYieldFP()
			}
			baseline := count(false)
			withMarker := count(true)
			if withMarker != baseline {
				t.Fatalf("%s HB-ordered marker accesses added live yield points: baseline=%d withMarker=%d", tt.name, baseline, withMarker)
			}
		})
	}
}

func TestExploreRWMutexFailedTryLockDoesNotRecordHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("RWMutex HB events are emitted by dst-race sync hooks")
	}
	marker := new(int)
	addr := uintptr(unsafe.Pointer(marker))
	sut := func() bool {
		var rw sync.RWMutex
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			rw.RLock()
			runtime.Gosched()
			rw.RUnlock()
		}()
		go func() {
			defer wg.Done()
			if !rw.TryLock() {
				dstAccessYield(unsafe.Pointer(marker), false)
				return
			}
			rw.Unlock()
		}()
		wg.Wait()
		return false
	}

	dstExploreInit(256, 4096, 512, 512)
	stack := [][]uint64{nil}
	seen := map[string]bool{}
	for len(stack) > 0 && len(seen) < 200 {
		prefix := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		key := encodePrefix(prefix)
		if seen[key] {
			continue
		}
		seen[key] = true
		_, _, tr := runOnce(1, prefix, map[accessForce]bool{}, sut)
		read := -1
		for i := range tr.accSeq {
			if tr.accAddr[i] != addr {
				continue
			}
			if !tr.accWrite[i] {
				read = i
			}
		}
		if read >= 0 {
			seq := tr.accSeq[read]
			for i, k := range tr.syncKind {
				if tr.syncSeq[i] == seq {
					t.Fatalf("failed public RWMutex.TryLock recorded DST HB event kind=%d id=%#x for goroutine %d: syncKind=%v syncSeq=%v syncID=%v", k, tr.syncID[i], seq, tr.syncKind, tr.syncSeq, tr.syncID)
				}
			}
			return
		}
		for i := len(prefix); i < len(tr.procs); i++ {
			for _, g := range tr.enabled[i] {
				if g == tr.procs[i] {
					continue
				}
				child := make([]uint64, i+1)
				copy(child, tr.procs[:i])
				child[i] = g
				stack = append(stack, child)
			}
		}
	}
	t.Fatalf("failed to reach a trace with reader-caused failed RWMutex.TryLock after %d schedules", len(seen))
}

func TestExploreRecordsRWMutexHB(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("RWMutex HB events are emitted by dst-race sync hooks")
	}
	for _, tt := range []struct {
		name string
		sut  func(unsafe.Pointer) func() bool
	}{
		{
			name: "UnlockToLock",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					var rw sync.RWMutex
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						rw.Lock()
						dstAccessYield(marker, true)
						runtime.Gosched()
						rw.Unlock()
					}()
					go func() {
						defer wg.Done()
						rw.Lock()
						dstAccessYield(marker, false)
						rw.Unlock()
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "UnlockToTryLock",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					var rw sync.RWMutex
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						rw.Lock()
						dstAccessYield(marker, true)
						runtime.Gosched()
						rw.Unlock()
					}()
					go func() {
						defer wg.Done()
						for {
							if rw.TryLock() {
								dstAccessYield(marker, false)
								rw.Unlock()
								return
							}
							runtime.Gosched()
						}
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "UnlockToRLock",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					var rw sync.RWMutex
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						rw.Lock()
						dstAccessYield(marker, true)
						runtime.Gosched()
						rw.Unlock()
					}()
					go func() {
						defer wg.Done()
						rw.RLock()
						dstAccessYield(marker, false)
						rw.RUnlock()
					}()
					wg.Wait()
					return false
				}
			},
		},
		{
			name: "RUnlockToLock",
			sut: func(marker unsafe.Pointer) func() bool {
				return func() bool {
					var rw sync.RWMutex
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						rw.RLock()
						dstAccessYield(marker, true)
						runtime.Gosched()
						rw.RUnlock()
					}()
					go func() {
						defer wg.Done()
						rw.Lock()
						dstAccessYield(marker, false)
						rw.Unlock()
					}()
					wg.Wait()
					return false
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			marker := new(int)
			addr := uintptr(unsafe.Pointer(marker))
			sut := tt.sut(unsafe.Pointer(marker))
			dstExploreInit(256, 4096, 512, 512)
			stack := [][]uint64{nil}
			seen := map[string]bool{}
			for len(stack) > 0 && len(seen) < 200 {
				prefix := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				key := encodePrefix(prefix)
				if seen[key] {
					continue
				}
				seen[key] = true
				_, _, tr := runOnce(1, prefix, map[accessForce]bool{}, sut)
				write, read := -1, -1
				for i := range tr.accSeq {
					if tr.accAddr[i] != addr {
						continue
					}
					if tr.accWrite[i] {
						write = i
					} else {
						read = i
					}
				}
				if write >= 0 && read >= 0 && write < read && tr.accSeq[write] != tr.accSeq[read] {
					clk, pidx := dporClocks(tr)
					if dporConcurrent(clk, pidx, tr, write, read) {
						t.Fatalf("RWMutex public HB did not order %s accesses: write=%d read=%d seq=%v syncKind=%v syncSeq=%v syncID=%v", tt.name, write, read, tr.accSeq, tr.syncKind, tr.syncSeq, tr.syncID)
					}
					return
				}
				for i := len(prefix); i < len(tr.procs); i++ {
					for _, g := range tr.enabled[i] {
						if g == tr.procs[i] {
							continue
						}
						child := make([]uint64, i+1)
						copy(child, tr.procs[:i])
						child[i] = g
						stack = append(stack, child)
					}
				}
			}
			t.Fatalf("failed to reach %s RWMutex HB trace after %d schedules", tt.name, len(seen))
		})
	}
}

func TestExplorePostGoBoundaryNonRace(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if dstRaceEnabledFP() {
		t.Skip("non-race post-go boundary regression")
	}
	sut := func() bool {
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
		return read == 0
	}
	res := Explore(1, Exhaustive, sut)
	if !res.Exhausted || res.Overflow || res.BudgetHit {
		t.Fatalf("post-go SUT did not cleanly exhaust: exhausted=%v overflow=%v budget=%v", res.Exhausted, res.Overflow, res.BudgetHit)
	}
	if len(res.Failures) == 0 {
		t.Fatalf("Explore missed child-before-parent-write after go statement: schedules=%d", res.Schedules)
	}
	failed, raced := Replay(1, res.Failures[0], sut)
	if !failed || raced {
		t.Fatalf("post-go failure did not replay as assertion failure: failed=%v raced=%v failure=%#v", failed, raced, res.Failures[0])
	}
}

func TestExplorePostGoBoundaryCanBeDisabled(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if dstRaceEnabledFP() {
		t.Skip("non-race post-go boundary regression")
	}
	old := dstSetPostGoYield(false)
	defer dstSetPostGoYield(old)
	res := Explore(1, Exhaustive, func() bool {
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
		return read == 0
	})
	if len(res.Failures) != 0 {
		t.Fatalf("disabled post-go boundary still explored child-before-parent write: %#v", res.Failures)
	}
}

type emptyPanicError struct{}

func (emptyPanicError) Error() string { return "" }

type exploreCallbackPanicObj struct{ b byte }

type exploreCallbackSignal struct {
	ch  chan struct{}
	msg string
}

//go:noinline
func makeExploreFinalizerPanic(msg string) {
	o := &exploreCallbackPanicObj{}
	runtime.SetFinalizer(o, func(*exploreCallbackPanicObj) { panic(msg) })
	runtime.KeepAlive(o)
}

//go:noinline
func makeExploreCleanupPanic(msg string) {
	o := &exploreCallbackPanicObj{}
	runtime.AddCleanup(o, func(msg string) { panic(msg) }, msg)
	runtime.KeepAlive(o)
}

//go:noinline
func makeExploreFinalizerChanTouch(ch chan struct{}) {
	o := &exploreCallbackPanicObj{}
	runtime.SetFinalizer(o, func(*exploreCallbackPanicObj) { ch <- struct{}{} })
	runtime.KeepAlive(o)
}

// TestExploreDrainPanicDiscardsResidualCallbacks verifies that after a
// drain-callback panic is recorded as a Failure, callbacks queued later in the
// run — including bubble-channel-touching ones — are deterministically
// discarded at teardown. Before the fix they leaked past dstDeactivate to the
// bubble-less async workers, which fataled the process ("send on synctest
// channel from outside bubble") after Explore had already returned.
func TestExploreDrainPanicDiscardsResidualCallbacks(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	want := "finalizer callback boom"
	sut := func() bool {
		makeExploreFinalizerPanic(want)
		runtime.GC()
		time.Sleep(time.Millisecond) // drain panics; recorded, drain dead
		ch := make(chan struct{}, 1)
		makeExploreFinalizerChanTouch(ch)
		runtime.GC()
		time.Sleep(time.Millisecond) // dead drain: the queued finalizer must be discarded
		return false
	}
	res := Explore(1, DPOR, sut)
	if len(res.Failures) != 1 || res.Failures[0].Panic != want {
		t.Fatalf("drain panic not reported as the failure: %#v", res.Failures)
	}
	// Surface any leaked bubble-stamped callback now (it would fatal the
	// process on the async workers) rather than after the test exits.
	for range 3 {
		runtime.GC()
	}
	time.Sleep(10 * time.Millisecond)
}

//go:noinline
func makeExploreFinalizerSignalPanic(sig exploreCallbackSignal) {
	o := &exploreCallbackPanicObj{}
	runtime.SetFinalizer(o, func(*exploreCallbackPanicObj) {
		sig.ch <- struct{}{}
		panic(sig.msg)
	})
	runtime.KeepAlive(o)
}

//go:noinline
func makeExploreCleanupSignalPanic(sig exploreCallbackSignal) {
	o := &exploreCallbackPanicObj{}
	runtime.AddCleanup(o, func(sig exploreCallbackSignal) {
		sig.ch <- struct{}{}
		panic(sig.msg)
	}, sig)
	runtime.KeepAlive(o)
}

func TestExploreReportsPanicFailure(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool { panic("boom") }
	res := Explore(1, DPOR, sut)
	if len(res.Failures) != 1 || res.Failures[0].Panic != "boom" || res.Failures[0].Deadlock != "" || res.Failures[0].Race {
		t.Fatalf("panic was not reported as a replayable failure: %#v", res.Failures)
	}
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		Replay(1, res.Failures[0], sut)
	}()
	if !panicked {
		t.Fatalf("Replay of panic failure did not panic")
	}
	res = Explore(1, DPOR, func() bool { panic(emptyPanicError{}) })
	if len(res.Failures) != 1 || res.Failures[0].Panic == "" || res.Failures[0].Deadlock != "" || res.Failures[0].Race {
		t.Fatalf("empty-message error panic was not reported as a replayable failure: %#v", res.Failures)
	}
}

func TestExploreReportsChildPanicFailure(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			panic("child boom")
		}()
		wg.Wait()
		return false
	}
	res := Explore(1, DPOR, sut)
	if len(res.Failures) != 1 || res.Failures[0].Panic != "child boom" || res.Failures[0].Deadlock != "" || res.Failures[0].Race {
		t.Fatalf("child panic was not reported as a replayable failure: %#v", res.Failures)
	}
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		Replay(1, res.Failures[0], sut)
	}()
	if !panicked {
		t.Fatalf("Replay of child panic failure did not panic")
	}
}

func TestExploreReportsDrainCallbackPanicFailure(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	for _, tt := range []struct {
		name string
		make func(string)
	}{
		{name: "finalizer", make: makeExploreFinalizerPanic},
		{name: "cleanup", make: makeExploreCleanupPanic},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.name + " callback boom"
			sut := func() bool {
				tt.make(want)
				time.Sleep(time.Millisecond)
				return false
			}
			res := Explore(1, DPOR, sut)
			if len(res.Failures) != 1 || res.Failures[0].Panic != want || res.Failures[0].Deadlock != "" || res.Failures[0].Race {
				t.Fatalf("%s panic was not reported as a replayable failure: %#v", tt.name, res.Failures)
			}
			panicked := false
			func() {
				defer func() {
					if v := recover(); v != nil && panicString(v) == want {
						panicked = true
					}
				}()
				Replay(1, res.Failures[0], sut)
			}()
			if !panicked {
				t.Fatalf("Replay of %s callback panic failure did not panic with %q", tt.name, want)
			}
		})
	}
}

func TestExploreReportsDrainCallbackPanicBeforeLaterTopPanic(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	for _, tt := range []struct {
		name string
		make func(exploreCallbackSignal)
	}{
		{name: "finalizer", make: makeExploreFinalizerSignalPanic},
		{name: "cleanup", make: makeExploreCleanupSignalPanic},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.name + " callback boom"
			sut := func() bool {
				ch := make(chan struct{})
				tt.make(exploreCallbackSignal{ch: ch, msg: want})
				<-ch
				panic("top boom")
			}
			res := Explore(1, DPOR, sut)
			if len(res.Failures) != 1 || res.Failures[0].Panic != want || res.Failures[0].Deadlock != "" || res.Failures[0].Race {
				t.Fatalf("%s callback panic was not preserved before later top panic: %#v", tt.name, res.Failures)
			}
		})
	}
}

func TestExploreReportsNestedChildPanicClearsPanicDefers(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	before := dstRunningPanicDefersFP()
	sut := func() bool {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { panic("inner") }()
			panic("outer")
		}()
		wg.Wait()
		return false
	}
	res := Explore(1, DPOR, sut)
	if len(res.Failures) != 1 || res.Failures[0].Panic != "inner" || res.Failures[0].Deadlock != "" || res.Failures[0].Race {
		t.Fatalf("nested child panic was not reported as a replayable failure: %#v", res.Failures)
	}
	if got := dstRunningPanicDefersFP(); got != before {
		t.Fatalf("nested child panic leaked runningPanicDefers: before=%d after=%d", before, got)
	}
}

func TestExploreReportsDeadlockFailure(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool {
		ch := make(chan struct{})
		<-ch
		return false
	}
	res := Explore(1, Exhaustive, sut)
	if len(res.Failures) != 1 || res.Failures[0].Deadlock == "" || res.Failures[0].Panic != "" || res.Failures[0].Race {
		t.Fatalf("deadlock was not reported as a replayable failure: exhausted=%v overflow=%v failures=%#v", res.Exhausted, res.Overflow, res.Failures)
	}
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		Replay(1, res.Failures[0], sut)
	}()
	if !panicked {
		t.Fatalf("Replay of deadlock failure did not panic")
	}
	clean := Explore(1, Exhaustive, func() bool { return false })
	if !clean.Exhausted || clean.Overflow || clean.BudgetHit || len(clean.Failures) != 0 {
		t.Fatalf("deadlocked bubble state affected later Explore run: %#v", clean)
	}
}

// budgetedExploreSUT gives the explorer multiple interleavings to cut short
// (two announced writes to one address, so DPOR has reversals to schedule)
// while keeping the REAL writes mutex-synchronized: the budget tests run
// in-process, and an actually-racy SUT would leak a TSan report past the
// harness under -race (the -tags dst -race suite must stay clean; the race
// oracle itself is enforced by the subprocess-based oracle tests).
func budgetedExploreSUT() bool {
	var mu sync.Mutex
	var x int
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		dstAccessYield(unsafe.Pointer(&x), true)
		mu.Lock()
		x = 1
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		dstAccessYield(unsafe.Pointer(&x), true)
		mu.Lock()
		x = 2
		mu.Unlock()
	}()
	wg.Wait()
	return false
}

func TestExploreWithScheduleBudgetReportsIncomplete(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	res := ExploreWith(1, ExploreOptions{Mode: Exhaustive, MaxSchedules: 1}, budgetedExploreSUT)
	if res.Schedules != 1 || !res.BudgetHit || res.Exhausted || res.Overflow {
		t.Fatalf("schedule budget not reported distinctly: schedules=%d exhausted=%v overflow=%v budget=%v", res.Schedules, res.Exhausted, res.Overflow, res.BudgetHit)
	}
}

func TestExploreWithStepBudgetReportsIncomplete(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	res := ExploreWith(1, ExploreOptions{Mode: Exhaustive, MaxSteps: 1}, budgetedExploreSUT)
	if !res.BudgetHit || res.Exhausted || res.Overflow {
		t.Fatalf("step budget not reported distinctly: schedules=%d exhausted=%v overflow=%v budget=%v", res.Schedules, res.Exhausted, res.Overflow, res.BudgetHit)
	}
}

// TestExploreFilterPageIndexFindsConflicts pins the shared-address filter's
// overlap detection through every lookup path of its page index: the
// second of two unordered conflicting accesses must YIELD (the filter sees the
// prior overlapping entry), so each conflicting case costs exactly one more
// yield than its structurally identical disjoint control. Cases cover the
// chained-page path (scalar/scalar, and an entry straddling a page boundary
// queried from its second page), the large-entry list (a range wider than the
// per-entry page cap), and the full-scan fallback (a queried range wider than
// the query page cap). The +1 holds whichever of the two accesses the schedule
// commits first — either order leaves one filtered and one yielding.
func TestExploreFilterPageIndexFindsConflicts(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("requires -race so the shared-address filter is active")
	}
	buf := make([]byte, 1<<17)
	yields := func(annA, annB func()) uint64 {
		dstExploreInit(1024, 4096, 1024, 1024)
		dstAccessYieldReset()
		runOnce(1, nil, map[accessForce]bool{}, func() bool {
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				annA()
			}()
			go func() {
				defer wg.Done()
				annB()
			}()
			wg.Wait()
			return false
		})
		return dstAccessYieldFP()
	}
	cases := []struct {
		name     string
		overlap  [2]func()
		disjoint [2]func()
	}{
		{
			name: "scalar-scalar",
			overlap: [2]func(){
				func() { dstAccessYield(unsafe.Pointer(&buf[64]), true) },
				func() { dstAccessYield(unsafe.Pointer(&buf[64]), true) },
			},
			disjoint: [2]func(){
				func() { dstAccessYield(unsafe.Pointer(&buf[64]), true) },
				func() { dstAccessYield(unsafe.Pointer(&buf[1<<16]), true) },
			},
		},
		{
			// A 4-byte entry straddling the 256-byte page boundary at 512,
			// conflicting with a scalar in its SECOND page: the index must
			// find it under every page it covers.
			name: "page-boundary-straddle",
			overlap: [2]func(){
				func() { dstAccessYieldRange(unsafe.Pointer(&buf[510]), 4, true) },
				func() { dstAccessYield(unsafe.Pointer(&buf[513]), true) },
			},
			disjoint: [2]func(){
				func() { dstAccessYieldRange(unsafe.Pointer(&buf[510]), 4, true) },
				func() { dstAccessYield(unsafe.Pointer(&buf[1<<16]), true) },
			},
		},
		{
			// 4096 bytes = 16 pages, beyond the per-entry page cap: the
			// entry lives on the always-scanned large-entry list.
			name: "large-entry-list",
			overlap: [2]func(){
				func() { dstAccessYieldRange(unsafe.Pointer(&buf[0]), 4096, true) },
				func() { dstAccessYield(unsafe.Pointer(&buf[2000]), true) },
			},
			disjoint: [2]func(){
				func() { dstAccessYieldRange(unsafe.Pointer(&buf[0]), 4096, true) },
				func() { dstAccessYield(unsafe.Pointer(&buf[1<<16]), true) },
			},
		},
		{
			// A queried range of 256+ pages exceeds the query page cap: the
			// filter falls back to the full entry scan and must still see
			// the prior scalar inside the range.
			name: "full-scan-fallback",
			overlap: [2]func(){
				func() { dstAccessYield(unsafe.Pointer(&buf[30000]), true) },
				func() { dstAccessYieldRange(unsafe.Pointer(&buf[0]), 1<<16, true) },
			},
			disjoint: [2]func(){
				func() { dstAccessYield(unsafe.Pointer(&buf[(1<<16)+512]), true) },
				func() { dstAccessYieldRange(unsafe.Pointer(&buf[0]), 1<<16, true) },
			},
		},
	}
	for _, tc := range cases {
		got := yields(tc.overlap[0], tc.overlap[1])
		want := yields(tc.disjoint[0], tc.disjoint[1])
		if got != want+1 {
			t.Errorf("%s: conflicting pair yielded %d times, disjoint control %d — want exactly control+1 (filter missed or over-detected the overlap)", tc.name, got, want)
		}
	}
}

// TestExploreFilterPageIndexExhaustionConservative pins the page-index
// overflow contract: exhausting the preallocated (entry,page) node pool must
// flip the filter conservative — every later access yields — never silently
// skip indexing an entry (which would let the filter miss a real conflict and
// lose an interleaving class). With a tiny budget, three 7-page ranges
// overrun the pool, so a later private scalar — normally filtered — must
// yield; with a roomy budget the same SUT's accesses are all filtered.
func TestExploreFilterPageIndexExhaustionConservative(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		t.Skip("requires -race so the shared-address filter is active")
	}
	buf := make([]byte, 1<<17)
	sut := func() bool {
		for i := 0; i < 3; i++ {
			dstAccessYieldRange(unsafe.Pointer(&buf[i*8192]), 7*256, true)
		}
		dstAccessYield(unsafe.Pointer(&buf[1<<16]), true)
		return false
	}
	yields := func(maxAccesses int) uint64 {
		dstExploreInit(64, 256, 64, maxAccesses)
		dstAccessYieldReset()
		runOnce(1, nil, map[accessForce]bool{}, sut)
		return dstAccessYieldFP()
	}
	// maxAccesses=8 gives a 16-node pool; 3 ranges need 21 nodes, so the
	// third range exhausts it and everything after it — the trailing scalar
	// plus any compiler auto-instrumented in-bubble access — must yield
	// conservatively. The budgets isolate the NODE pool as the tripped cap:
	// the SUT creates only 4 entries against an 8-entry budget, so the
	// entry-table cap cannot be what flips the flag (the silent-exhaustion
	// mutation run confirms the node pool is the trigger). The roomy run pins the baseline at zero, so any
	// nonzero count in the tiny run can only come from the conservative
	// flag the exhaustion set.
	if got := yields(1024); got != 0 {
		t.Errorf("roomy pool: %d yields, want 0 (single-goroutine accesses are all filtered)", got)
	}
	if got := yields(8); got == 0 {
		t.Errorf("exhausted pool: 0 yields — node-pool exhaustion did not set the conservative fallback, the filter can silently lose conflicts")
	}
}

// TestExploreSyncEventOverflowReportsIncomplete: a run that drops offline sync-HB
// events (the buffer is sized by maxEdges) must mark the trace incomplete — the
// dropped events under-order the trace-HB the weak-initial computation reads, so a
// silent overflow could drop a Mazurkiewicz class while Exhausted read true
// (exploration.md, hardening clause 1). Mirrors the access-log overflow test.
func TestExploreSyncEventOverflowReportsIncomplete(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if !dstRaceEnabledFP() {
		// Sync-HB events are recorded only under -race (chan/mutex hooks gate on
		// dstBuild && raceenabled) — the offline DPOR that reads them runs in the
		// auto-instrument regime, which is the race leg.
		t.Skip("requires -race (sync-HB events are recorded only under -race)")
	}
	// maxEdges=4 sizes BOTH the edge and sync-event buffers at 4; every OTHER budget
	// is roomy. A SINGLE goroutine hammering a buffered channel records a sync event
	// per send/recv (the slot HB) but creates NO goready edges (no waiter to wake),
	// few decisions, and — with a large maxAccesses — no access-log overflow. So the
	// sync-event buffer is the ONLY one that overflows: tr.overflow can then come
	// only from the sync-event fold, which is what this test pins (a many-goroutine
	// or small-maxAccesses SUT trips edge/access-log overflow and passes vacuously —
	// the mutation that deletes the fold must FAIL here).
	dstExploreInit(1<<14, 1<<18, 4, 1<<20)
	dstAccessYieldReset()
	_, _, tr := runOnce(1, nil, map[accessForce]bool{}, func() bool {
		ch := make(chan int, 1)
		for i := 0; i < 32; i++ {
			ch <- i
			<-ch
		}
		return false
	})
	if !dstSyncEventOverflowProbe() {
		t.Fatalf("the SUT did not overflow the sync-event buffer — test is vacuous, raise the op count or lower maxEdges")
	}
	// Isolation: every non-sync overflow contributor must be off, or tr.overflow
	// would be true regardless of the fold under test.
	if dstEdgeOverflowFP() || dstTraceOverflowFP() || dstAccLogOverflowFP() {
		t.Fatalf("a non-sync buffer overflowed (edge=%v trace=%v accLog=%v) — tr.overflow no longer isolates the sync-event fold (vacuous)",
			dstEdgeOverflowFP(), dstTraceOverflowFP(), dstAccLogOverflowFP())
	}
	if !tr.overflow {
		t.Fatalf("sync-event overflow did not mark the trace incomplete — a dropped HB event can silently lose a class")
	}
}

//go:linkname dstAccPageCharge runtime.dstAccPageCharge
func dstAccPageCharge(size uintptr) uintptr

// TestExploreAccPageChargeAddressIndependent pins the M17 fix: the access-filter's
// page-node capacity charge is a function of the access SIZE alone — the exact
// worst-case page count over all address alignments — never of the run-local address.
// An alignment-dependent charge would flip dstFilterConservative at a different point
// in a fresh process (addresses shift with explorer allocations and arena placement),
// misaligning a replayed schedule and breaking DST-L2-2 (exploration.md, hardening
// clause 2). Verified against a brute-force max over every alignment within a page.
func TestExploreAccPageChargeAddressIndependent(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	const page = 1 << 8 // dstAccPageShift = 8
	for _, size := range []uintptr{1, 2, 255, 256, 257, 511, 512, 513, 1000, 4096, 65537} {
		var worst uintptr
		for a := uintptr(0); a < page; a++ {
			start := a >> 8
			end := (a + size - 1) >> 8
			if n := end - start + 1; n > worst {
				worst = n
			}
		}
		if got := dstAccPageCharge(size); got != worst {
			t.Errorf("dstAccPageCharge(%d) = %d, want %d (exact alignment-worst-case page count)", size, got, worst)
		}
	}
}

// TestExploreFanOutOverflowIsNotBudgetHit: a run whose enabled-set FAN-OUT
// exceeds the internal capacity — while its decision count still fits the
// caller's MaxSteps — reports Overflow (internal truncation), never BudgetHit:
// the attribution names the capacity that actually hit (the fan-out headroom
// derives from MaxSteps, so raising it can still help).
// 300 concurrently-live goroutines give the spawn ramp a quadratic
// enabled-set footprint (~N²/2 entries) that crosses MaxSteps*64 near step
// 256, well under the 512-decision budget.
func TestExploreFanOutOverflowIsNotBudgetHit(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	res := ExploreWith(1, ExploreOptions{MaxSteps: 512}, func() bool {
		var wg sync.WaitGroup
		release := make(chan struct{})
		for i := 0; i < 300; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-release
			}()
		}
		close(release)
		wg.Wait()
		return false
	})
	if !res.Overflow {
		t.Errorf("Overflow = false, want true: the enabled-set fan-out exceeded the internal capacity")
	}
	if res.BudgetHit {
		t.Errorf("BudgetHit = true, want false: the truncation is the internal fan-out capacity, not a caller budget (with the misattribution, MaxSteps took the blame)")
	}
	if res.Exhausted {
		t.Errorf("Exhausted = true, want false under overflow")
	}
}

// TestExploreTruncatedFailureReplays: a failure found in a run whose decision
// trace TRUNCATED (fan-out overflow) still carries a replayable Schedule.
// Failure.Schedule is the SPAWNING prefix (derived from untruncated parents —
// here the root, so empty), which is what makes it structurally gap-free; the
// runtime separately CHECKS that recording never resumes past a truncation
// (the trace-gap throw), so any future consumer of the recorded trace is
// protected too.
func TestExploreTruncatedFailureReplays(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	sut := func() bool {
		var wg sync.WaitGroup
		release := make(chan struct{})
		for i := 0; i < 300; i++ { // overflow the fan-out capacity mid-ramp
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-release
			}()
		}
		close(release)
		wg.Wait() // the fan-out shrinks back to 1: resumed recording would gap here
		return true
	}
	res := ExploreWith(1, ExploreOptions{MaxSteps: 512}, sut)
	if len(res.Failures) == 0 {
		t.Fatal("the failing SUT produced no failure")
	}
	failed, _ := Replay(1, res.Failures[0], sut)
	if !failed {
		t.Error("replay of the truncated-trace failure did not reproduce the assertion failure")
	}
}

// dporConflict runs one shared-variable read/write race through the manual
// access hooks: a reader and a writer of v, so DPOR sees a reversible
// conflict and seeds a backtrack at the reader's decision. Returns the value
// the reader observed (1 iff the write was ordered first).
func dporConflict(v *int) int {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dstAccessYield(unsafe.Pointer(v), true)
		*v = 1
	}()
	dstAccessYield(unsafe.Pointer(v), false)
	seen := *v
	wg.Wait()
	return seen
}

// TestExploreDPORTruncatedChildContinuesWalk is the discriminating pin for the
// truncated-child continuation contract: a truncated DPOR child must not END
// the walk — backtracks seeded on earlier frames by earlier untruncated runs
// are still explored. Two independent conflicts, the shallow one (b) before
// the deeper one (a); the fan-out that truncates is gated on a's REVERSAL, so
// deepest-first backtracking reaches the truncating run while b's backtrack is
// still pending. The observable failure lives ONLY in b's reversal. Continuing
// past the truncation (the contract) explores it; ending the walk at the
// truncation (the regression this pins) loses it. Empirically: 9 schedules /
// 3 failures continuing, 3 / 0 with the walk broken at truncation.
func TestExploreDPORTruncatedChildContinuesWalk(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if dstRaceEnabledFP() {
		// The conflicts are intentional data races (the point of the manual
		// access hooks); -race fails tRunner on them. DPOR's walk structure —
		// the contract under test — is identical in both build modes.
		t.Skip("intentionally racy sut; the continuation contract is build-mode-independent")
	}
	sut := func() bool {
		var b, a int
		bWas := dporConflict(&b) // shallow conflict: seeds a backtrack first
		aWas := dporConflict(&a) // deeper conflict
		if aWas == 1 {
			// Gated on the DEEPER conflict's reversal so deepest-first
			// backtracking explores this truncating arm BEFORE the shallow
			// conflict's reversal is drained.
			var wg sync.WaitGroup
			release := make(chan struct{})
			for i := 0; i < 300; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-release
				}()
			}
			close(release)
			wg.Wait()
		}
		return bWas == 1 // the failure is reachable only via b's reversal
	}
	res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSteps: 256}, sut)
	if !res.Overflow {
		t.Errorf("Overflow = false, want true: the gated fan-out truncates a child")
	}
	if res.Exhausted {
		t.Errorf("Exhausted = true, want false under truncation")
	}
	// The load-bearing assertion: the shallow conflict's failure survives the
	// truncation of the deeper arm. A walk that stopped at the truncating run
	// would report zero failures.
	if len(res.Failures) == 0 {
		t.Fatalf("no failures found: the walk did not continue past the truncated child (its pending backtracks were abandoned)")
	}
}

// TestExploreDPORTruncatedChildNoExtensionExplosion pins the EXTENSION-SKIP
// leg of the truncation guard (explore.go: `if tr.traceTruncated { n =
// len(stack) }`), distinct from the continuation leg above: a truncated
// trace must not be EXTENDED into new DPOR frames, or each conflict in the
// truncating run's recorded prefix multiplies the walk (its children
// re-truncate without bound). The SUT records k independent conflicts and
// then an UNCONDITIONAL fan-out that truncates every run: with the guard the
// stack is never extended past the recorded conflicts, so the walk stays
// bounded (backtracks over the k conflicts alone); without it the truncated
// frames spawn children that re-truncate, and schedules grow ~3^k. k=6 is
// well under a second guarded and blows past any test budget unguarded, so
// a bounded time cap discriminates without a fragile timeout assertion.
func TestExploreDPORTruncatedChildNoExtensionExplosion(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	if dstRaceEnabledFP() {
		t.Skip("intentionally racy sut; the extension-skip is build-mode-independent")
	}
	const k = 6
	sut := func() bool {
		vars := make([]int, k)
		for i := range vars {
			_ = dporConflict(&vars[i]) // k independent recorded conflicts
		}
		// Unconditional fan-out: every run truncates here, AFTER the k
		// conflicts are recorded.
		var wg sync.WaitGroup
		release := make(chan struct{})
		for i := 0; i < 300; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-release
			}()
		}
		close(release)
		wg.Wait()
		return false
	}
	res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSteps: 256}, sut)
	if !res.Overflow {
		t.Errorf("Overflow = false, want true under the fan-out truncation")
	}
	// The load-bearing assertion: extension-skip keeps the walk bounded. With
	// the guard, the k conflicts truncate immediately, so the walk explores a
	// handful of schedules; without it, ~3^k. A generous ceiling well below
	// 3^6 = 729 kills the mutant without over-pinning the exact count.
	if res.Schedules > 64 {
		t.Fatalf("walk explored %d schedules for %d conflicts: truncated frames were extended (no extension-skip)", res.Schedules, k)
	}
}

// TestExploreDPORFanOutOverflowContinues: the DPOR walk under a fan-out
// truncation reports Overflow (not BudgetHit) and does not claim exhaustion —
// the truncating run here is the last thing to explore, so it pins the
// Overflow/BudgetHit attribution, not the continuation (see
// TestExploreDPORTruncatedChildContinuesWalk for that) nor the
// extension-skip (see TestExploreDPORTruncatedChildNoExtensionExplosion).
func TestExploreDPORFanOutOverflowContinues(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSteps: 512}, func() bool {
		var wg sync.WaitGroup
		release := make(chan struct{})
		for i := 0; i < 300; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-release
			}()
		}
		close(release)
		wg.Wait()
		return false
	})
	if !res.Overflow {
		t.Errorf("Overflow = false, want true under a fan-out truncation")
	}
	if res.BudgetHit {
		t.Errorf("BudgetHit = true, want false: no caller budget was exceeded")
	}
	if res.Exhausted {
		t.Errorf("Exhausted = true, want false under overflow")
	}
}
