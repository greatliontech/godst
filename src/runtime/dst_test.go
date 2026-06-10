// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"internal/platform"
	"internal/testenv"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

// runTestProgDSTNoRace builds the testprog with -tags=dst but NEVER -race (unlike
// runTestProgDST, whose buildTestProg appends -race when the outer test runs under
// -race). The DPOR brain-validation Explore tests use it: they validate the algorithm
// on the SUTs' CONTROLLED manual dstAccessYield/dstSyncAcquire transitions, which the
// dst-race compiler auto-instrumentation (active only under -race) would perturb by
// adding a yield at every memory access. Completeness/optimality/soundness of the
// brain are -race-independent; the auto-instrumentation path is exercised separately
// by TestDSTExploreAutoInstrument, and the data-race oracle by TestDSTExploreRaceOracle.
func runTestProgDSTNoRace(t *testing.T, name string, env ...string) string {
	t.Helper()
	testenv.MustHaveGoBuild(t)
	exe := filepath.Join(t.TempDir(), "tp_dst")
	buildTestProgExplicit(t, exe, "-tags=dst")
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
// rand.* top-level functions (e.g. randomized sampling, skiplist heights) and
// sync.Pool via runtime.randn.
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

// buildTestProgExplicit builds the testprog with the given flags verbatim, NOT
// inheriting the test binary's own -race mode (unlike buildTestProg, which
// appends -race when the test runs under -race). This lets a single test compare
// a normal-dst build against a -race-dst build.
func buildTestProgExplicit(t *testing.T, exe string, flags ...string) {
	t.Helper()
	cmd := exec.Command(testenv.GoToolPath(t), append([]string{"build", "-o", exe}, flags...)...)
	cmd.Dir = "testdata/testprog"
	cmd = testenv.CleanCmdEnv(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building testprog %v: %v\n%s", flags, err, out)
	}
}

// TestDSTMapHashKeyBuildInvariant verifies that the -tags dst global map hash key
// is position-independent, so map iteration order is identical across builds — in
// particular between a normal-dst build and a -race-dst build. The key is fixed
// by -tags dst (randinit seeds the global RNG from a constant), but deriving it
// from bootstrapRand drew from the global RNG at a startup *stream position* that
// -race shifts by one draw (composition/instrumentation varies the preceding
// draw count), so it was only fixed *per build*: a >=16-element map iterated in a
// different order under -race. alg.go now derives the key from a fixed constant
// (dstFixedHashKey), position-independently. Per-map m.seed still varies order by
// seed (TestDSTDeterministicMap); only this one global key is fixed, now
// identically across builds.
//
// Mutation check: reverting alg.go to fill the key from bootstrapRand makes the
// -race build's order differ from the normal build's, failing here.
func TestDSTMapHashKeyBuildInvariant(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the extra -race build")
	}
	testenv.MustHaveGoBuild(t)
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skipf("race detector not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testenv.MustHaveCGO(t) // -race requires cgo

	dir := t.TempDir()
	normalExe := filepath.Join(dir, "tp_normal")
	raceExe := filepath.Join(dir, "tp_race")
	buildTestProgExplicit(t, normalExe, "-tags=dst")
	buildTestProgExplicit(t, raceExe, "-tags=dst", "-race")

	// DSTMapOrder iterates a 48-element (multi-group) map, whose order depends on
	// the global hash key; single-group maps (<=8) place keys in insertion order
	// and are invariant regardless, so they would not detect a key shift.
	env := dstEnv("12345")
	normalOut := runBuiltTestProg(t, normalExe, "DSTMapOrder", env...)
	raceOut := runBuiltTestProg(t, raceExe, "DSTMapOrder", env...)
	if strings.TrimSpace(normalOut) == "" {
		t.Fatal("empty map order output")
	}
	if normalOut != raceOut {
		t.Fatalf("map iteration order differs between a normal-dst and a -race-dst build "+
			"(the -tags dst global hash key is not build-invariant):\nnormal=%q\n  race=%q",
			normalOut, raceOut)
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
// synctest) in the next. This is the pattern that otherwise requires a library
// pooling channels to allocate a fresh one per use. GOGC=off ensures the reap is
// the only thing that clears the pool.
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
// GC trigger (Tier 2, A.5) makes finalizer discovery deterministic at the
// contract granularity. A single-goroutine workload (no Seq-5 interleaving)
// allocates finalizable objects with varied lifetimes. The testprog prints
// "numGC total".
//
// The assertion is the set-level observable (DST-GC-1; see the testing/simulation
// package doc): the GC count and the total set of finalizers discovered are
// deterministic, -race-robust. (Which GC *cycle* discovers a given object — the
// per-cycle split — is also deterministic now, via the per-object dstHeapAlloc
// trigger, and is asserted separately by TestDSTGCPerCycleDiscoveryDeterministic;
// this test pins the coarser set level.) This test guards finalizer
// *set-level* determinism (the total discovered set is reproducible). The relative
// trigger the set level rests on is mutation-guarded separately by
// TestDSTMemoryLimit (its baseline-independence check fails if the dstHeapBase
// subtraction is dropped); this discovery workload's numGC is robust to that
// offset, so it is not the test that pins the baseline. (Multi-goroutine contended
// workloads carry a residual from the GC-independent Seq-5 scheduling-order gap;
// this test is single-goroutine to isolate the GC trigger.)
func TestDSTGCFinalizerDiscoveryDeterministic(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	first := strings.TrimSpace(runTestProgDST(t, "DSTGCFinDiscovery", env...))
	f := strings.Fields(first)
	if len(f) != 2 || f[0] == "0" || f[1] == "0" {
		t.Fatalf("no finalizer discovery recorded (%q): the in-run GC did not "+
			"discover finalizable objects", first)
	}
	for i := 0; i < 9; i++ {
		out := strings.TrimSpace(runTestProgDST(t, "DSTGCFinDiscovery", env...))
		if out != first {
			t.Fatalf("finalizer set-level discovery diverged (run %d): numGC/total "+
				"nondeterministic\nfirst=%q\ngot  =%q", i+1, first, out)
		}
	}
}

// TestDSTGCPerCycleDiscoveryDeterministic verifies that *per-cycle* finalizer
// discovery is deterministic, including under -race: the DST heap trigger fires on
// per-object allocated bytes (dstHeapAlloc), not span-granular heapLive, so *which*
// GC cycle discovers a given object is a reproducible function of the seed — not
// merely the GC set level (DSTGCFinDiscovery covers set-level). The testprog reads
// a mid-run partial finalizer-discovery count, which depends on the trigger
// crossings (unlike the run-end total, it is sensitive to *when* each cycle fires).
// The same seed must reproduce it across runs, for the floored (small live set) and
// GOGC-scaled (large pinned live set) regimes, in normal AND -race builds (the
// harness builds the testprog with -race when the test runs under -race).
//
// Mutation check: reverting the trigger to fire on heapLive (span-granular) instead
// of dstHeapAlloc makes the mid-run partial wobble run-to-run — the span crossing
// lands at a different allocation as the entry span-fill phase varies between
// process runs — so the same-seed runs diverge and this fails.
//
// (This asserts within-build replay determinism, the -race contract. The raw
// finqueued-based count also carries pre-bubble stdlib finalizers that survive the
// entry GC and die in-bubble, whose count varies between *builds* but is constant
// within one binary; a SUT observes its own finalizers, which are build-invariant.)
func TestDSTGCPerCycleDiscoveryDeterministic(t *testing.T) {
	for _, regime := range []struct {
		name string
		prog string
		env  []string
	}{
		{"floored", "DSTGCPerCycle", []string{"DSTSEED=12345", "GOGC=100"}},
		{"gogc-scaled", "DSTGCPerCycle", []string{"DSTSEED=12345", "GOGC=100", "DSTBIGLIVE=16"}},
		// MemoryLimit-governed (GOGC=off so the per-object limit crossing is the sole
		// trigger). A small finalizable rate + bulk non-finalizable garbage so the
		// limit fires without a finalizer-resurrection GC storm; the limit crossing
		// (bubbleMarked + dstHeapAlloc) is per-object, so the partial is reproducible.
		{"memlimit", "DSTMemLimitPerCycle", []string{"DSTSEED=12345", "GOGC=off", "DSTMEMLIMIT=4194304"}},
	} {
		first := strings.TrimSpace(runTestProgDST(t, regime.prog, regime.env...))
		f := strings.Fields(first)
		if len(f) != 2 || f[0] == "0" {
			t.Fatalf("%s: no mid-run per-cycle discovery recorded (%q)", regime.name, first)
		}
		for i := 0; i < 5; i++ {
			if got := strings.TrimSpace(runTestProgDST(t, regime.prog, regime.env...)); got != first {
				t.Fatalf("%s: per-cycle discovery not reproducible across same-seed runs (run %d): "+
					"the trigger crossing is not per-object deterministic\nfirst=%q\ngot  =%q",
					regime.name, i+1, first, got)
			}
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

// TestDSTIdentityExtra verifies the rest of the process-identity surface beyond
// pid/hostname is simulated deterministically inside Run and restored outside it:
// os.Getppid/Getuid/Getgid/Geteuid/Getegid, os/user.Current, and runtime.NumCPU
// (the last overridable via Options.NumCPU). Mutation check: dropping any
// dstSim* accessor branch in os/runtime changes the corresponding field.
func TestDSTIdentityExtra(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTIdentityExtra"))
	const want = "inside=[1 7777 7777 7777 7777 8 7777:7777:sim:/home/sim] customcpu=3 restoredids=true"
	if out != want {
		t.Fatalf("extended identity not simulated correctly:\n got=%q\nwant=%q", out, want)
	}
}

// TestDSTCryptoRandDeterministic verifies INV-CRYPTO: crypto/rand is a
// reproducible function of the seed inside Run (and only inside it). It asserts
// same-seed determinism (eq), that the stream varies with the seed (seedvaries,
// so it is not a constant), that two reads outside a run still differ (realdiffers
// — production crypto/rand is untouched), and that the seed-keyed output is
// identical across two separate processes (replay). This holds under -race
// (the per-g RNG drives it). Mutation check: making dstReadRandom return false
// (no fill) breaks eq and the cross-process replay; ignoring the seed breaks
// seedvaries.
func TestDSTCryptoRandDeterministic(t *testing.T) {
	out1 := strings.TrimSpace(runTestProgDST(t, "DSTCryptoRand", "DSTSEED=12345"))
	out2 := strings.TrimSpace(runTestProgDST(t, "DSTCryptoRand", "DSTSEED=12345"))
	if !strings.Contains(out1, " eq=true seedvaries=true realdiffers=true") {
		t.Fatalf("crypto/rand not deterministic/seed-varying/real-outside under DST: %q", out1)
	}
	if out1 != out2 {
		t.Fatalf("crypto/rand not reproducible across processes for the same seed:\nrun1=%q\nrun2=%q", out1, out2)
	}
}

// TestDSTNet verifies the in-memory deterministic network (the first I/O feature):
// inside simulation.Run a client/server exchange over net.Dial/Listen completes
// with the simulated addresses, replays byte-identically across processes, and the
// per-run registry resets between runs (the second of two in-process runs Listens
// on the same address without "address already in use"). Network I/O that cannot
// run deterministically — or at all in a sandbox — on the real OS is here a
// function of the seed. Mutation check: dropping the dstNetEpoch reset makes the
// second run fail "address already in use" (DIVERGED); not gating Dial/Listen on
// dstActive() makes it hit the real network (refused/hang).
func TestDSTNet(t *testing.T) {
	const want = "resp=echo:ping local=127.0.0.1:40000 remote=10.0.0.1:9000 | server saw ping from 127.0.0.1:40000"
	out1 := strings.TrimSpace(runTestProgDST(t, "DSTNet", "DSTSEED=42"))
	if out1 != want {
		t.Fatalf("in-memory net exchange wrong:\n got=%q\nwant=%q", out1, want)
	}
	out2 := strings.TrimSpace(runTestProgDST(t, "DSTNet", "DSTSEED=42"))
	if out2 != out1 {
		t.Fatalf("in-memory net not reproducible across processes:\nrun1=%q\nrun2=%q", out1, out2)
	}
}

// TestDSTSchedSystemIsolation verifies the system-goroutine-isolation invariant
// that keeps the schedule deterministic regardless of timing-/composition-varying
// runtime-infrastructure scheduling: under DST the scheduling RNG advances exactly
// once per *bubble* goroutine selection and never for a system (bubble==nil) one.
// So for a contended workload under the Random strategy, rngDraws == decisions -
// sysScheds, with sysScheds>0 (system goroutines are interleaved). Without isolation, system selections
// would draw from the bubble RNG, and how often they occur (timing/binary
// composition) would shift every subsequent selection — the nondeterminism a bare
// `import "net"` exposed (~1% of runs). Mutation check: making dstFindRunnable
// select system goroutines via dstSchedSelect (drawing RNG) makes rngDraws ==
// decisions != decisions - sysScheds.
func TestDSTSchedSystemIsolation(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTSchedStats", "DSTSEED=12345"))
	f := strings.Fields(out)
	if len(f) != 3 {
		t.Fatalf("bad stats output %q, want \"decisions sysScheds rngDraws\"", out)
	}
	decisions, _ := strconv.Atoi(f[0])
	sysScheds, _ := strconv.Atoi(f[1])
	rngDraws, _ := strconv.Atoi(f[2])
	if sysScheds <= 0 {
		t.Fatalf("no system goroutines scheduled (%q): the workload does not exercise the isolation", out)
	}
	if rngDraws != decisions-sysScheds {
		t.Fatalf("scheduling RNG drew for system goroutines (not isolated): rngDraws=%d, want decisions-sysScheds=%d-%d=%d",
			rngDraws, decisions, sysScheds, decisions-sysScheds)
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

// TestDSTMemoryLimit verifies Options.MemoryLimit deterministically bounds the
// bubble's heap growth (design.md D6: Options.MemoryLimit): a tighter limit forces
// more GCs, and the GC count is reproducible. It also guards the per-bubble
// relative trigger's dstHeapBase baseline (the mechanism that makes GC
// deterministic across processes by excluding the run-to-run-varying pre-bubble
// heap): numGC under a fixed limit must be independent of a large retained
// pre-bubble heap, because the trigger fires on heapLive-dstHeapBase. Mutation
// checks: (a) ignoring dstMemLimit makes the two limits produce the same count;
// (b) dropping the dstHeapBase baseline (absolute trigger) lets the pre-bubble
// heap inflate numGC, which the baseline-independence assertion catches (e.g. with
// a 16 MiB pre-bubble heap, numGC jumps from 9 to >16000).
func TestDSTMemoryLimit(t *testing.T) {
	run := func(limit, pre string) int {
		out := strings.TrimSpace(runTestProgDST(t, "DSTMemLimit", "DSTSEED=1", "GOGC=off",
			"DSTMEMLIMIT="+limit, "DSTPREBUBBLE="+pre))
		n, err := strconv.Atoi(out)
		if err != nil {
			t.Fatalf("bad NumGC output %q: %v", out, err)
		}
		return n
	}
	tight := run("2097152", "0") // 2 MiB
	loose := run("8388608", "0") // 8 MiB
	if tight < 2 {
		t.Fatalf("MemoryLimit did not bound the heap: tight-limit numGC=%d (heap grew unbounded?)", tight)
	}
	if tight <= loose {
		t.Fatalf("MemoryLimit had no effect: 2 MiB numGC=%d not greater than 8 MiB numGC=%d", tight, loose)
	}
	// Baseline independence: a 16 MiB heap retained before the run must not change
	// numGC — the relative trigger subtracts it (dstHeapBase). The absolute trigger
	// (baseline dropped) lets it inflate the live total and explodes numGC.
	if withPre := run("2097152", "16777216"); withPre != tight {
		t.Fatalf("MemoryLimit numGC depends on pre-bubble heap (the dstHeapBase baseline is "+
			"not subtracted): with 16 MiB retained pre-bubble numGC=%d, without=%d", withPre, tight)
	}
	for i := 0; i < 3; i++ {
		if got := run("2097152", "0"); got != tight {
			t.Fatalf("MemoryLimit numGC nondeterministic across runs (run %d): got %d, want %d", i+1, got, tight)
		}
	}
}

// TestDSTAccessYieldSound verifies the Level-2 access-granularity yield substrate is
// sound and deterministic (DST-L2-1/2). DSTYieldSound EXPLORES a mutex-protected
// non-atomic counter with a yield WHILE the lock is held: a sound seam never runs a
// goroutine blocked on Lock, so yielding inside a critical section preserves mutual
// exclusion and the counter reaches exactly G*K on every interleaving → zero failures.
// schedules>1 confirms the yield drove interleavings (not a vacuous pass). Built
// NON-race (access-granularity yielding is a scheduled-strategy mechanism; the dst-race
// compiler auto-instrumentation, active only under -race, would add unrelated yields).
// Deterministic across same-seed runs (DST-L2-2). See docs/dst/design.md "Level 2".
func TestDSTAccessYieldSound(t *testing.T) {
	out1 := runTestProgDSTNoRace(t, "DSTYieldSound", "DSTSEED=1")
	if exploreFailures(t, out1) != 0 {
		t.Fatalf("access-granularity yield is unsound: a goroutine blocked on Lock was run "+
			"inside a critical section (lost update): %q", out1)
	}
	if n := exploreSchedules(t, out1); n <= 1 {
		t.Fatalf("yield-while-locked test is vacuous: only %d schedule(s) — the access-yield "+
			"did not drive interleavings: %q", n, out1)
	}
	if !strings.Contains(out1, "exhausted=true") {
		t.Fatalf("yield-while-locked interleaving space not exhausted: %q", out1)
	}
	if out2 := runTestProgDSTNoRace(t, "DSTYieldSound", "DSTSEED=1"); out1 != out2 {
		t.Fatalf("access-granularity yield not deterministic across same-seed runs:\n run1=%q\n run2=%q", out1, out2)
	}
}

// TestDSTExploreFindsAtomicityViolation verifies the systematic explorer finds, in
// a single Explore call, the atomicity violation that the coarse random/PCT
// strategies miss for every seed (0/200). It also confirms the explorer is sound
// (the mutex-protected counter SUT yields no failing interleaving) and
// deterministic. See docs/dst/design.md (Level 2, DST-L2-1/2).
func TestDSTExploreFindsAtomicityViolation(t *testing.T) {
	out := runTestProgDSTNoRace(t, "DSTExplore", "DSTSEED=1", "DSTEXPLORE=atomicity", "DSTMODE=dpor")
	if exploreFailures(t, out) == 0 {
		t.Fatalf("Explore did not find the atomicity violation that requires a mid-gap "+
			"interleaving: %q", out)
	}
	if !strings.Contains(out, "exhausted=true") || !strings.Contains(out, "overflow=false") {
		t.Fatalf("Explore did not cleanly exhaust the atomicity SUT's interleaving space: %q", out)
	}
	if out2 := runTestProgDSTNoRace(t, "DSTExplore", "DSTSEED=1", "DSTEXPLORE=atomicity", "DSTMODE=dpor"); out != out2 {
		t.Fatalf("Explore not deterministic across same-seed runs:\n run1=%q\n run2=%q", out, out2)
	}
	// Soundness: the mutex-protected counter has NO buggy interleaving — a sound
	// explorer reports zero failures over the whole space, under both modes.
	for _, mode := range []string{"dpor", "exhaustive"} {
		s := runTestProgDSTNoRace(t, "DSTExplore", "DSTSEED=1", "DSTEXPLORE=mutexcount", "DSTMODE="+mode)
		if exploreFailures(t, s) != 0 {
			t.Fatalf("explorer (%s) reported a spurious failure on the sound mutex counter "+
				"(ran a blocked goroutine?): %q", mode, s)
		}
	}
}

// TestDSTExploreComplete verifies DPOR is COMPLETE — it reaches the identical set
// of reachable outcomes as exhaustive enumeration, while exploring no more
// interleavings. If DPOR's outcome set were a subset, it would be silently missing
// reachable states (bugs). See docs/dst/design.md (Level 2, DST-L2-3). The larger
// generated sweep below carries the strict reduction/optimality guard; this tiny SUT's
// exhaustive tree becomes minimal once shared-address filtering removes private steps.
func TestDSTExploreComplete(t *testing.T) {
	exh := runTestProgDSTNoRace(t, "DSTExploreOutcomes", "DSTSEED=1", "DSTMODE=exhaustive")
	dpor := runTestProgDSTNoRace(t, "DSTExploreOutcomes", "DSTSEED=1", "DSTMODE=dpor")
	exhSet, dporSet := exploreOutcomes(t, exh), exploreOutcomes(t, dpor)
	if exhSet != dporSet {
		t.Fatalf("DPOR is incomplete: reaches outcomes %q but exhaustive reaches %q", dporSet, exhSet)
	}
	exhN, dporN := exploreSchedules(t, exh), exploreSchedules(t, dpor)
	if dporN > exhN {
		t.Fatalf("DPOR explored more interleavings than exhaustive: dpor=%d, exhaustive=%d", dporN, exhN)
	}
}

// exploreField returns the value of "key=" in an Explore output line.
func exploreField(t *testing.T, out, key string) string {
	t.Helper()
	for _, f := range strings.Fields(out) {
		if rest, ok := strings.CutPrefix(f, key+"="); ok {
			return rest
		}
	}
	t.Fatalf("Explore output missing %q=: %q", key, out)
	return ""
}

func exploreFailures(t *testing.T, out string) int {
	t.Helper()
	n, err := strconv.Atoi(exploreField(t, out, "failures"))
	if err != nil {
		t.Fatalf("bad failures field in %q: %v", out, err)
	}
	return n
}

func exploreSchedules(t *testing.T, out string) int {
	t.Helper()
	n, err := strconv.Atoi(exploreField(t, out, "schedules"))
	if err != nil {
		t.Fatalf("bad schedules field in %q: %v", out, err)
	}
	return n
}

// exploreOutcomes returns the "outcomes=[...]" set as a canonical string.
func exploreOutcomes(t *testing.T, out string) string {
	t.Helper()
	i := strings.Index(out, "outcomes=[")
	if i < 0 {
		t.Fatalf("Explore output missing outcomes=[...]: %q", out)
	}
	rest := out[i+len("outcomes="):]
	j := strings.IndexByte(rest, ']')
	if j < 0 {
		t.Fatalf("Explore output has unterminated outcomes=[: %q", out)
	}
	return rest[:j+1]
}

// TestDSTExploreHBPrunes verifies that DPOR's happens-before pruning works: in
// twoPairSUT two producer/consumer pairs interleave freely, but each pair's shared
// access is channel-ordered (not a race). A DPOR that recognizes the
// happens-before order prunes the futile reorderings of those ordered accesses and
// explores 4 schedules with no failures; the address-only relation (no HB)
// over-explores them (21). The <=10 bound passes with HB pruning (4) and fails
// without it (21), so it has teeth. See docs/dst/design.md (Level 2, increment 2).
func TestDSTExploreHBPrunes(t *testing.T) {
	out := runTestProgDSTNoRace(t, "DSTExplore", "DSTSEED=1", "DSTEXPLORE=twopair", "DSTMODE=dpor")
	if exploreFailures(t, out) != 0 {
		t.Fatalf("explorer reported a spurious race on channel-ordered (not racing) accesses: %q", out)
	}
	if !strings.Contains(out, "exhausted=true") {
		t.Fatalf("explorer did not exhaust twoPairSUT's space: %q", out)
	}
	if n := exploreSchedules(t, out); n > 10 {
		t.Fatalf("happens-before pruning ineffective: explored %d schedules (want <=10; "+
			"the address-only relation explores ~21): %q", n, out)
	}
}

// TestDSTExploreSweep is the DST-L2-3 completeness guard: a generated family of
// small concurrent programs (2-3 goroutines; reads/writes over 1-2 shared vars;
// with and without mutex synchronization) plus hand-written hard SUTs (a channel
// rendezvous-order choice) is explored under BOTH DPOR and brute-force Exhaustive,
// and DPOR must reach the IDENTICAL set of observable outcomes for every one. This
// is the real net that the micro-SUTs (TestDSTExploreComplete, a single no-mutex
// program) only weakly approximate.
//
// It specifically guards the synchronization-acquisition-order classes: WHICH
// goroutine acquires a mutex / rendezvous on a channel first is a real scheduling
// choice that changes the outcome, but it occurs at a transition recording no
// memory access, so DPOR drops one order unless each acquisition is recorded as a
// conflicting transition (runtime.dstSyncAcquire). With that hook neutered the
// sweep fails 23/289; with it, 0 — so it has teeth. See docs/dst/design.md
// (Level 2, DST-L2-3 + "Completeness boundary").
//
// It ALSO guards source-DPOR OPTIMALITY: the sweep reports maxDpor, the largest
// per-program DPOR schedule count. Full source-DPOR (sleep sets + weak-initial source
// sets) holds maxDpor=69 on this family. Mutation-measured regressions: dropping
// sleep (weak-initials only) → 85; dropping both → the persistent-set search → 125.
// The <80 bound below therefore catches a regression of EITHER mechanism (the
// remaining one, dropping weak-initials, makes the search incomplete and is caught by
// mismatches instead). Completeness (mismatches=0) is the hard invariant; this is the
// optimality regression catch. maxDpor is deterministic for the fixed family+seed.
//
// Built WITHOUT -race: completeness is a property of the DPOR algorithm, not the
// detector, and the no-mutex SUTs contain intentional data races the detector would
// otherwise report. Skipped under -short (it is exhaustive over the family);
// TestDSTExploreComplete still provides basic completeness coverage there.
func TestDSTExploreSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the exhaustive-equivalence completeness sweep")
	}
	testenv.MustHaveGoBuild(t)
	exe := filepath.Join(t.TempDir(), "tp_dst")
	buildTestProgExplicit(t, exe, "-tags=dst")
	out := runBuiltTestProg(t, exe, "DSTExploreSweep", "DSTSEED=1")
	if exploreField(t, out, "mismatches") != "0" {
		t.Fatalf("DPOR completeness sweep found mismatches vs exhaustive enumeration "+
			"(a dropped Mazurkiewicz class — DST-L2-3 violation):\n%s", out)
	}
	if maxDpor, err := strconv.Atoi(exploreField(t, out, "maxDpor")); err != nil {
		t.Fatalf("bad maxDpor field in %q: %v", out, err)
	} else if maxDpor >= 80 {
		t.Fatalf("DPOR optimality regressed: maxDpor=%d (full source-DPOR holds 69; "+
			"dropping sleep sets → 85, dropping to the persistent-set search → 125):\n%s", maxDpor, out)
	}
}

// TestDSTExploreRaceOracle verifies the -race detector works as Explore's data-race
// oracle (D5): an explored interleaving that exhibits an unsynchronized access pair
// is reported as a Failure with Race=true, with replay metadata that reproduces it. The
// SUT (raceOracleSUT) has two unsynchronized writes and NO assertion, so a data race
// is the ONLY possible finding — proving Explore surfaces races, not just SUT
// assertions. The detector fires even at the GOMAXPROCS=1 serial execution DST uses
// (it is clock-based, not timing-based), and the verdict is a deterministic function
// of (seed, schedule): two same-seed runs report the identical first-race schedule.
//
// Built explicitly WITH -race (the oracle is race-only; dstRaceErrors returns 0
// otherwise). Skipped where the race detector is unavailable. The testprog exits
// nonzero (race detector exit) and prints a DATA RACE report to stderr; runBuiltTestProg
// ignores the exit code, and the assertions parse only the deterministic "raceoracle"
// summary fields (not the address-bearing report). See docs/dst/design.md (Level 2, D5).
func TestDSTExploreRaceOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the -race oracle build")
	}
	testenv.MustHaveGoBuild(t)
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skipf("race detector not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testenv.MustHaveCGO(t) // -race requires cgo
	exe := filepath.Join(t.TempDir(), "tp_race")
	buildTestProgExplicit(t, exe, "-tags=dst", "-race")
	// uncond: an unconditional write-write race — the oracle must fire (proves -race
	// works as the oracle under simulation.Run's GOMAXPROCS=1 serial execution).
	// cond: an INTERLEAVING-CONDITIONAL race manifesting only when the reader acquires
	// the mutex first — the explorer must reach that schedule (via the sync-decision
	// machinery) for the oracle to see it; a coarse scheduler would miss it on the other
	// acquisition order.
	for _, mode := range []string{"uncond", "cond"} {
		out := runBuiltTestProg(t, exe, "DSTExploreRaceOracle", "DSTSEED=1", "DSTRACE="+mode)
		if races, err := strconv.Atoi(exploreField(t, out, "races")); err != nil {
			t.Fatalf("bad races field in %q: %v", out, err)
		} else if races < 1 {
			t.Fatalf("race oracle (%s) found no data race (D5 oracle not firing under "+
				"simulation.Run / explorer did not reach the racy interleaving):\n%s", mode, out)
		}
		// The first-race schedule is a deterministic function of the seed (DST-L2-2 +
		// D5). Compare the parsed schedule, not the full output, whose race report
		// carries nondeterministic addresses.
		out2 := runBuiltTestProg(t, exe, "DSTExploreRaceOracle", "DSTSEED=1", "DSTRACE="+mode)
		if a, b := exploreField(t, out, "firstrace"), exploreField(t, out2, "firstrace"); a != b {
			t.Fatalf("race oracle (%s) nondeterministic: first-race schedule %q vs %q", mode, a, b)
		}
	}
}

// TestDSTExploreRaceReplay verifies a race failure first observed under replay-promoted
// access forces carries a complete replay token. The replay runs in a fresh process so
// TSan's process-global report dedup does not mask the reproduced race.
func TestDSTExploreRaceReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the -race replay build")
	}
	testenv.MustHaveGoBuild(t)
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skipf("race detector not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testenv.MustHaveCGO(t) // -race requires cgo
	exe := filepath.Join(t.TempDir(), "tp_race")
	buildTestProgExplicit(t, exe, "-tags=dst", "-race")
	out := runBuiltTestProg(t, exe, "DSTExploreRaceReplay", "DSTSEED=1")
	if races, err := strconv.Atoi(exploreField(t, out, "races")); err != nil {
		t.Fatalf("bad races field in %q: %v", out, err)
	} else if races < 1 {
		t.Fatalf("race replay fixture did not find a race failure:\n%s", out)
	}
	if n, err := strconv.Atoi(exploreField(t, out, "forcecount")); err != nil {
		t.Fatalf("bad forcecount field in %q: %v", out, err)
	} else if n == 0 {
		t.Fatalf("race failure did not carry promoted access forces:\n%s", out)
	}
	schedule := exploreField(t, out, "schedule")
	forces := exploreField(t, out, "forces")
	if schedule == "_" || forces == "_" {
		t.Fatalf("race failure replay token incomplete: schedule=%q forces=%q\n%s", schedule, forces, out)
	}
	replay := runBuiltTestProg(t, exe, "DSTExploreRaceReplay", "DSTSEED=1", "DSTREPLAY=1", "DSTSCHEDULE="+schedule, "DSTFORCES="+forces)
	if exploreField(t, replay, "raced") != "true" {
		t.Fatalf("race failure replay did not reproduce the race:\nexplore=%s\nreplay=%s", out, replay)
	}
}

// TestDSTExploreAutoInstrument is the increment-1 acceptance: the dst-race compiler
// mode auto-inserts a dstAccessYield before each -race memory-access hook, so an
// UNMODIFIED SUT (no manual dstAccessYield/dstSyncAcquire) becomes explorable.
// unmodifiedRMWSUT is two goroutines doing an unsynchronized read-modify-write of a
// shared counter; the lost update (final != 2) is reachable ONLY because the compiler
// made the reads/writes interleavable. The test asserts:
//   - assertfail >= 1: Explore found the lost-update interleaving with no hand
//     annotation (auto-instrumentation feeds the explorer end-to-end).
//   - complete == true: DPOR's reachable-outcome set equals brute-force Exhaustive's.
//     The dense auto-instrumentation exercises replay-promoted filtered accesses and
//     race-enabled conservative conflict backtracking, which the non-race family sweep
//     does not reach.
//   - exh/noiseExh stay tractable: shared-address filtering removed private and
//     HB-ordered access yields from the auto-instrumented transition set. The plain
//     RMW was measured at ~19k exhaustive schedules before filtering.
//   - manualRWRComplete/outcomes guard the source-DPOR weak-initial prologue case
//     exposed by filtering: a zero-address scheduling prologue must not mask the real
//     next access. This SUT uses //go:norace helpers so the -race binary has the
//     runtime filter active while keeping the access stream hand-controlled.
//   - createComplete/outcomes guard the post-go first-access case: a parent write
//     immediately after creating a child must still allow the child-before-write order
//     even though the parent write has no prior conflicting access in that run.
//   - wakeComplete/outcomes is the same guard for a child made runnable by close(ch):
//     after the close wakes it, the child may run before the parent's following write.
//
// Built explicitly WITH -race (auto-instrumentation is gated on -tags dst + -race).
// Skipped where the race detector is unavailable.
func TestDSTExploreAutoInstrument(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the dst-race auto-instrumentation build")
	}
	testenv.MustHaveGoBuild(t)
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skipf("race detector not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testenv.MustHaveCGO(t) // -race requires cgo
	exe := filepath.Join(t.TempDir(), "tp_race")
	buildTestProgExplicit(t, exe, "-tags=dst", "-race")
	out := runBuiltTestProg(t, exe, "DSTExploreAuto", "DSTSEED=1")
	if a, err := strconv.Atoi(exploreField(t, out, "assertfail")); err != nil {
		t.Fatalf("bad assertfail field in %q: %v", out, err)
	} else if a < 1 {
		t.Fatalf("compiler auto-instrumentation did not feed the explorer: the unmodified "+
			"RMW SUT's lost-update interleaving was not found (assertfail=0):\n%s", out)
	}
	if exploreField(t, out, "complete") != "true" {
		t.Fatalf("source-DPOR dropped a class on the auto-instrumented SUT (the "+
			"no-enabled-weak-initial fallback is not complete — DST-L2-3):\n%s", out)
	}
	if n, err := strconv.Atoi(exploreField(t, out, "outcomes")); err != nil {
		t.Fatalf("bad outcomes field in %q: %v", out, err)
	} else if n != 2 {
		t.Fatalf("shared-address filtering changed the RMW outcome set: outcomes=%d, want 2:\n%s", n, out)
	}
	if n, err := strconv.Atoi(exploreField(t, out, "exh")); err != nil {
		t.Fatalf("bad exh field in %q: %v", out, err)
	} else if n >= 1000 {
		t.Fatalf("shared-address filtering did not control the RMW exhaustive explosion: exh=%d, want <1000:\n%s", n, out)
	}
	if exploreField(t, out, "noiseComplete") != "true" {
		t.Fatalf("source-DPOR dropped a class on the private-noise auto-instrumented SUT:\n%s", out)
	}
	if n, err := strconv.Atoi(exploreField(t, out, "noiseOutcomes")); err != nil {
		t.Fatalf("bad noiseOutcomes field in %q: %v", out, err)
	} else if n != 2 {
		t.Fatalf("shared-address filtering changed the private-noise RMW outcome set: outcomes=%d, want 2:\n%s", n, out)
	}
	if n, err := strconv.Atoi(exploreField(t, out, "noiseExh")); err != nil {
		t.Fatalf("bad noiseExh field in %q: %v", out, err)
	} else if n >= 1000 {
		t.Fatalf("shared-address filtering did not control the private-noise exhaustive explosion: noiseExh=%d, want <1000:\n%s", n, out)
	}
	if exploreField(t, out, "rwrComplete") != "true" {
		t.Fatalf("source-DPOR dropped a filtered R/W/R class (weak-initial prologue bug):\n%s", out)
	}
	if n, err := strconv.Atoi(exploreField(t, out, "rwrOutcomes")); err != nil {
		t.Fatalf("bad rwrOutcomes field in %q: %v", out, err)
	} else if n != 4 {
		t.Fatalf("filtered R/W/R outcome set changed: outcomes=%d, want 4:\n%s", n, out)
	}
	if exploreField(t, out, "manualRWRComplete") != "true" {
		t.Fatalf("source-DPOR dropped a filtered manual R/W/R class (weak-initial prologue bug):\n%s", out)
	}
	if n, err := strconv.Atoi(exploreField(t, out, "manualRWROutcomes")); err != nil {
		t.Fatalf("bad manualRWROutcomes field in %q: %v", out, err)
	} else if n != 4 {
		t.Fatalf("filtered manual R/W/R outcome set changed: outcomes=%d, want 4:\n%s", n, out)
	}
	if exploreField(t, out, "createComplete") != "true" {
		t.Fatalf("source-DPOR dropped a post-go first-access class:\n%s", out)
	}
	if n, err := strconv.Atoi(exploreField(t, out, "createOutcomes")); err != nil {
		t.Fatalf("bad createOutcomes field in %q: %v", out, err)
	} else if n != 2 {
		t.Fatalf("post-go first-access outcome set changed: outcomes=%d, want 2:\n%s", n, out)
	}
	if exploreField(t, out, "wakeComplete") != "true" {
		t.Fatalf("source-DPOR dropped a post-wake continuation class:\n%s", out)
	}
	if n, err := strconv.Atoi(exploreField(t, out, "wakeOutcomes")); err != nil {
		t.Fatalf("bad wakeOutcomes field in %q: %v", out, err)
	} else if n != 2 {
		t.Fatalf("post-wake continuation outcome set changed: outcomes=%d, want 2:\n%s", n, out)
	}
}

// TestDSTExploreSyncAutoInstrument is the acceptance for runtime sync-decision
// auto-hooks (deferral 1): an UNMODIFIED SUT whose outcome depends on lock /
// rendezvous/release/close decision order, built -tags dst -race, must have DPOR
// reach BOTH decision outcomes with NO manual dstSyncAcquire. The compiler
// auto-instruments shared memory accesses, but the sync-object decision is an addr=0
// transition and the in-section accesses are object-serialized (HB-ordered, not a
// reorderable race), so DPOR keeps only one order UNLESS the runtime auto-hooks the
// decision itself (mutex Lock/TryLock/Unlock, failed TryLock/TryRLock decisions, RWMutex
// reader/writer admission and release, channel ops/close, blocking and non-blocking
// select channel cases, and Once's mutex-backed first execution path →
// dstSyncAcquire).
//
// The oracle is DPOR-only against a construction-known ground truth, not a
// DPOR-vs-Exhaustive comparison: under -race the compiler instruments every memory
// access, so Exhaustive enumerates the access-granularity explosion (a trivial RMW
// already hits ~19k schedules) and is intractable here until shared-address filtering
// lands — so that cross-check belongs with the filtering increment. Each SUT is two
// symmetric goroutines contending over one object decision, which has EXACTLY two
// outcomes, so the test asserts for each sync-decision SUT:
//   - Exhausted == true: DPOR cleanly finished (not budget-truncated).
//   - Outcomes == 2: both decision outcomes were reached (DST-L2-3 for this shape).
//     With the runtime sync hook neutered DPOR finds 1 — the teeth.
//
// Built explicitly WITH -race (the sync auto-hooks are gated on -tags dst + -race,
// the same gate as the memory auto-instrumentation, preserving DST-L2-4). Skipped
// where the race detector is unavailable.
func TestDSTExploreSyncAutoInstrument(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the dst-race sync-decision auto-instrumentation build")
	}
	testenv.MustHaveGoBuild(t)
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skipf("race detector not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testenv.MustHaveCGO(t) // -race requires cgo
	exe := filepath.Join(t.TempDir(), "tp_race")
	buildTestProgExplicit(t, exe, "-tags=dst", "-race")
	out := runBuiltTestProg(t, exe, "DSTExploreSyncAuto", "DSTSEED=1")
	for _, sut := range []string{"mutex", "chan", "rwmutex", "tryrlockfail", "tryrlockrelease", "trywlockrelease", "trylock", "trylockfail", "trylockrelease", "selectsend", "selectblocksend", "selectnbsend", "selectrecv", "selectblockrecv", "selectnbrecv", "chanclose", "once"} {
		if exploreField(t, out, sut+"Exhausted") != "true" {
			t.Fatalf("unmodified %s SUT did not cleanly exhaust under DPOR (budget "+
				"truncation, not a clean acceptance):\n%s", sut, out)
		}
		if n, err := strconv.Atoi(exploreField(t, out, sut+"Outcomes")); err != nil {
			t.Fatalf("bad %sOutcomes field in %q: %v", sut, out, err)
		} else if n != 2 {
			t.Fatalf("runtime %s sync-decision auto-hook missing or ineffective: DPOR reached "+
				"%d decision outcomes on the unmodified %s SUT, want 2 (both outcomes). Without "+
				"the auto-hook the sync-object decision is an addr=0 transition DPOR cannot reverse, "+
				"so it finds 1 — DST-L2-3:\n%s", sut, n, sut, out)
		}
	}
}
