// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"internal/race"
	"strings"
	"testing"
)

// dstEnv is the white-box DST knob combination for the low-level mechanism
// testprogs (which call runtime.dstActivate via $DSTSEED rather than the
// testing/simulation.Run API): a fixed seed, no async preemption, single P, and GC off,
// so that a non-blocking goroutine's randomness is a pure function of the seed.
func dstEnv(seed string) []string {
	return []string{
		"GOMAXPROCS=1",
		"GOGC=off",
		"GODEBUG=asyncpreemptoff=1",
		"DSTSEED=" + seed,
	}
}

// dstChurnEnv is like dstEnv but with GOMAXPROCS=4, to exercise per-g robustness
// under M migration (which the single-P API cannot reproduce).
func dstChurnEnv(seed string) []string {
	return []string{
		"GOMAXPROCS=4",
		"GOGC=off",
		"GODEBUG=asyncpreemptoff=1",
		"DSTSEED=" + seed,
	}
}

// runTestProgDST builds the testprog with -tags dst (which fixes the global map
// hash key, a precondition for deterministic map order and for testing/simulation.Run)
// and runs the named function.
func runTestProgDST(t *testing.T, name string, env ...string) string {
	exe, err := buildTestProg(t, "testprog", "-tags=dst")
	if err != nil {
		t.Fatal(err)
	}
	return runBuiltTestProg(t, exe, name, env...)
}

// TestDSTDeterministicSelect verifies that DST makes select poll
// order a reproducible function of the seed: the same seed yields an identical
// schedule across runs, and a different seed yields a different one. Without the
// runtime RNG seeding this fails, because select poll order is seeded from OS
// entropy per process.
func TestDSTDeterministicSelect(t *testing.T) {
	out1 := runTestProgDST(t, "DSTSelectOrder", dstEnv("12345")...)
	out2 := runTestProgDST(t, "DSTSelectOrder", dstEnv("12345")...)
	if out1 == "" {
		t.Fatal("empty output from testprog")
	}
	if out1 != out2 {
		t.Fatalf("same seed produced different schedules:\nrun1=%q\nrun2=%q", out1, out2)
	}

	out3 := runTestProgDST(t, "DSTSelectOrder", dstEnv("67890")...)
	if out3 == out1 {
		t.Fatalf("different seeds produced identical schedule (seed has no effect): %q", out1)
	}
}

// TestDSTSelectChurn verifies the per-goroutine select RNG is robust to M
// migration. A goroutine is bounced across M's under concurrent churn at
// GOMAXPROCS=4, then records its select order, which must be identical across
// runs. Drawing select poll order from the per-m cheaprand stream makes this
// diverge run-to-run (the goroutine lands on different M's with different RNG
// state); the per-g DST stream makes it depend only on the goroutine's logical
// history. This is the test that distinguishes the per-g mechanism from per-m —
// a single-goroutine, single-P test cannot, because per-m is already
// deterministic there. (Empirically: per-g 0 divergences, per-m ~58/60.)
func TestDSTSelectChurn(t *testing.T) {
	env := dstChurnEnv("12345")
	first := runTestProgDST(t, "DSTSelectChurn", env...)
	if first == "" {
		t.Fatal("empty output from testprog")
	}
	for i := 0; i < 3; i++ {
		out := runTestProgDST(t, "DSTSelectChurn", env...)
		if out != first {
			t.Fatalf("select order diverged across runs under M churn (run %d):\nfirst=%q\ngot  =%q", i+1, first, out)
		}
	}
}

// TestDSTBubbleReproducible verifies that a synctest bubble's randomness is
// reproducible in isolation: the per-g DST tree is re-rooted per bubble, so a
// bubble's measured math/rand draws are identical whether or not another bubble
// ran before it in the same process. Without per-bubble re-rooting the measured
// bubble would inherit the caller's (advanced) tree position and diverge.
func TestDSTBubbleReproducible(t *testing.T) {
	env := dstEnv("12345")
	withNoise := runTestProgDST(t, "DSTBubbleReproNoise", env...)
	plain := runTestProgDST(t, "DSTBubbleReproPlain", env...)
	if plain == "" {
		t.Fatal("empty output from testprog")
	}
	if withNoise != plain {
		t.Fatalf("bubble randomness depends on prior bubbles (not reproducible in isolation):\nafter noise bubble=%q\nplain          =%q", withNoise, plain)
	}
}

// TestDSTMathRandChurn verifies that the math/rand and math/rand/v2 globals
// (linkname'd to runtime.rand) are robust to M migration under DST: a goroutine
// is bounced across M's under churn at GOMAXPROCS=4 and its global-rand draws
// must be identical across runs. Drawing from the per-m chacha8 stream makes
// this diverge run-to-run; the per-g DST stream makes it depend only on the
// goroutine's logical history. This covers application/library use of the
// rand.* top-level functions (e.g. Pebble's iterator sampling and skiplist
// heights, and sync.Pool via runtime.randn).
func TestDSTMathRandChurn(t *testing.T) {
	env := dstChurnEnv("12345")
	first := runTestProgDST(t, "DSTMathRandChurn", env...)
	if first == "" {
		t.Fatal("empty output from testprog")
	}
	for i := 0; i < 3; i++ {
		out := runTestProgDST(t, "DSTMathRandChurn", env...)
		if out != first {
			t.Fatalf("math/rand draws diverged across runs under M churn (run %d):\nfirst=%q\ngot  =%q", i+1, first, out)
		}
	}
}

// TestDSTDeterministicMap verifies that DST makes map iteration
// order a reproducible function of the seed: the per-map seed and iterator start
// offsets are drawn from the per-g DST stream, so the same seed yields an
// identical order and a different seed yields a different one.
func TestDSTDeterministicMap(t *testing.T) {
	out1 := runTestProgDST(t, "DSTMapOrder", dstEnv("12345")...)
	out2 := runTestProgDST(t, "DSTMapOrder", dstEnv("12345")...)
	if out1 == "" {
		t.Fatal("empty output from testprog")
	}
	if out1 != out2 {
		t.Fatalf("same seed produced different map order:\nrun1=%q\nrun2=%q", out1, out2)
	}
	out3 := runTestProgDST(t, "DSTMapOrder", dstEnv("67890")...)
	if out3 == out1 {
		t.Fatalf("different seeds produced identical map order (seed has no effect): %q", out1)
	}
}

// TestDSTSysmonNoPreempt verifies that under deterministic scheduling
// (DST active) sysmon does not time-preempt a long-running goroutine.
// A watcher goroutine prints "1" if it ever observed the burst goroutine
// mid-burst, which at GOMAXPROCS=1 can only happen if sysmon preempted the
// burst. With the retake gate, it must print "0".
func TestDSTSysmonNoPreempt(t *testing.T) {
	out := runTestProgDST(t, "DSTNoPreempt", dstEnv("1")...)
	if out != "0\n" {
		t.Fatalf("a goroutine was preempted under DST (got %q, want %q): sysmon time-based retake not gated", out, "0\n")
	}
}

// TestDSTPoolReapedAcrossRuns verifies that dst.Run reaps sync.Pools when it
// returns, so a channel pooled in one run is not reused (and rejected by
// synctest) in the next. This is the pattern that otherwise requires patching
// libraries like Pebble to allocate a fresh channel per use. GOGC=off ensures the
// reap is the only thing that clears the pool.
func TestDSTPoolReapedAcrossRuns(t *testing.T) {
	out := runTestProgDST(t, "DSTPoolAcrossRuns", "GOGC=off")
	if out != "ok 2\n" {
		t.Fatalf("pooled channel reuse across dst.Run failed (got %q, want %q): pool not reaped between runs", out, "ok 2\n")
	}
}

// TestDSTGCAllocBoundDeterministic verifies the two Chunk A guarantees for an
// alloc-heavy, non-blocking SUT (design dimension 11): GC is enabled in-run and
// bounds memory, and its observable effect is deterministic. The testprog churns
// ~60MB across four non-blocking goroutines inside dst.Run with GOGC=100, so only
// the heap trigger (not synctest quiescence) can fire GC.
//
// Two assertions, with distinct teeth:
//   - numGC>0 — the reliable teeth for the core Chunk A change (GC enabled in-run).
//     Disabling GC (GOGC=off, or reverting dst.Run to force GC off) makes numGC=0
//     and fails here: memory would be unbounded.
//   - "<sum> <numGC>" identical across runs — observable determinism. With STW
//     (Tier 2, D2) the GC count is stable; concurrent GC lets wall-clock-timed
//     floating garbage flip numGC ±1 (GOGC=100 churn: 20 vs 21), which several
//     samples here catch. This is a secondary guard: STW's primary, reliable teeth
//     is deterministic finalizer/weak discovery, tested in Chunk B. Note the exact
//     trigger byte is NOT asserted — it carries sub-observable accounting noise
//     (design D1); only the observable count is.
func TestDSTGCAllocBoundDeterministic(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"} // GC on → in-run heap trigger fires
	first := runTestProgDST(t, "DSTGCAllocBound", env...)
	if first == "" {
		t.Fatal("empty output from testprog")
	}
	fields := strings.Fields(strings.TrimSpace(first))
	if len(fields) != 2 {
		t.Fatalf("unexpected output %q, want \"<sum> <numGC>\"", first)
	}
	if fields[1] == "0" {
		t.Fatalf("no GC fired during the alloc-bound run (numGC=0): the in-run heap "+
			"trigger is not active, so memory was not bounded by GC; output %q", first)
	}
	for i := 0; i < 9; i++ {
		out := runTestProgDST(t, "DSTGCAllocBound", env...)
		if out != first {
			t.Fatalf("alloc-bound run diverged across runs (run %d): in-run GC is "+
				"observably nondeterministic\nfirst=%q\ngot  =%q", i+1, first, out)
		}
	}
}

// TestDSTGCFinalizerDiscoveryDeterministic verifies that the per-bubble relative
// GC trigger (Tier 2, A.5) makes finalizer discovery deterministic. A
// single-goroutine workload (no Seq-5 interleaving) allocates finalizable objects
// with varied lifetimes. The testprog prints "numGC total perCycleHash".
//
// The assertion is layered to the determinism contract (see testing/simulation package
// doc): the set-level observable (numGC + total finalizers discovered) is
// asserted always — it is -race-robust because the requested-bytes trigger fires
// the right number of times with the right total under -race too. The byte-exact
// per-cycle hash is asserted only in normal builds; under -race the per-cycle
// split jitters by ±span (the trigger is checked at span-grab boundaries that
// -race's redzones shift), which is the documented GC-timing relaxation. The
// absolute pacer trigger makes even numGC/total float — caught by the mutation
// test on the relative-trigger gate. (Multi-goroutine contended workloads carry a
// residual from the GC-independent Seq-5 scheduling-order gap; this test is
// single-goroutine to isolate the GC trigger.)
func TestDSTGCFinalizerDiscoveryDeterministic(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	setLevel := func(out string) string { // "numGC total" — drop the per-cycle hash
		f := strings.Fields(strings.TrimSpace(out))
		if len(f) != 3 {
			t.Fatalf("unexpected output %q, want \"numGC total hash\"", out)
		}
		return f[0] + " " + f[1]
	}
	first := runTestProgDST(t, "DSTGCFinDiscovery", env...)
	f := strings.Fields(strings.TrimSpace(first))
	if len(f) != 3 || f[0] == "0" || f[1] == "0" {
		t.Fatalf("no finalizer discovery recorded (%q): the in-run GC did not "+
			"discover finalizable objects", first)
	}
	for i := 0; i < 9; i++ {
		out := runTestProgDST(t, "DSTGCFinDiscovery", env...)
		if race.Enabled {
			// -race: byte-exact per-cycle has ±span jitter; assert the -race-robust
			// set-level (numGC + total discovered).
			if setLevel(out) != setLevel(first) {
				t.Fatalf("finalizer set-level discovery diverged under -race (run %d): "+
					"numGC/total nondeterministic\nfirst=%q\ngot  =%q", i+1, first, out)
			}
		} else if out != first {
			// normal build: full byte-exact per-cycle determinism.
			t.Fatalf("finalizer-discovery sequence diverged (run %d): per-cycle GC "+
				"discovery is nondeterministic\nfirst=%q\ngot  =%q", i+1, first, out)
		}
	}
}

// TestDSTFinalizerBubbleChannelOp verifies invariant DST-FIN-1: a finalizer that
// does a bubble channel op runs without fatal inside dst.Run, because the
// bubble-scoped drain goroutine (g.bubble == the bubble) runs it, not the async
// system finalizer goroutine fing (g.bubble == nil). The testprog drops a
// finalizable object whose finalizer sends 42 on a bubble channel, then receives
// it — reaching quiescence runs the finalizer, whose send unblocks the receive.
//
// Teeth: with fing draining instead (the !dstActive() gate removed in proc.go),
// the send fatals "send on synctest channel from outside bubble"; with no drain,
// the receive deadlocks. Either way the output is not "ok 42".
func TestDSTFinalizerBubbleChannelOp(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	out := runTestProgDST(t, "DSTFinChanOp", env...)
	if strings.TrimSpace(out) != "ok 42" {
		t.Fatalf("finalizer bubble channel op failed (got %q, want \"ok 42\"): the "+
			"finalizer did not run on a bubble goroutine", out)
	}
}

// TestDSTFinalizerRunSetDeterministic verifies invariant DST-FIN-2: the set of
// finalizers run by Run end is deterministic across runs of the same seed, and
// they actually run. A single-goroutine workload (no Seq-5 interleaving) makes
// 2000 finalizable objects all dead by the time f returns; the testprog prints
// "count sumHex" where sum folds an order-independent per-id mix (the set, not the
// order — which the contract leaves unspecified). count and sum are identical
// across runs in normal and -race builds (the run set is the whole dead set,
// independent of per-cycle discovery jitter).
//
// Teeth: without the drain-exit handshake the run panics (deadlock, total != 1);
// without finalizers running the count is 0; nondeterministic discovery would
// vary the count/sum.
func TestDSTFinalizerRunSetDeterministic(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	first := runTestProgDST(t, "DSTFinRunSet", env...)
	f := strings.Fields(strings.TrimSpace(first))
	if len(f) != 2 || f[0] == "0" {
		t.Fatalf("no finalizers ran (%q): the drain did not run the queued finalizers", first)
	}
	for i := 0; i < 9; i++ {
		out := runTestProgDST(t, "DSTFinRunSet", env...)
		if out != first {
			t.Fatalf("finalizer run set diverged across runs (run %d): the set of "+
				"finalizers run by Run end is nondeterministic\nfirst=%q\ngot  =%q", i+1, first, out)
		}
	}
}

// TestDSTFinalizerSpawn verifies D4 dimension 5: a finalizer that spawns a
// goroutine works inside dst.Run. The spawned goroutine (created from the drain,
// which carries the bubble) inherits the bubble — so its channel send does not
// fatal — and is deterministically scheduled and accounted, so the receive
// completes rather than deadlocking. Expects "ok 7".
func TestDSTFinalizerSpawn(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	out := runTestProgDST(t, "DSTFinSpawn", env...)
	if strings.TrimSpace(out) != "ok 7" {
		t.Fatalf("finalizer goroutine spawn failed (got %q, want \"ok 7\"): the "+
			"spawned goroutine did not inherit the bubble or was not accounted", out)
	}
}

// TestDSTFinalizerPreBubbleDrainedBubbleless verifies the M2 fix: finalizers the
// entry GC queues for pre-bubble objects run bubble-less in dstActivate, not
// in-bubble at the first quiescence (where they would add a run-to-run-varying
// count to the first quiescence's finalizer set, weakening DST-FIN-2). The
// testprog drops a finalizable object before dst.Run and checks, as f's first
// act, that its finalizer already ran. GOGC=off so no pre-Run GC runs it early on
// the ungated fing. Expects "true"; without the pre-bubble drain it is "false".
func TestDSTFinalizerPreBubbleDrainedBubbleless(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=off"}
	out := runTestProgDST(t, "DSTFinPreBubble", env...)
	if strings.TrimSpace(out) != "true" {
		t.Fatalf("pre-bubble finalizer not drained in dstActivate (got %q, want \"true\"): "+
			"the entry GC's pre-bubble finalizers are run in-bubble instead of bubble-less", out)
	}
}

// TestDSTCleanupBubbleChannelOp verifies invariant DST-CLEANUP-1 (the cleanup
// analogue of DST-FIN-1): a cleanup doing a bubble channel op runs without fatal
// inside dst.Run, because the bubble-scoped drain runs it, not the async cleanup
// pool (g.bubble == nil). Expects "ok 42".
func TestDSTCleanupBubbleChannelOp(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	out := runTestProgDST(t, "DSTCleanupChanOp", env...)
	if strings.TrimSpace(out) != "ok 42" {
		t.Fatalf("cleanup bubble channel op failed (got %q, want \"ok 42\"): the "+
			"cleanup did not run on a bubble goroutine", out)
	}
}

// TestDSTCleanupRunSetDeterministic verifies invariant DST-CLEANUP-2: the set of
// cleanups run by the in-bubble drain is deterministic across runs of the same
// seed, and they actually run. Read in-bubble (after a quiescence) so the post-Run
// reap cannot launder the count. Set-level under -race.
func TestDSTCleanupRunSetDeterministic(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	first := runTestProgDST(t, "DSTCleanupRunSet", env...)
	f := strings.Fields(strings.TrimSpace(first))
	if len(f) != 2 || f[0] == "0" {
		t.Fatalf("no cleanups ran (%q): the drain did not run the queued cleanups", first)
	}
	for i := 0; i < 9; i++ {
		out := runTestProgDST(t, "DSTCleanupRunSet", env...)
		if out != first {
			t.Fatalf("cleanup run set diverged across runs (run %d): the set of "+
				"cleanups run is nondeterministic\nfirst=%q\ngot  =%q", i+1, first, out)
		}
	}
}

// TestDSTCleanupRNGIsolation verifies that AddCleanup does not perturb the
// bubble's DST RNG: under DST the async cleanup goroutine (which would draw from
// the creating goroutine's stream and persists across Runs, making the draw
// process-history-dependent) is not created — the createGs gate in mcleanup.go.
// The testprog compares the first rand draw in a Run that calls AddCleanup against
// one that does not; they must match. Expects "ok"; without the gate, "perturbed".
//
// The finalizer counterpart — the createfing gate in mfinal.go, which keeps the
// first SetFinalizer in a Run from creating fing and perturbing the stream — is
// the same mechanism but is not independently testable here: every testprog
// process already creates fing at startup (a stdlib import registers a
// finalizer), so createfing-during-a-Run is unreachable in this harness. The fault
// it guards (a SUT whose first finalizer is inside dst.Run) is reachable in spec;
// this test covers the mechanism class.
func TestDSTCleanupRNGIsolation(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	out := runTestProgDST(t, "DSTCleanupRNGIsolation", env...)
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("AddCleanup perturbed the bubble RNG (got %q, want \"ok\"): a cleanup "+
			"goroutine was created under DST, advancing the bubble goroutine's stream", out)
	}
}

// TestDSTCleanupPreBubbleDrainedBubbleless verifies the cleanup half of the
// dstActivate pre-bubble drain (parallel to TestDSTFinalizerPreBubbleDrained
// Bubbleless): the entry GC's pre-bubble cleanups run bubble-less in dstActivate,
// not in-bubble at the first quiescence. GOGC=off so no pre-Run GC runs them early
// on the ungated async pool. Expects "true".
func TestDSTCleanupPreBubbleDrainedBubbleless(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=off", "GOMAXPROCS=1"}
	out := runTestProgDST(t, "DSTCleanupPreBubble", env...)
	if strings.TrimSpace(out) != "true" {
		t.Fatalf("pre-bubble cleanup not drained in dstActivate (got %q, want \"true\")", out)
	}
}

// TestDSTCleanupPriorGoroutineNotWoken verifies the cleanup WAKE gate (proc.go):
// a cleanup goroutine created before the Run (AddCleanup runs createGs ungated
// outside DST) must not be woken during the Run to run bubble cleanups with
// g.bubble == nil. The testprog forces such a goroutine to exist, then runs a
// channel-op cleanup; with the gate the bubble drain runs it ("ok 42"), without
// it the pre-existing async goroutine runs it and fatals. This is the prior-G
// scenario the createGs gate alone does not cover.
func TestDSTCleanupPriorGoroutineNotWoken(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	out := runTestProgDST(t, "DSTCleanupChanOpPriorG", env...)
	if strings.TrimSpace(out) != "ok 42" {
		t.Fatalf("pre-existing cleanup goroutine ran a bubble cleanup (got %q, want "+
			"\"ok 42\"): the cleanup wake gate did not keep it parked during the Run", out)
	}
}

// TestDSTWeakClearingDeterministic verifies invariant DST-MEM-1 (weak half): the
// set of weak pointers cleared by the in-run STW GC is deterministic across runs
// of the same seed. The testprog drops half of 256 weakly-referenced objects and,
// after a quiescence, reads how many weak refs cleared (in-bubble). Prints
// "cleared alive"; identical across runs, normal and -race (set-level). Weak
// clearing happens during the STW sweep (design.md D4 dimension 7), so the cleared
// set is the quiescent dead set, deterministic.
func TestDSTWeakClearingDeterministic(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	first := runTestProgDST(t, "DSTWeakClearing", env...)
	f := strings.Fields(strings.TrimSpace(first))
	if len(f) != 2 || f[0] == "0" {
		t.Fatalf("no weak refs cleared (%q): weak clearing did not happen in-run", first)
	}
	for i := 0; i < 9; i++ {
		out := runTestProgDST(t, "DSTWeakClearing", env...)
		if out != first {
			t.Fatalf("weak clearing diverged across runs (run %d): the cleared set is "+
				"nondeterministic\nfirst=%q\ngot  =%q", i+1, first, out)
		}
	}
}

// TestDSTGCOffMemoryBounded verifies invariant DST-MEM-2: an allocating bubble is
// deterministically memory-bounded even with GOGC=off. The DST heap trigger falls
// back to a fixed defaultHeapMinimum floor when gcPercent < 0, so a GOGC=off bubble
// that churns ~20MB still triggers several STW GCs (NumGC > 1) rather than growing
// unbounded, and the count is identical across runs. Without the floor (the trigger
// returns false for gcPercent < 0), NumGC stays at 1 (the dstActivate entry GC)
// and the heap is unbounded.
func TestDSTGCOffMemoryBounded(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=off"}
	first := strings.TrimSpace(runTestProgDST(t, "DSTGCOffBound", env...))
	if first == "" || first == "1" || first == "0" {
		t.Fatalf("GOGC=off bubble not memory-bounded (NumGC=%q): the heap trigger did "+
			"not fall back to the floor, so the heap grew unbounded", first)
	}
	for i := 0; i < 5; i++ {
		out := strings.TrimSpace(runTestProgDST(t, "DSTGCOffBound", env...))
		if out != first {
			t.Fatalf("GOGC=off GC count diverged across runs (run %d): the floor-bounded "+
				"GC is nondeterministic\nfirst=%q\ngot  =%q", i+1, first, out)
		}
	}
}

// TestDSTRunAPI exercises the public testing/simulation.Run API end-to-end: Run itself
// enforces GOMAXPROCS=1 and disables async/time preemption, so the test sets no
// scheduling knobs (only the seed and GC off). The same seed replays an
// identical schedule; a different seed yields a different one.
func TestDSTRunAPI(t *testing.T) {
	env := func(seed string) []string { return []string{"GOGC=off", "DSTSEED=" + seed} }
	out1 := runTestProgDST(t, "DSTRunDeterminism", env("12345")...)
	out2 := runTestProgDST(t, "DSTRunDeterminism", env("12345")...)
	if out1 == "" {
		t.Fatal("empty output from testprog")
	}
	if out1 != out2 {
		t.Fatalf("dst.Run with same seed produced different schedules:\nrun1=%q\nrun2=%q", out1, out2)
	}
	out3 := runTestProgDST(t, "DSTRunDeterminism", env("67890")...)
	if out3 == out1 {
		t.Fatalf("dst.Run with different seeds produced identical schedule (seed has no effect): %q", out1)
	}
}

// dstSchedSeeds is the seed spread the Seq-5 scheduling tests use for the
// seed-variation and soundness sweeps.
var dstSchedSeeds = []string{"1", "2", "3", "12345", "999", "777", "424242", "55"}

// dstSchedScenarios names the probe scenarios (testprog dst_sched.go) and whether
// each is expected to diversify freely (true) or only within happens-before
// constraints (false, e.g. a channel ring whose token path is causally fixed).
var dstSchedScenarios = []struct {
	name        string
	freeDiverse bool
}{
	{"gosched", true},   // global runq (Gosched)
	{"spawn", true},     // runnext/local ring (goroutine creation)
	{"mutex", true},     // sema handoff
	{"broadcast", true}, // goready fan-out (close)
	{"chanring", false}, // channel rendezvous: HB pins the token path
}

// dstSchedStrategies are the exploration strategies the scheduling tests cover:
// the default random strategy and PCT (depth 3, with a step bound matched to the
// ~30-48 scheduling decisions these small scenarios take, so the priority-change
// points actually fire). Both must be deterministic, diverse, and sound.
var dstSchedStrategies = []struct {
	name  string
	extra []string
}{
	{"random", nil},
	{"pct", []string{"DSTPCT=3", "DSTPCTSTEPS=40"}},
}

// runSched runs a scheduling probe scenario under the given seed and strategy env.
func runSched(t *testing.T, exe, scenario, seed string, extra ...string) string {
	env := append(dstEnv(seed), "DSTSCENARIO="+scenario)
	env = append(env, extra...)
	return strings.TrimSpace(runBuiltTestProg(t, exe, "DSTSchedScenario", env...))
}

// TestDSTScheduleDeterministic verifies Seq-5 invariant DST-SCHED-2: the seeded
// interleaving is a reproducible function of the seed, for every strategy. For
// each scenario the same seed must yield an identical goroutine interleaving
// across runs. This is the unconditional logical-determinism layer of the DST
// contract, so it must hold in normal and -race builds alike. Mutation check:
// drawing the scheduling RNG from a load-dependent source (per-m rand, or a
// global the system goroutines advance) makes this fail.
func TestDSTScheduleDeterministic(t *testing.T) {
	exe, err := buildTestProg(t, "testprog", "-tags=dst")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range dstSchedStrategies {
		for _, sc := range dstSchedScenarios {
			first := runSched(t, exe, sc.name, "12345", st.extra...)
			if first == "" || first == "UNKNOWN_SCENARIO" {
				t.Fatalf("%s/%s: bad probe output %q", st.name, sc.name, first)
			}
			for i := 0; i < 4; i++ {
				if got := runSched(t, exe, sc.name, "12345", st.extra...); got != first {
					t.Fatalf("%s/%s: interleaving not deterministic across same-seed runs (run %d):\nfirst=%s\ngot  =%s",
						st.name, sc.name, i+1, first, got)
				}
			}
		}
	}
}

// TestDSTScheduleDiversity verifies the Seq-5 feature, for every strategy: the
// interleaving is seed-*varied*, so different seeds explore different sound
// interleavings (the completeness gain — before Seq 5 every seed produced the
// identical schedule). Freely-concurrent scenarios must produce many distinct
// interleavings; the channel-ring, whose token path is fixed by happens-before,
// must still vary (>1) but is not required to vary freely — diversity scales with
// real scheduling freedom. Mutation check: a constant scheduling draw (FIFO)
// collapses every scenario back to a single seed-invariant interleaving.
func TestDSTScheduleDiversity(t *testing.T) {
	exe, err := buildTestProg(t, "testprog", "-tags=dst")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range dstSchedStrategies {
		for _, sc := range dstSchedScenarios {
			seen := map[string]bool{}
			for _, s := range dstSchedSeeds {
				seen[runSched(t, exe, sc.name, s, st.extra...)] = true
			}
			if len(seen) <= 1 {
				t.Fatalf("%s/%s: interleaving is seed-invariant (%d distinct over %d seeds): the strategy has no effect",
					st.name, sc.name, len(seen), len(dstSchedSeeds))
			}
			if sc.freeDiverse && len(seen) < len(dstSchedSeeds)/2 {
				t.Fatalf("%s/%s: weak diversity (%d distinct over %d seeds), expected free concurrency to vary widely",
					st.name, sc.name, len(seen), len(dstSchedSeeds))
			}
		}
	}
}

// TestDSTScheduleSoundness verifies Seq-5 invariant DST-SCHED-1 for every
// strategy: the seam selects only among runnable goroutines, never one blocked on
// a primitive. A counter guarded solely by a sync.Mutex, incremented
// non-atomically inside the critical section, must reach its exact total for
// every seed — if the seam ever ran a goroutine blocked on the mutex (or
// corrupted the runq so two interleaved in the critical section), updates would
// be lost. Holds in normal and -race builds. Mutation check: dropping the
// runnable-set restriction (selecting a blocked G) makes the count wrong or crashes.
func TestDSTScheduleSoundness(t *testing.T) {
	exe, err := buildTestProg(t, "testprog", "-tags=dst")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range dstSchedStrategies {
		for _, s := range dstSchedSeeds {
			if got := runSched(t, exe, "mutexcount", s, st.extra...); got != "ok" {
				t.Fatalf("%s: soundness violation at seed %s: mutexcount=%s (the seam ran a blocked goroutine or corrupted the runq)", st.name, s, got)
			}
		}
	}
}

// TestDSTSchedulePCTChangePoints verifies PCT's priority-change points actually
// fire: with a step bound matched to the run length they deprioritize a running
// goroutine mid-run, producing a different interleaving than when the bound is so
// large the change points fall past the end of the run (degenerating to a fixed
// priority order). If the change-point mechanism were dead code, the two would be
// identical. Mutation check: never applying a change point makes them match.
func TestDSTSchedulePCTChangePoints(t *testing.T) {
	exe, err := buildTestProg(t, "testprog", "-tags=dst")
	if err != nil {
		t.Fatal(err)
	}
	differ := 0
	for _, s := range dstSchedSeeds {
		withCP := runSched(t, exe, "gosched", s, "DSTPCT=3", "DSTPCTSTEPS=40")
		noCP := runSched(t, exe, "gosched", s, "DSTPCT=3", "DSTPCTSTEPS=100000")
		if withCP != noCP {
			differ++
		}
	}
	if differ == 0 {
		t.Fatalf("PCT change points had no effect: matched-bound and out-of-range-bound runs were identical for all %d seeds (change-point deprioritization is dead)", len(dstSchedSeeds))
	}
}

// TestDSTProcessIdentity verifies the simulation fixes os.Getpid/os.Hostname to a
// deterministic identity inside Run (a default, or the Options value), and
// restores the real machine's identity afterward. Closes the determinism hole a
// SUT reading pid/hostname would otherwise have.
func TestDSTProcessIdentity(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTProcessIdentity"))
	const want = "def=1/sim custom=4242/node7 restored=true realoverridden=true"
	if out != want {
		t.Fatalf("process identity not simulated correctly:\n got=%q\nwant=%q", out, want)
	}
}

// TestDSTFinalizerChainNoLeak verifies the Run-end fixpoint drain resolves a
// finalizer chain whose tail touches a bubble channel fully in-bubble, so it does
// not leak to the post-Run reap and fatal (design.md D4: Run-end fixpoint).
// Mutation check: reverting the dstStopGCDrain fixpoint to a single drain makes
// the testprog fatal instead of printing "ok".
func TestDSTFinalizerChainNoLeak(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTFinChain", "DSTSEED=1", "GOGC=off"))
	if out != "ok" {
		t.Fatalf("finalizer chain not resolved in-bubble (got %q): a channel-touching "+
			"chain tail leaked to the post-Run reap", out)
	}
}
