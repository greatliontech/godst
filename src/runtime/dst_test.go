// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"internal/platform"
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
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

// runTestProgNetDST builds testprognet with -tags dst and runs the named
// function. The DST net testprogs live in testprognet, not testprog: importing
// net links cgo into the binary, and a cgo binary disables the runtime's
// deadlock detection, which testprog's crash tests depend on.
func runTestProgNetDST(t *testing.T, name string, env ...string) string {
	exe, err := buildTestProg(t, "testprognet", "-tags=dst")
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
// different order under -race. internal/runtime/maps now derives the key from a
// fixed constant (dstFixedHashKey), position-independently. Per-map m.seed still
// varies order by seed (TestDSTDeterministicMap); only this one global key is
// fixed, now identically across builds.
//
// Mutation check: reverting maps.AlgInit to fill the key from bootstrapRand makes
// the -race build's order differ from the normal build's, failing here.
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

// TestDSTAccessYieldBuildModeInert verifies the Level-2 build-mode boundary: user
// code gets compiler-inserted runtime.dstAccessYield and runtime.dstAccessYieldRange
// calls only when BOTH -tags dst and -race are present. Other build modes may carry
// inert runtime DST state in this fork, but they must not emit Level-2 access-yield
// hooks into user code.
func TestDSTAccessYieldBuildModeInert(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips build-mode objdump checks")
	}
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := []byte(`package main

import "sync/atomic"

var scalarSink int
var rangeSink [32]byte
var rangeSource [32]byte
var atomicSink atomic.Int32
var atomicWord int32

//go:noinline
func touch() {
	scalarSink++
	rangeSink = rangeSource
	atomic.AddInt32(&atomicWord, 1)
	atomicSink.Store(2)
}

func main() {
	touch()
}
`)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), src, 0o666); err != nil {
		t.Fatal(err)
	}
	type dstAccessHooks struct {
		scalar bool
		range_ bool
		atomic bool
	}
	dstAccessHookPresence := func(name string, flags ...string) dstAccessHooks {
		t.Helper()
		exe := filepath.Join(dir, name)
		args := append([]string{"build", "-o", exe}, flags...)
		args = append(args, "main.go")
		cmd := exec.Command(testenv.GoToolPath(t), args...)
		cmd.Dir = dir
		cmd = testenv.CleanCmdEnv(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s %v: %v\n%s", name, flags, err, out)
		}

		cmd = exec.Command(testenv.GoToolPath(t), "tool", "objdump", "-s", "main.touch", exe)
		cmd = testenv.CleanCmdEnv(cmd)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("objdumping %s: %v\n%s", name, err, out)
		}
		text := string(out)
		return dstAccessHooks{
			scalar: strings.Contains(text, "runtime.dstAccessYield(SB)"),
			range_: strings.Contains(text, "runtime.dstAccessYieldRange(SB)"),
			atomic: strings.Contains(text, "runtime.dstAtomicYield(SB)"),
		}
	}
	assertNoDSTAccessHooks := func(name string, flags ...string) {
		t.Helper()
		if got := dstAccessHookPresence(name, flags...); got.scalar || got.range_ || got.atomic {
			t.Fatalf("%s build emitted DST access hooks: scalar=%v range=%v atomic=%v", name, got.scalar, got.range_, got.atomic)
		}
	}
	assertDSTAccessHooks := func(name string, flags ...string) {
		t.Helper()
		if got := dstAccessHookPresence(name, flags...); !got.scalar || !got.range_ || !got.atomic {
			t.Fatalf("%s build missing DST hooks: scalar=%v range=%v atomic=%v", name, got.scalar, got.range_, got.atomic)
		}
	}

	assertNoDSTAccessHooks("plain")
	assertNoDSTAccessHooks("dst", "-tags=dst")
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skipf("race detector not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testenv.MustHaveCGO(t) // -race requires cgo
	assertNoDSTAccessHooks("race", "-race")
	assertDSTAccessHooks("dst_race", "-tags=dst", "-race")
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

// TestDSTPooledFinalizerRunEndInBubble verifies that the run-end sync.Pool reap
// is part of the in-bubble drain. The testprog has one run-end finalizer perform
// a Pool.Put of a finalizer object whose callback does another Pool.Put; the
// post-callback pool generation window must reset so the channel-touching tail
// still runs inside the bubble. Use the non-race builder because race-mode
// sync.Pool.Put may randomly drop values, bypassing the pool-generation path.
func TestDSTPooledFinalizerRunEndInBubble(t *testing.T) {
	out := runTestProgDSTNoRace(t, "DSTPooledFinalizerRunEnd", "DSTSEED=1", "GOGC=off")
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("pooled finalizer escaped in-bubble run-end drain (got %q, want %q)", out, "ok\n")
	}
}

// TestDSTPooledCleanupRunEndInBubble is the cleanup analogue of
// TestDSTPooledFinalizerRunEndInBubble.
func TestDSTPooledCleanupRunEndInBubble(t *testing.T) {
	out := runTestProgDSTNoRace(t, "DSTPooledCleanupRunEnd", "DSTSEED=1", "GOGC=off")
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("pooled cleanup escaped in-bubble run-end drain (got %q, want %q)", out, "ok\n")
	}
}

// TestDSTGCAllocBoundDeterministic verifies the two STW-forcing-increment guarantees for an
// alloc-heavy, non-blocking SUT (design dimension 11): GC is enabled in-run and
// bounds memory, and its observable effect is deterministic. The testprog churns
// ~60MB across four non-blocking goroutines inside dst.Run with GOGC=100, so only
// the heap trigger (not synctest quiescence) can fire GC.
//
// Two assertions, with distinct teeth:
//   - numGC>0 — the reliable teeth for the core change (GC enabled in-run).
//     Disabling GC (GOGC=off, or reverting dst.Run to force GC off) makes numGC=0
//     and fails here: memory would be unbounded.
//   - "<sum> <numGC>" identical across runs — observable determinism. With STW
//     (Tier 2, D2) the GC count is stable; concurrent GC lets wall-clock-timed
//     floating garbage flip numGC ±1 (GOGC=100 churn: 20 vs 21), which several
//     samples here catch. This is a secondary guard: STW's primary, reliable teeth
//     is deterministic finalizer/weak discovery, tested with the quiescence drain. Note the exact
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

// TestDSTGCCleanupOrderRegistration is the H6 cleanup regression: the bubble drain runs
// its cleanup batch in registration-sequence order (cleanupFn.dstSeq), so the id-0
// cleanup runs first. Teeth: without the cross-block reg-seq sort the drain runs blocks
// in `full`-stack LIFO order, so the last-registered (highest-id) block runs first and
// the first-run id is far from 0.
func TestDSTGCCleanupOrderRegistration(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTCleanupOrder", "DSTSEED=12345"))
	if out != "0" {
		t.Errorf("first cleanup to run was id %s, want 0: the cleanup drain is not running in registration order (sweep/block-LIFO order)", out)
	}
}

// TestDSTGCPoolCarryoverDeterministic is the M4 regression: the DST heap trigger
// excludes runtime-internal pooled allocations (g, sudog, _defer), so a second
// in-process run's inherited g/sudog pools do not shift the GC cycle boundary. The
// testprog runs the same goroutine+channel+finalizer program twice at one seed; both
// runs' mid-run per-cycle finalizer discovery (and total) must match. Teeth: without
// the exclusion, run 2 reuses ~3000 pooled g's where run 1 allocated, moving
// dstHeapAlloc by ~MB so the per-cycle count diverges.
func TestDSTGCPoolCarryoverDeterministic(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTGCPoolCarryover", "DSTSEED=12345", "GOGC=100"))
	f := strings.Fields(out)
	if len(f) != 4 {
		t.Fatalf("DSTGCPoolCarryover: want 4 fields (partial1 partial2 total1 total2), got %q", out)
	}
	if f[0] == "0" {
		t.Fatalf("no mid-run per-cycle discovery recorded (%q)", out)
	}
	if f[0] != f[1] {
		t.Errorf("per-cycle discovery differs between two in-process runs (%s vs %s): an inherited g/sudog "+
			"pool shifted the trigger — internal pooled allocations are not excluded from dstHeapAlloc", f[0], f[1])
	}
	if f[2] != f[3] {
		t.Errorf("total finalizer discovery differs between two in-process runs (%s vs %s)", f[2], f[3])
	}
}

// TestDSTGCDeferPoolCarryoverDeterministic reaches the heap-lowered _defer arm
// of the pooled-struct cancellation independently of g and sudog churn. The
// first same-seed run allocates loop defers fresh; the second reuses them. Both
// the per-object trigger exclusion and dstPooledMarked subtraction are required
// for their per-cycle finalizer-discovery fingerprints to match.
//
// TestDSTPooledDeferAccounting independently pins that the loop defers reach
// both the pooled allocation counter and its marked-side snapshot; this test
// asserts the resulting cold/warm behavior end to end.
func TestDSTGCDeferPoolCarryoverDeterministic(t *testing.T) {
	out := runTestProgDST(t, "DSTGCDeferPoolCarryover", "DSTSEED=12345", "GOGC=100")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("heap-defer pool moved the DST trigger (got %q, want \"done\")", out)
	}
}

// TestDSTFinalizerBlockedDrainQuiescence verifies the drain wake guard:
// when a finalizer blocks on a bubble channel, the drain is parked inside the
// channel wait, and a later quiescence with finalizer work still pending must
// not goready it there.
//
// Teeth: with the dstDrainParked check removed from dstDrainAtQuiescence, the
// driver's wake corrupts the channel wait queue and the run dies with "fatal
// error: runtime: sudog with non-nil elem" instead of printing "done".
func TestDSTFinalizerBlockedDrainQuiescence(t *testing.T) {
	out := runTestProgDST(t, "DSTFinBlockedDrain", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("blocked-drain quiescence failed (got %q, want \"done\")", out)
	}
}

// TestDSTFinalizerGoexitDrain verifies drain-death handling: a
// finalizer that calls runtime.Goexit kills the drain; the driver must never
// wake the dead g again, and callbacks queued after the death — including
// bubble-channel-touching ones — are deterministically discarded in-run.
//
// Teeth: without the gdestroy clear, the next quiescence wake dies with "fatal
// error: bad g->status in ready"; without the teardown discard, the queued
// bubble-channel finalizer leaks to fing after deactivation and fatals with
// "send on synctest channel from outside bubble".
func TestDSTFinalizerGoexitDrain(t *testing.T) {
	out := runTestProgDST(t, "DSTFinGoexitDrain", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("Goexit drain death failed (got %q, want \"done\")", out)
	}
}

// TestDSTFinalizerGoexitLedger verifies the finalizer queue ledger stays exact
// when the drain dies mid-block: already-run entries are accounted per-entry in
// runFinqBlocks, while the unrun remainder closes the internal discard ledger
// without inflating the public executed metric.
//
// Teeth: with per-entry accounting reverted to the block-end add (which a
// mid-block death skips), the already-run entries are never counted —
// finPending() never clears, the Run-end fixpoint cannot terminate, and the
// in-run ledger delta check reports a mismatch.
func TestDSTFinalizerGoexitLedger(t *testing.T) {
	out := runTestProgDST(t, "DSTFinGoexitLedger", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("mid-block drain-death ledger failed (got %q, want \"done\")", out)
	}
}

func TestDSTCleanupGoexitLedger(t *testing.T) {
	out := runTestProgDST(t, "DSTCleanupGoexitLedger", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("cleanup drain-death ledger failed (got %q, want \"done\")", out)
	}
}

// TestDSTFinalizerStuckDrainRunEnd verifies that a drain still blocked inside a
// finalizer at Run end is reported as the deterministic synctest deadlock — the
// driver must not goready it out of the finalizer's channel wait.
//
// Teeth: with the stop-site dstDrainParked guard removed, the exit wake
// corrupts the channel wait queue and the output is the "sudog with non-nil
// elem" fatal instead of the deadlock panic.
func TestDSTFinalizerStuckDrainRunEnd(t *testing.T) {
	out := runTestProgDST(t, "DSTFinStuckDrainRunEnd", "DSTSEED=12345")
	if !strings.Contains(out, "deadlock: main bubble goroutine has exited but blocked goroutines remain") {
		t.Fatalf("stuck drain at Run end not reported as deadlock:\n%s", out)
	}
	if strings.Contains(out, "sudog") || strings.Contains(out, "unreachable") {
		t.Fatalf("stuck drain at Run end corrupted state or returned:\n%s", out)
	}
}

// TestDSTFinalizerAbandonedChainReuse verifies that a chain abandoned by a
// drain that never died (run 1 ends in a recorded/recovered deadlock with the
// drain parked inside a finalizer forever) is freed at the next activation and
// never spliced into a later run's discard ledger.
//
// Teeth: without dstDiscardAbandonedDrainChains at activation, run 2's
// drain-death discard splices run 1's stale chain into run 2's run-local
// executed counter, finPending() never clears, and run 2's end-of-run fixpoint
// hangs - the test times out instead of printing "done".
func TestDSTFinalizerAbandonedChainReuse(t *testing.T) {
	out := runTestProgDST(t, "DSTFinAbandonedChainReuse", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("abandoned drain chain reuse failed (got %q, want \"done\")", out)
	}
}

// TestDSTFinalizerStuckDrainResidue verifies that when the drain is stuck
// forever inside a finalizer at Run end, callbacks the run queued but the
// drain never reached are discarded before deactivation - not leaked to the
// bubble-less async workers.
//
// Teeth: without the discard at the stuck-drain Run-end branch, fing runs the
// leaked bubble-channel finalizer after the Run and the testprog fatals with
// "send on synctest channel from outside bubble" instead of printing "done".
func TestDSTFinalizerStuckDrainResidue(t *testing.T) {
	out := runTestProgDST(t, "DSTFinStuckDrainResidue", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("stuck-drain residue discard failed (got %q, want \"done\")", out)
	}
}

// TestDSTBubbleStreamIsolation verifies the salted per-bubble re-root: the
// SUT's second spawned goroutine must not share a per-g RNG stream with the
// finalizer drain.
//
// Teeth: with the bubble re-root unsalted (dstBubbleRoot instead of
// dstBubbleMainRoot), bubble.main replays the run caller's draws and the
// second child's first 16 crypto/rand bytes equal the finalizer's - the
// testprog prints "collision".
func TestDSTBubbleStreamIsolation(t *testing.T) {
	out := runTestProgDST(t, "DSTBubbleStreamIsolation", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("bubble stream isolation failed (got %q, want \"done\")", out)
	}
}

// TestDSTForeignBubbleIsolation verifies that plain synctest bubbles running
// concurrently with a simulation - including ones created mid-run - do not
// perturb the simulation's schedule.
//
// Teeth: with foreign-bubble goroutines classified as simulation-owned in
// firstSystemG (or with the simulation-bubble claim relaxed from
// activating-goroutine identity to first-bubble-wins in synctestRun), the
// foreign bubbles consume seed draws / clobber the scheduling RNG and the
// fingerprints diverge.
func TestDSTForeignBubbleIsolation(t *testing.T) {
	out := runTestProgDST(t, "DSTForeignBubbleIsolation", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("foreign bubble isolation failed (got %q, want \"done\")", out)
	}
}

// TestDSTProcessFencePidfd verifies the interception-boundary fence does not
// poison process-global host state via os's pidfd probe (checkPidfdOnce, a
// sync.OnceValue). A bubble goroutine touching an os process op must not run
// the probe — whose raw syscalls the fence would panic on, caching the panic in
// the Once and re-panicking forever on the host after the run. Run in a fresh
// process so the bubble op is the first-ever pidfd probe (the only ordering
// under which poisoning shows).
//
// Teeth: without the pidfdWorks() bubble short-circuit (os/pidfd_linux.go), the
// bubble's os.FindProcess runs checkPidfd -> pidfd_open -> fenced panic, so
// bubblePanicked flips true AND the host op re-panics (hostOK false).
func TestDSTProcessFencePidfd(t *testing.T) {
	// The testprog hardcodes Run(1, …); no DSTSEED is read.
	out := runTestProgDST(t, "DSTProcessFencePidfd")
	if strings.TrimSpace(out) != "bubblePanicked=false hostOK=true" {
		t.Fatalf("pidfd probe poisoned by bubble fence (got %q, want \"bubblePanicked=false hostOK=true\")", out)
	}
}

// TestDSTZeroCopyFence verifies the interception fence does not poison the
// process-global copy_file_range support probe (internal/poll.supportCopyFileRange,
// a sync.OnceValue whose body reads the kernel version via a fenced uname). A
// bubble goroutine's io.Copy between two real host files must route to the
// generic read/write loop, not the zero-copy path — so it neither panics nor
// runs (and poisons) that Once. Run in a fresh process so the bubble copy is the
// first-ever copy_file_range probe.
//
// Teeth: without the bubble arm of the zero_copy_linux.go gate, the bubble's
// io.Copy hits copy_file_range -> uname -> fenced panic (bubblePanicked=true,
// copyOK=false) AND poisons the Once so the post-run host copy re-panics
// (hostOK=false).
func TestDSTZeroCopyFence(t *testing.T) {
	out := runTestProgDST(t, "DSTZeroCopyFence")
	if strings.TrimSpace(out) != "bubblePanicked=false copyOK=true hostOK=true" {
		t.Fatalf("zero-copy fence poisoned or mis-fenced (got %q, want \"bubblePanicked=false copyOK=true hostOK=true\")", out)
	}
}

// TestDSTCgoFence verifies the interception boundary fences cgo at cgocall: a
// bubble goroutine's C call panics with the unsupported shape, while a non-bubble
// C call in the same process is unaffected. Uses testprogcgo built with -tags=dst
// and cgo.
//
// Teeth: without the cgocall fence, the bubble's C.dstCgoNoop() runs normally
// (no panic) → bubblePanicked=false.
func TestDSTCgoFence(t *testing.T) {
	testenv.MustHaveCGO(t)
	exe, err := buildTestProg(t, "testprogcgo", "-tags=dst")
	if err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSpace(runBuiltTestProg(t, exe, "DSTCgoFence"))
	if out != "bubblePanicked=true hostOK=true" {
		t.Fatalf("cgo fence mis-fenced (got %q, want \"bubblePanicked=true hostOK=true\")", out)
	}
}

// TestDSTNonBubbleAllocTrigger verifies that non-bubble allocations do not
// advance the deterministic GC trigger: NumGC deltas are identical with and
// without an outside allocator churning.
//
// Teeth: with the simulation-bubble gate removed from the dstHeapAlloc
// accounting in mallocgc, the outside goroutine's megabyte allocations move
// the cycle boundaries and the deltas diverge.
func TestDSTNonBubbleAllocTrigger(t *testing.T) {
	out := runTestProgDST(t, "DSTNonBubbleAllocTrigger", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("non-bubble alloc trigger isolation failed (got %q, want \"done\")", out)
	}
}

// TestDSTGCSysstackAlloc: cold-process and warm-process runs at one seed
// agree on the full per-cycle discovery sequence INCLUDING the run-end tail.
// Two exclusions make that hold: allocations the runtime performs on
// systemstack on a bubble goroutine's behalf (allgs growth) never reach the
// trigger counter — their size and timing are process history, not SUT heap
// growth (mutation: dropping the getg() == cur leg of the dispatcher gate
// counts run 1's growth arrays and shifts its crossings) — and the pooled
// structs run 1 allocates fresh (g/sudog) are subtracted back out of the
// GOGC-scaled target (dstPooledMarked; mutation: dropping the subtraction
// re-inflates the cold run's late target and splits the totals — 52104 vs
// 48626 at this shape and seed on the fixed tree).
func TestDSTGCSysstackAlloc(t *testing.T) {
	out := runTestProgDST(t, "DSTGCSysstackAlloc", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("systemstack bookkeeping moved the DST trigger (got %q, want \"done\")", out)
	}
}

// TestDSTPooledGBytesExact: the pooled-g cancellation constant equals what a
// fresh g's allocation actually charges heapMarked (size-class elemsize,
// malloc header included past MinSizeForMallocHeader) — the exactness the
// warm/cold cancellation contract requires, pinned arithmetically because
// the totals-equality tests move only when a GC crossing happens to land
// inside the 8-byte-per-g band.
func TestDSTPooledGBytesExact(t *testing.T) {
	if got, want := runtime.DstFreshGHeapBytes(), runtime.DstFreshGHeapBytesWant(); got != want {
		t.Fatalf("dstPooledGBytes arithmetic = %d, want %d (a fresh g charges heapMarked its full elemsize)", got, want)
	}
}

// TestDSTMemfdFDIsolation: the harness's page-cache memfds are invisible in
// the simulated fd namespace — a bubble goroutine gets exactly EBADF for
// them on every fenced surface (named wrappers, Pread via Syscall6, and
// RawSyscall — all through the trampolines), a daemonize-style close sweep
// is the harmless loop it is in
// production, and resizes and reads of the open simulated file keep working.
// Mutation: dropping the trampoline check fails at the white-box probe
// ("got nil, want EBADF" — the probe's own close then host-closes the
// memfd); dropping the registration prints "no page-cache fd found";
// answering success instead of EBADF fails the probes on the exact value.
func TestDSTMemfdFDIsolation(t *testing.T) {
	out := runTestProgDST(t, "DSTMemfdFDIsolation", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("page-cache fds reachable from the simulated process (got %q, want \"done\")", out)
	}
}

// TestDSTHostFDCloseRefused: a bubble goroutine's close of a real
// (non-virtual) fd number never reaches the kernel — answered EBADF at the
// trampolines on both the named and raw surfaces. This is what makes host-fd
// creation (page-cache memfds, the runtime's lazily-created netpoll epoll fd)
// atomic with respect to in-flight bubble dispatch: a close that is never
// dispatched cannot straddle the harness assigning that number to a newborn
// fd. The prog proves the kernel never saw the close by pushing a byte
// through a harness pipe whose read end the bubble "closed". Mutation:
// letting SYS_CLOSE dispatch for non-virtual numbers really closes the pipe
// and the post-run write/read fails; answering success instead of EBADF
// fails the probes on the exact value.
func TestDSTHostFDCloseRefused(t *testing.T) {
	out := runTestProgDST(t, "DSTHostFDCloseRefused", "DSTSEED=12345")
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("bubble close of a host fd reached the kernel (got %q, want \"ok\")", out)
	}
}

// TestDSTGCForeignStart: a DST-armed cycle is STARTED only inside the
// bubble-allocation gate. The prog holds the bubble's live set above
// Options.MemoryLimit (trigger condition persistently true) and churns a
// foreign allocator; NumGC deltas with and without the churn must be equal.
// Mutation: re-enabling the span-grab trigger sites under dstActive lets
// every foreign span grab start an extra cycle at a wall-clock-dependent
// point, diverging the deltas.
func TestDSTGCForeignStart(t *testing.T) {
	out := runTestProgDST(t, "DSTGCForeignStart", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("foreign allocation started DST-armed GC cycles (got %q, want \"done\")", out)
	}
}

// TestDSTGOMAXPROCSAutoModeRestored verifies that in an auto-GOMAXPROCS
// process the pin sets the custom flag for the run (blocking the sysmon
// auto-updater) and restores auto mode afterward.
//
// Teeth: with the restore reverted to a plain GOMAXPROCS(oldProcs), the
// custom flag stays set after the Run and "after=" reports false.
func TestDSTGOMAXPROCSAutoModeRestored(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTGOMAXPROCSAutoRestore", "DSTSEED=12345"))
	if out == "custom" {
		t.Skip("process started in custom GOMAXPROCS mode")
	}
	if out != "inrun=false after=true" {
		t.Fatalf("auto GOMAXPROCS pin/restore failed (got %q, want \"inrun=false after=true\")", out)
	}
}

// TestDSTRunRequiresBuildTag verifies the documented build-constraint panic:
// a binary built WITHOUT -tags dst refuses simulation.Run with the
// reproducible-map-hash-key message (the one documented panic that can only
// be tested from an untagged build).
func TestDSTRunRequiresBuildTag(t *testing.T) {
	exe, err := buildTestProg(t, "testprog") // no -tags dst
	if err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSpace(runBuiltTestProg(t, exe, "DSTRunNoTag"))
	if !strings.Contains(out, "requires building with -tags dst") {
		t.Fatalf("untagged simulation.Run = %q, want the -tags dst build panic", out)
	}
}

// TestDSTFaultAPILinksUntagged pins the tag boundary of the fault API: the
// simulated filesystem lives behind -tags dst, so an untagged binary that calls
// CrashHost (or any fault whose implementation reaches os) must still LINK and
// no-op outside a run. A direct linkname to a dst-only os symbol from this
// package's untagged files breaks the build of any program that calls the API,
// with a relocation error naming an internal symbol.
func TestDSTFaultAPILinksUntagged(t *testing.T) {
	exe, err := buildTestProg(t, "testprog") // no -tags dst
	if err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSpace(runBuiltTestProg(t, exe, "DSTFaultAPINoTag"))
	if out != "fault api no-op" {
		t.Fatalf("untagged fault API = %q, want a clean no-op", out)
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

// TestDSTFinalizerProfileNoOvercount verifies that a finalizer running on the DST
// drain is counted exactly once by runtime.GoroutineProfile. The drain is a user
// goroutine, so fingRunningFinalizer must not add a synthetic extra record for it.
func TestDSTFinalizerProfileNoOvercount(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	out := runTestProgDST(t, "DSTFinProfile", env...)
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("goroutine profile overcounted DST drain finalizer (got %q, want \"ok\")", out)
	}
}

// TestDSTFinalizerPreBubbleDeferred verifies that finalizers queued before a run
// stay out of that run's bubble drain. The testprog builds a pre-bubble chain;
// the tail may resolve before the run or be deferred until after it, but its
// state must not change during an in-run quiescence and it must never observe
// dstActive.
func TestDSTFinalizerPreBubbleDeferred(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=off"}
	out := runTestProgDST(t, "DSTFinPreBubble", env...)
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 6 || f[0] != f[2] || f[1] != f[3] || f[4] != "false" || f[5] != "false" {
		t.Fatalf("pre-bubble finalizer entered run or observed dstActive (got %q): want head/tail start==after and active=false", out)
	}
}

// TestDSTFinalizerPreBubbleReleased verifies that pre-bubble finalizers detached
// from the run are released back to normal finalizer processing after dstDeactivate.
func TestDSTFinalizerPreBubbleReleased(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=off"}
	out := runTestProgDST(t, "DSTFinPreBubbleRelease", env...)
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("pre-bubble finalizers were not released after dstDeactivate (got %q, want \"ok\")", out)
	}
}

// TestDSTFinalizerPreBubbleInFlightIgnored verifies that a finalizer already
// running on fing before Run does not poison the run's pending counts.
func TestDSTFinalizerPreBubbleInFlightIgnored(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=off"}
	out := runTestProgDST(t, "DSTFinPreBubbleInFlight", env...)
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("pre-bubble in-flight finalizer blocked run-end drain (got %q, want \"ok\")", out)
	}
}

// TestDSTFinalizerInFlightWorkerDoesNotStealRunQueue verifies that a pre-run
// fing callback released during Run parks before taking in-run work. The in-run
// finalizer sends on a bubble channel, so async execution would fatal.
func TestDSTFinalizerInFlightWorkerDoesNotStealRunQueue(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=off"}
	out := runTestProgDST(t, "DSTFinInFlightReleaseDuringRun", env...)
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("in-flight finalizer worker stole in-run work or missed drain (got %q, want \"ok\")", out)
	}
}

// TestDSTFinalizerLongRunEndChain verifies that the Run-end fixpoint has no
// finite round cap: a chain longer than the old cap still resolves in-bubble
// before teardown, so the tail observes dstActive before Run returns.
func TestDSTFinalizerLongRunEndChain(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	out := runTestProgDST(t, "DSTFinLongChain", env...)
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("long finalizer chain escaped run-end drain (got %q, want \"ok\")", out)
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

// TestDSTCleanupPreBubbleDeferred is the cleanup analogue of
// TestDSTFinalizerPreBubbleDeferred: queued pre-bubble cleanups must not
// execute in the run or observe dstActive.
func TestDSTCleanupPreBubbleDeferred(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=off"}
	out := runTestProgDST(t, "DSTCleanupPreBubble", env...)
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 6 || f[0] != f[2] || f[1] != f[3] || f[4] != "false" || f[5] != "false" {
		t.Fatalf("pre-bubble cleanup entered run or observed dstActive (got %q): want head/tail start==after and active=false", out)
	}
}

// TestDSTCleanupPreBubbleReleased is the cleanup analogue of
// TestDSTFinalizerPreBubbleReleased.
func TestDSTCleanupPreBubbleReleased(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=off"}
	out := runTestProgDST(t, "DSTCleanupPreBubbleRelease", env...)
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("pre-bubble cleanups were not released after dstDeactivate (got %q, want \"ok\")", out)
	}
}

// TestDSTCleanupPreBubbleInFlightIgnored is the cleanup analogue of
// TestDSTFinalizerPreBubbleInFlightIgnored.
func TestDSTCleanupPreBubbleInFlightIgnored(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=off"}
	out := runTestProgDST(t, "DSTCleanupPreBubbleInFlight", env...)
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("pre-bubble in-flight cleanup blocked run-end drain (got %q, want \"ok\")", out)
	}
}

// TestDSTCleanupInFlightWorkerDoesNotStealRunQueue is the cleanup analogue of
// TestDSTFinalizerInFlightWorkerDoesNotStealRunQueue.
func TestDSTCleanupInFlightWorkerDoesNotStealRunQueue(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=off"}
	out := runTestProgDST(t, "DSTCleanupInFlightReleaseDuringRun", env...)
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("in-flight cleanup worker stole in-run work or missed drain (got %q, want \"ok\")", out)
	}
}

// TestDSTCleanupLongRunEndChain is the cleanup analogue of
// TestDSTFinalizerLongRunEndChain.
func TestDSTCleanupLongRunEndChain(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	out := runTestProgDST(t, "DSTCleanupLongChain", env...)
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("long cleanup chain escaped run-end drain (got %q, want \"ok\")", out)
	}
}

// TestDSTCleanupProfileNoOvercount is the cleanup analogue of
// TestDSTFinalizerProfileNoOvercount.
func TestDSTCleanupProfileNoOvercount(t *testing.T) {
	env := []string{"DSTSEED=12345", "GOGC=100"}
	out := runTestProgDST(t, "DSTCleanupProfile", env...)
	if strings.TrimSpace(out) != "ok" {
		t.Fatalf("goroutine profile overcounted DST drain cleanup (got %q, want \"ok\")", out)
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
// clearing happens during the STW sweep (gc.md D4 dimension 7), so the cleared
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

func TestDSTRunRejectsNestedWithoutClearingOuterState(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTRunNestedGuard", "GOGC=off"))
	const want = "nested=true active=true pid=1"
	if out != want {
		t.Fatalf("nested Run guard failed:\n got=%q\nwant=%q", out, want)
	}
}

func TestDSTRunRejectsOverlappingTopLevelRuns(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTRunOverlapGuard", "GOGC=off"))
	const want = "overlap=true active=true"
	if out != want {
		t.Fatalf("overlapping Run guard failed:\n got=%q\nwant=%q", out, want)
	}
}

func TestDSTRunKeepsGOMAXPROCSPinned(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTRunGOMAXPROCSPinned", "GOGC=off", "GOMAXPROCS=4"))
	const want = "before=4 old=1 afterSet=1 afterDefault=1 auto=false restored=4"
	if out != want {
		t.Fatalf("GOMAXPROCS escaped DST Run:\n got=%q\nwant=%q", out, want)
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

func TestDSTSchedulerCanonicalCandidateIdentity(t *testing.T) {
	physical := []uint64{30, 10, 20}
	for ordinal, want := range []uint64{10, 20, 30} {
		if got := runtime.DSTTestStableCandidateSelection(physical, uint32(ordinal)); got != want {
			t.Fatalf("ordinal %d selected creation index %d, want %d", ordinal, got, want)
		}
	}
	if got := runtime.DSTTestPCTTieSelection([]uint64{20, 10}, []uint64{1, 2}); got != 10 {
		t.Fatalf("PCT tie selected creation index %d, want 10", got)
	}
	for _, strategy := range []int{0, 1} {
		if got := runtime.DSTTestHarnessTransparency(strategy); got != [3]bool{true, true, true} {
			t.Fatalf("strategy %d harness transparency = %v, want all true", strategy, got)
		}
	}
}

// TestDSTScheduleDiversity verifies seed-varied scheduling, for every strategy:
// the interleaving is seed-*varied*, so different seeds explore different sound
// interleavings (the completeness gain — before the seeded scheduling draw,
// every seed produced the identical schedule). Freely-concurrent scenarios must produce many distinct
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

func TestDSTSchedulePCTMaximumDepth(t *testing.T) {
	if got := runtime.DSTTestPCTChangeCount(16); got != 15 {
		t.Fatalf("depth 16 change points = %d, want 15", got)
	}
}

// TestDSTProcessIdentity verifies the simulation fixes os.Getpid/os.Hostname to a
// deterministic identity inside Run (a default, or the Options value), and
// restores the real machine's identity afterward. Closes the determinism hole a
// SUT reading pid/hostname would otherwise have.
func TestDSTProcessIdentity(t *testing.T) {
	out := strings.TrimSpace(runTestProgNetDST(t, "DSTProcessIdentity"))
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
	out := strings.TrimSpace(runTestProgNetDST(t, "DSTIdentityExtra"))
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
	if !strings.Contains(out1, " eq=true seedvaries=true realdiffers=true active=false") {
		t.Fatalf("crypto/rand not deterministic/seed-varying/real-and-inactive-outside under DST: %q", out1)
	}
	const vectors = " vectors=1:2b,7:2b298af2e35a36,8:2b298af2e35a36e0,9:2b298af2e35a36e08e,15:2b298af2e35a36e08ea48997c92d76 emptyneutral=true"
	if !strings.Contains(out1, vectors) {
		t.Fatalf("crypto/rand seeded byte encoding or empty-read draw count changed: %q", out1)
	}
	// The deterministic in-run stream (h=...) replays across processes; the
	// outside-run bytes (out=...) are real entropy and must NOT. Comparing the
	// split fields catches a mutation that leaves the deterministic source
	// active-but-advancing outside the run, which the in-process
	// realdiffers check alone cannot.
	det1, outside1, ok1 := strings.Cut(out1, " out=")
	det2, outside2, ok2 := strings.Cut(out2, " out=")
	if !ok1 || !ok2 {
		t.Fatalf("missing out= field:\nrun1=%q\nrun2=%q", out1, out2)
	}
	if det1 != det2 {
		t.Fatalf("crypto/rand not reproducible across processes for the same seed:\nrun1=%q\nrun2=%q", out1, out2)
	}
	if outside1 == outside2 {
		t.Fatalf("outside-run crypto/rand identical across processes (still deterministic?):\nrun1=%q\nrun2=%q", out1, out2)
	}
}

// TestDSTCryptoSeededChildAfterDeactivate pins the active half of the entropy
// gate independently of the seeded sentinel: a white-box child keeps its
// nonzero stream root after deactivation but must read OS entropy. Across two
// processes the bytes therefore differ.
func TestDSTCryptoSeededChildAfterDeactivate(t *testing.T) {
	a := strings.TrimSpace(runTestProgDST(t, "DSTCryptoSeededChildAfterDeactivate"))
	b := strings.TrimSpace(runTestProgDST(t, "DSTCryptoSeededChildAfterDeactivate"))
	if a == "" || a == b {
		t.Fatalf("a seeded child reads deterministic crypto/rand after deactivation:\na=%q\nb=%q", a, b)
	}
}

// TestDSTCryptoUnseededGoroutine is the INV-CRYPTO unseeded-leg regression: a
// goroutine started before the run (dstrand==0) that reads crypto/rand DURING the run
// must get real OS entropy, not the fixed zero-rooted stream — so its bytes differ
// across processes. The bug fills from the zero-rooted (seed-independent, deterministic)
// stream, making them identical. Mutation: removing the gp.dstrand==0 gate in
// dstReadRandom makes the two runs' bytes identical.
func TestDSTCryptoUnseededGoroutine(t *testing.T) {
	a := strings.TrimSpace(runTestProgDST(t, "DSTCryptoUnseededGoroutine", "DSTSEED=1"))
	b := strings.TrimSpace(runTestProgDST(t, "DSTCryptoUnseededGoroutine", "DSTSEED=1"))
	if a == "" || a == b {
		t.Fatalf("a pre-run (unseeded) goroutine's in-run crypto/rand is deterministic across processes (predictable entropy — the zero-rooted stream leak):\na=%q\nb=%q", a, b)
	}
}

// TestDSTCryptoUnseededVectors is the INV-CRYPTO stable-sentinel regression: a
// pre-run (unseeded) goroutine that spawns a child, draws math/rand, runs a
// select, or adds a fake timer (in a foreign synctest bubble) DURING the run —
// and, for the spawn, the child itself — must still read real OS entropy from
// crypto/rand: none of those operations may advance the zero-rooted stream and
// flip the goroutine into the run-seeded tree. Each labeled line must differ
// across two processes; under the bug the corresponding vector's bytes are
// seed-independent and identical. Mutation: dropping the dstrand != 0 guard at
// runtime.rand breaks mathrand (and spawnparent, via the tainted parent); at
// selectgo's pollorder draw, select; at the fake-timer tie-break, timer; at
// newproc1's child seeding, spawnchild and spawnparent.
func TestDSTCryptoUnseededVectors(t *testing.T) {
	parse := func(out string) map[string]string {
		if strings.Contains(out, "incomplete") {
			t.Fatalf("unseeded readers did not run during the active window:\n%s", out)
		}
		m := make(map[string]string)
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			label, hex, ok := strings.Cut(line, "=")
			if !ok || hex == "" {
				t.Fatalf("malformed vector line %q in:\n%s", line, out)
			}
			m[label] = hex
		}
		return m
	}
	a := parse(runTestProgDST(t, "DSTCryptoUnseededVectors", "DSTSEED=1"))
	b := parse(runTestProgDST(t, "DSTCryptoUnseededVectors", "DSTSEED=1"))
	for _, label := range []string{"mathrand", "select", "spawnchild", "spawnparent", "timer"} {
		if a[label] == "" || b[label] == "" {
			t.Fatalf("vector %q missing:\na=%v\nb=%v", label, a, b)
		}
		if a[label] == b[label] {
			t.Errorf("vector %q: identical crypto/rand bytes across processes (seed-independent deterministic stream — the %s operation seeded an unseeded goroutine): %s", label, label, a[label])
		}
	}
}

// TestDSTSchedForeignSpinner: bounded infrastructure-first scheduling — a
// pre-run foreign goroutine that never blocks cannot starve the simulation
// (the runs complete), and the fairness hand-off leaves the seeded
// interleaving untouched (fingerprints with the spinner churning equal the
// spinner-free fingerprints, random and PCT). Mutation: an unconditional
// system-first pick livelocks the run and the prog's foreign watchdog prints
// "starved"; selecting over the full mixed set instead of the sim-only subset
// diverges the fingerprints.
func TestDSTSchedForeignSpinner(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTSchedForeignSpinner", "DSTSEED=12345"))
	if out != "done" {
		t.Fatalf("simulation under a persistently-runnable foreign goroutine: %q", out)
	}
}

// TestDSTWedgeSpinCallFree: a bubble goroutine in a CALL-FREE spin loop (no
// preemption point) wedges the whole process — no scheduler decision can ever
// happen again, so only sysmon's wall arm can see it. The run must die with
// the loud DST-WEDGE diagnostic naming the spinning goroutine, not hang until
// the test timeout. Mutation: neutering dstWedgeSysmonCheck (or its sysmon
// call) leaves the subprocess wedged until testenv's deadline kills it, and
// the diagnostic never appears.
func TestDSTWedgeSpinCallFree(t *testing.T) {
	out := runTestProgDST(t, "DSTWedgeSpinCallFree")
	if !strings.Contains(out, "DST-WEDGE") ||
		!strings.Contains(out, "call-free spin") ||
		!strings.Contains(out, "without a single scheduler decision") {
		t.Fatalf("call-free spinner did not get the DST-WEDGE diagnostic:\n%s", out)
	}
	if !strings.Contains(out, "Options.WedgeWallLimit") {
		t.Fatalf("diagnostic does not name the knob:\n%s", out)
	}
	if !strings.Contains(out, "goroutine ") {
		t.Fatalf("diagnostic does not name the spinning goroutine:\n%s", out)
	}
	if strings.Contains(out, "unreachable") {
		t.Fatalf("wedged run returned instead of failing loud:\n%s", out)
	}
}

// TestDSTWedgeParkLoop: a bubble that keeps scheduling but never reaches
// durable quiescence (mutex ping-pong with Gosched yields — a non-durable
// park loop) is permanently stuck in zero virtual time, invisible to the
// durably-blocked deadlock detector. The quiescence arm must fail the run
// loudly at its decision bound, naming the runnable goroutines. Mutation:
// dropping the dstSchedSinceQuiesce bound check in dstFindRunnable leaves the
// subprocess spinning until testenv's deadline kills it.
func TestDSTWedgeParkLoop(t *testing.T) {
	out := runTestProgDST(t, "DSTWedgeParkLoop")
	if !strings.Contains(out, "DST-WEDGE") ||
		!strings.Contains(out, "durable quiescence") {
		t.Fatalf("non-durable park loop did not get the DST-WEDGE diagnostic:\n%s", out)
	}
	if !strings.Contains(out, "Options.WedgeDecisionLimit") {
		t.Fatalf("diagnostic does not name the knob:\n%s", out)
	}
	if !strings.Contains(out, "main.DSTWedgeParkLoop") {
		t.Fatalf("diagnostic does not carry the spinning goroutines' stacks:\n%s", out)
	}
}

// TestDSTCryptoPriorRunCaller is the INV-CRYPTO cross-run leg: the goroutine
// that called a completed run survives with a seeded per-g root; deactivation
// must clear it, so that during a LATER run (started by another goroutine) it
// reads real OS entropy like any outsider — not bytes derived from the
// previous run's seed. The prog's output must differ across two processes.
// Mutation: removing the dstrand clear in dstDeactivate makes the caller's
// in-second-run bytes a pure function of the first run's seed, identical
// across processes.
func TestDSTCryptoPriorRunCaller(t *testing.T) {
	a := strings.TrimSpace(runTestProgDST(t, "DSTCryptoPriorRunCaller", "DSTSEED=3"))
	b := strings.TrimSpace(runTestProgDST(t, "DSTCryptoPriorRunCaller", "DSTSEED=3"))
	if strings.Contains(a, "incomplete") || strings.Contains(b, "incomplete") {
		t.Fatalf("prior run's caller did not read during the second run's active window:\na=%q\nb=%q", a, b)
	}
	if a == "" || a == b {
		t.Fatalf("a prior run's caller reads deterministic crypto/rand during a later run (stale seeded root survived deactivation):\na=%q\nb=%q", a, b)
	}
}

// TestDSTIdentityGroups verifies the simulated group list and the minimal
// simulated user/group database (the simulated user and its group resolve by
// name and id; everything else is deterministically unknown; host values
// return after the run).
func TestDSTIdentityGroups(t *testing.T) {
	out := strings.TrimSpace(runTestProgNetDST(t, "DSTIdentityGroups", "DSTSEED=12345"))
	if out != "done" {
		t.Fatalf("simulated identity groups/database failed: %q", out)
	}
}

// TestDSTDiskReplay: a concurrent file workload under the in-memory DST
// filesystem replays byte-identically across processes — the cross-process
// form of the filesystem determinism invariant (in-process form and the rest
// of the chunk surface are covered in os's dst tests).
func TestDSTDiskReplay(t *testing.T) {
	out1 := runTestProgDST(t, "DSTDiskReplay", "DSTSEED=42")
	if !strings.Contains(out1, "content=[g") || !strings.Contains(out1, "sizes=[") {
		t.Fatalf("malformed disk replay transcript:\n%s", out1)
	}
	out2 := runTestProgDST(t, "DSTDiskReplay", "DSTSEED=42")
	if out1 != out2 {
		t.Fatalf("in-memory filesystem not reproducible across processes:\nrun1=%q\nrun2=%q", out1, out2)
	}
}

// TestDSTPipeReplay: a concurrent pipe workload under the in-memory DST pipe
// (the third I/O feature) replays byte-identically across processes — the
// schedule-sensitivity rides the frame order in the content line, and a
// fake-clock deadline event pins virtual-time exactness (in-process coverage
// lives in os's dst tests). The end/total guard is the completeness pin: the
// drain must terminate in EOF with all 477 record bytes (schedule-invariant)
// — a transcript that merely STARTED, then died mid-drain, fails here rather
// than slipping through on byte-identical-because-deterministic failure.
func TestDSTPipeReplay(t *testing.T) {
	out1 := runTestProgDST(t, "DSTPipeReplay", "DSTSEED=42")
	if !strings.Contains(out1, "content=[g") || !strings.Contains(out1, "sips=[") ||
		!strings.Contains(out1, "end=EOF total=477") ||
		!strings.Contains(out1, "stat=prw-------") ||
		!strings.Contains(out1, "deadline: +3s err=read |0: i/o timeout") {
		t.Fatalf("malformed pipe replay transcript:\n%s", out1)
	}
	out2 := runTestProgDST(t, "DSTPipeReplay", "DSTSEED=42")
	if out1 != out2 {
		t.Fatalf("in-memory pipe not reproducible across processes:\nrun1=%q\nrun2=%q", out1, out2)
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
	const want = "resp=echo:ping local=127.0.0.1:40000 remote=127.0.0.1:9000 | server saw ping from 127.0.0.1:40000"
	out1 := strings.TrimSpace(runTestProgNetDST(t, "DSTNet", "DSTSEED=42"))
	if out1 != want {
		t.Fatalf("in-memory net exchange wrong:\n got=%q\nwant=%q", out1, want)
	}
	out2 := strings.TrimSpace(runTestProgNetDST(t, "DSTNet", "DSTSEED=42"))
	if out2 != out1 {
		t.Fatalf("in-memory net not reproducible across processes:\nrun1=%q\nrun2=%q", out1, out2)
	}
}

// TestDSTNetSemantics verifies the DST network shim preserves public net semantics
// at the TCP surface it intercepts and rejects unsupported network kinds rather
// than modeling them as impossible TCP-like streams.
func TestDSTNetSemantics(t *testing.T) {
	out := strings.TrimSpace(runTestProgNetDST(t, "DSTNetSemantics", "DSTSEED=42"))
	const want = "canceled=true deadline=true nilpanic=true udpreject=true udpdialreject=true zeroports=true invalidport=true localhost=true dnsreject=true servicereject=true familyreject=true wildcardfamilyreject=true tcp6wildcardreject=true tcp6local=true tcp4unspecified=true"
	if out != want {
		t.Fatalf("DST net semantics mismatch:\n got=%q\nwant=%q", out, want)
	}
}

// TestDSTSchedSystemIsolation verifies the non-simulation-goroutine isolation
// invariant that keeps the schedule deterministic regardless of
// timing-/composition-varying infrastructure scheduling: under DST the
// scheduling RNG advances exactly once per SIMULATION goroutine selection and
// never for an infrastructure one (simulation membership is the sticky per-g
// dstSimG property — the run's own goroutines stay simulation candidates even
// while the GC assist paths transiently nil their bubble field). The prog
// runs a foreign Gosched spinner across the run, so rngDraws ==
// decisions - sysScheds with sysScheds>0 (the spinner's alternation picks).
// Without isolation, infrastructure selections would draw from the bubble
// RNG, and how often they occur (timing/binary composition) would shift
// every subsequent selection — the nondeterminism a bare `import "net"`
// exposed (~1% of runs). Mutation check: making dstFindRunnable
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

// TestDSTPCTNonBubbleCreation is the M1 regression: a goroutine creation by a
// non-bubble goroutine must not consume a PCT priority draw (drawn from the scheduling
// RNG at creation), or it shifts the measured goroutines' priorities and interleaving.
// Mutation: dropping the callergp.bubble == dstSimBubble gate on dstPCTAssignPrio makes
// the testprog print "PCT schedule perturbed by non-bubble creation".
func TestDSTPCTNonBubbleCreation(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTPCTNonBubbleCreation", "DSTSEED=12345"))
	if out != "done" {
		t.Fatalf("non-bubble creation perturbed the PCT schedule (creation-side draw not isolated):\n%s", out)
	}
}

// TestDSTPCTMainDrawsPriority is the H1 regression: bubble.main is a simulation
// goroutine and MUST draw a PCT priority at creation, even though synctestRun creates
// it BEFORE claiming dstSimBubble (so a naive callergp.bubble == dstSimBubble gate
// misses it). Reads bubble.main's dstPrio inside a PCT run: it must be nonzero (drawn).
// Mutation: gating dstPCTAssignPrio on callergp.bubble == dstSimBubble alone leaves
// bubble.main at dstPrio 0 (always lowest in PCT selection).
func TestDSTPCTMainDrawsPriority(t *testing.T) {
	out := strings.TrimSpace(runTestProgDST(t, "DSTPCTMainDrawsPriority", "DSTSEED=12345"))
	if out != "nonzero" {
		t.Fatalf("bubble.main PCT priority = %q, want nonzero (bubble.main did not draw a priority — it sits at 0, always lowest)", out)
	}
}

// TestDSTFinalizerChainNoLeak verifies the Run-end fixpoint drain resolves a
// finalizer chain whose tail touches a bubble channel fully in-bubble, so it does
// not leak to post-teardown async processing and fatal (gc.md D4: Run-end fixpoint).
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
// bubble's heap growth (gc.md D6: Options.MemoryLimit): a tighter limit forces
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
// Deterministic across same-seed runs (DST-L2-2). See docs/dst/exploration.md "Level 2".
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
	// The SUT announces every lock acquisition through dstSyncAcquire,
	// so even in this norace build the exploration carries its own
	// ordering visibility and KEEPS its exhaustion claim — the
	// mutex-blind downgrade (TestDPORMutexBlindDowngrade in
	// testing/simulation) fires only for parks with no announces.
	if !strings.Contains(out1, "budgethit=false") || !strings.Contains(out1, "overflow=false") {
		t.Fatalf("yield-while-locked enumeration truncated: %q", out1)
	}
	if !strings.Contains(out1, "exhausted=true") || !strings.Contains(out1, "uninstrumented=false") {
		t.Fatalf("announced-sync exploration must keep its exhaustion claim: %q", out1)
	}
	if out2 := runTestProgDSTNoRace(t, "DSTYieldSound", "DSTSEED=1"); out1 != out2 {
		t.Fatalf("access-granularity yield not deterministic across same-seed runs:\n run1=%q\n run2=%q", out1, out2)
	}
}

// TestDSTExploreFindsAtomicityViolation verifies the systematic explorer finds, in
// a single Explore call, the atomicity violation that the coarse random/PCT
// strategies miss for every seed (0/200). It also confirms the explorer is sound
// (the mutex-protected counter SUT yields no failing interleaving) and
// deterministic. See docs/dst/exploration.md (Level 2, DST-L2-1/2).
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
// reachable states (bugs). See docs/dst/exploration.md (Level 2, DST-L2-3). The larger
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
// without it (21), so it has teeth. See docs/dst/exploration.md (Level 2, increment 2).
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

// TestDSTExploreTimerHB validates that fake-timer wake edges do not make DPOR prune
// a timer-gated race as happens-before-ordered. Both goroutines sleep until the same
// virtual time; after the root fires both timers, their read/write accesses are
// co-enabled and Exhaustive reaches both read outcomes. DPOR must match that outcome
// set while still reporting exhausted coverage.
func TestDSTExploreTimerHB(t *testing.T) {
	out := runTestProgDSTNoRace(t, "DSTExploreTimerHB", "DSTSEED=1")
	if exploreField(t, out, "complete") != "true" {
		t.Fatalf("DPOR dropped a timer-gated memory class (timer wake HB over-ordered a race):\n%s", out)
	}
	if n, err := strconv.Atoi(exploreField(t, out, "outcomes")); err != nil {
		t.Fatalf("bad outcomes field in %q: %v", out, err)
	} else if n != 2 {
		t.Fatalf("timer-gated SUT outcome set changed: outcomes=%d, want 2:\n%s", n, out)
	}
	if exploreField(t, out, "exhExhausted") != "true" || exploreField(t, out, "dporExhausted") != "true" || exploreField(t, out, "overflow") != "false" {
		t.Fatalf("timer-gated exploration did not finish cleanly:\n%s", out)
	}
}

// TestDSTExploreSweep is the DST-L2-3 completeness guard: a generated family of
// small concurrent programs (2-3 goroutines; reads/writes over 1-2 shared vars;
// with and without mutex synchronization) plus hand-written hard SUTs (channel
// rendezvous order and timer-gated memory conflicts) is explored under BOTH DPOR
// and brute-force Exhaustive,
// and DPOR must reach the IDENTICAL set of observable outcomes for every one. This
// is the real net that the micro-SUTs (TestDSTExploreComplete, a single no-mutex
// program) only weakly approximate.
//
// It specifically guards the synchronization-acquisition-order classes: WHICH
// goroutine acquires a mutex / rendezvous on a channel first is a real scheduling
// choice that changes the outcome, but it occurs at a transition recording no
// memory access, so DPOR drops one order unless each acquisition is recorded as a
// conflicting transition (runtime.dstSyncAcquire). With that hook neutered, 23 of
// the original 290-program family fail (411 of the full 802 with dstAtomicYield
// neutered); with the hooks live, 0 — so it has teeth. See docs/dst/exploration.md
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
	// The family SIZE is asserted, not just mismatches=0: DPOR==Exhaustive
	// over a silently-shrunk family is a hollow completeness proof (a
	// generator regression — a collapsed loop bound, a dropped sub-family —
	// keeps mismatches=0 green while the net thins). The spec's DST-L2-3
	// claim stands on 802 SUTs; an intentional family change updates this
	// constant together with exploration.md.
	if got := exploreField(t, out, "programs"); got != "802" {
		t.Fatalf("sweep family size = %s programs, want 802 (exploration.md DST-L2-3): "+
			"a shrunk family silently caps the completeness net:\n%s", got, out)
	}
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
// summary fields (not the address-bearing report). See docs/dst/exploration.md (Level 2, D5).
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
	out := runBuiltTestProg(t, exe, "DSTExploreRaceOracle", "DSTSEED=1", "DSTRACE=multi")
	if races, err := strconv.Atoi(exploreField(t, out, "races")); err != nil {
		t.Fatalf("bad races field in %q: %v", out, err)
	} else if races < 2 {
		t.Fatalf("multi-race oracle collapsed distinct TSan reports into %d race failure(s), want at least 2:\n%s", races, out)
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
	budget := runBuiltTestProg(t, exe, "DSTExploreBudgetPromotion", "DSTSEED=1")
	if exploreField(t, budget, "schedules") != "1" || exploreField(t, budget, "runs") != "1" ||
		exploreField(t, budget, "budget") != "true" || exploreField(t, budget, "exhausted") != "false" {
		t.Fatalf("ExploreWith MaxSchedules did not cap the whole promotion loop:\n%s", budget)
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
//   - rangeComplete/outcomes guard range/composite access filtering: a range write at
//     a struct base must conflict with a scalar read of a field inside the struct.
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
	if n, err := strconv.Atoi(exploreField(t, out, "unfOutcomes")); err != nil {
		t.Fatalf("bad unfOutcomes field in %q: %v", out, err)
	} else if n != 2 {
		t.Fatalf("UNFILTERED DPOR outcome set diverges from the filtered ground truth: "+
			"unfOutcomes=%d, want 2 (a filter defect cancels out of the filtered "+
			"DPOR==Exhaustive check; this leg bypasses the filter):\n%s", n, out)
	}
	if exploreField(t, out, "unfExhausted") != "true" {
		t.Fatalf("unfiltered cross-check leg did not complete exploration:\n%s", out)
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
	if exploreField(t, out, "rangeComplete") != "true" {
		t.Fatalf("source-DPOR dropped a range-vs-field class (range access identity bug):\n%s", out)
	}
	if n, err := strconv.Atoi(exploreField(t, out, "rangeOutcomes")); err != nil {
		t.Fatalf("bad rangeOutcomes field in %q: %v", out, err)
	} else if n != 2 {
		t.Fatalf("range-vs-field outcome set changed: outcomes=%d, want 2:\n%s", n, out)
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

// TestDSTSyncHBRaceIgnore verifies the HB shadow's raceignore mirror on the
// recorded sync-event stream directly: a plain Mutex pair records exactly its
// acquire+release, a runtime.RaceDisable-bracketed pair records nothing, and
// RWMutex ops record ONLY the public readerSem/writerSem events — the embedded
// writer mutex's HB inside Lock/Unlock is suppressed by upstream's race.Disable
// brackets via the raceignore check in the HB-record bridges. Outcome-based
// tests cannot enforce this (HB records only prune; RWMutex's embedded pairs
// are subsumed by public ones in every reachable shape), so the white-box event
// counts are the teeth: dropping the raceignore check inflates rwLock to 3
// events and ignoredPair to 2; deleting or mis-placing a public RWMutex hook
// (e.g. recording RUnlock's release after race.Disable) zeroes its field.
func TestDSTSyncHBRaceIgnore(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the dst-race build")
	}
	testenv.MustHaveGoBuild(t)
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skipf("race detector not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testenv.MustHaveCGO(t) // -race requires cgo
	exe := filepath.Join(t.TempDir(), "tp_race")
	buildTestProgExplicit(t, exe, "-tags=dst", "-race")
	out := runBuiltTestProg(t, exe, "DSTSyncHBSuppress", "DSTSEED=1")
	for _, field := range []string{"mutexPair", "ignoredPair", "rwLock", "rwUnlock", "rwRLock", "rwRUnlock", "tryLockPair", "chanPair", "ignoredChan", "ignoredAtomic", "contended", "exhausted"} {
		if got := exploreField(t, out, field); got != "true" {
			t.Fatalf("HB raceignore mirror violated: %s=%s (see DSTSyncHBSuppress fixture):\n%s",
				field, got, out)
		}
	}
}

// TestDSTForeignCallbackDeferred verifies the ownership boundary of the bubble
// drain: finalizers/cleanups registered MID-RUN by goroutines outside
// the simulation bubble are discovered by the simulation's GCs but deferred
// past the run (they execute on the ordinary async workers afterward), while
// the simulation's own registrations still run on the drain in-run.
//
// Teeth: with the epoch routing removed from queuefinalizer (or
// cleanupQueue.enqueue), the foreign callback runs on the drain mid-run and the
// program reports it; with the routing inverted, the simulation's own control
// callbacks never run in-run; with the release dropped, the deferred callbacks
// never run after the run.
func TestDSTForeignCallbackDeferred(t *testing.T) {
	out := runTestProgDST(t, "DSTForeignCallbackDeferred", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("foreign callback deferral failed (got %q, want \"done\")", out)
	}
}

// TestDSTRunqOverflowOrder verifies the order-preserving local-ring overflow:
// with more simultaneously-runnable goroutines than the ring holds,
// foreign goroutines churning through the same ring must not permute the
// simulation candidates' enumeration order — the schedule fingerprint with
// foreign churn equals the alone run's, and the overflow path demonstrably
// fired (a run that never overflows is reported as vacuous).
//
// Teeth: with the DST overflow branch in runqput removed (falling back to
// runqputslow's spill), foreign ring occupancy shifts the spill boundary and
// the rotated candidates diverge the fingerprint; with the overflow rerouted
// to the global-runq tail instead of the ring-extension queue, the overflowed
// goroutines land behind the Gosched'd ones and the fingerprint diverges too.
func TestDSTRunqOverflowOrder(t *testing.T) {
	out := runTestProgDST(t, "DSTRunqOverflowOrder", "DSTSEED=12345")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("runq overflow order isolation failed (got %q, want \"done\")", out)
	}
}

// TestDSTOvfFlushAtDeactivate verifies that goroutines still in the DST
// ring-overflow queue at deactivation are flushed to the global run queue: the
// normal scheduler never reads the overflow queue, so a missing flush strands
// them forever (the testprog hangs and times out).
func TestDSTOvfFlushAtDeactivate(t *testing.T) {
	out := runTestProgDST(t, "DSTOvfFlushAtDeactivate")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("overflow flush at deactivate failed (got %q, want \"done\")", out)
	}
}

// TestDSTWhiteBoxCleanupChurnP4 pins the white-box GOMAXPROCS>1 deferral
// contract: with DST active and no bubble, every mid-active cleanup defers
// through the finlock-serialized partial block and the exact count survives
// to the post-deactivation release — none run while active, none are lost.
// The churn straddles the activation boundary, where a background GC latched
// pre-activation sweeps lazily (concurrently) across dstSeed.Store.
func TestDSTWhiteBoxCleanupChurnP4(t *testing.T) {
	out := runTestProgDST(t, "DSTWhiteBoxCleanupChurnP4")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("white-box cleanup churn failed (got %q, want \"done\")", out)
	}
}

// TestDSTGOMAXPROCSEntryRace verifies the entry-race closure: a foreign GOMAXPROCS
// call racing run entry either loses (its update is dropped under the
// setter's STW once dstActive) or is caught loud by the post-activation pin
// verification — a run never proceeds with GOMAXPROCS != 1.
func TestDSTGOMAXPROCSEntryRace(t *testing.T) {
	out := runTestProgDST(t, "DSTGOMAXPROCSEntryRace")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("GOMAXPROCS entry race not contained (got %q, want \"done\")", out)
	}
}

// TestDSTGOMAXPROCSDelayedSTWDropped pins the runtime side of the entry-race
// closure deterministically: a GOMAXPROCS/SetDefaultGOMAXPROCS call held
// between its not-active gate and its stop-the-world (the
// computeMaxProcsLock-contention shape) while a simulation activates must
// have its update dropped by the post-STW dstActive re-check.
func TestDSTGOMAXPROCSDelayedSTWDropped(t *testing.T) {
	out := runTestProgDST(t, "DSTGOMAXPROCSDelayedSTW")
	if strings.TrimSpace(out) != "done" {
		t.Fatalf("delayed-STW setter not dropped (got %q, want \"done\")", out)
	}
}

// TestDSTExploreAtomicAutoInstrument is the acceptance for the dst-race
// sync/atomic decision-point emission: an
// UNMODIFIED SUT whose outcome turns on same-address atomic order — the CAS
// winner, the last store/swap, an add racing a load, And/Or order, the typed
// API's out-of-line method calls, pointer/64-bit widths — or on len(ch) racing a
// send, must have DPOR reach BOTH outcomes (Outcomes==2, Exhausted==true)
// with no manual hooks. (The typed-API SUT exercises the OUT-OF-LINE method
// classification: sync/atomic is a noRaceFunc package, so its methods are not
// inlinable under -race and only the instrumented call site can announce.) Without the emission the decision commits inside
// TSan's NOSPLIT atomic assembly with no recorded transition and DPOR
// explores one class while still reporting Exhausted=true — the
// Completeness-boundary exclusion this feature removes.
func TestDSTExploreAtomicAutoInstrument(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the dst-race atomic auto-instrumentation build")
	}
	testenv.MustHaveGoBuild(t)
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skipf("race detector not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testenv.MustHaveCGO(t) // -race requires cgo
	exe := filepath.Join(t.TempDir(), "tp_race")
	buildTestProgExplicit(t, exe, "-tags=dst", "-race")
	out := runBuiltTestProg(t, exe, "DSTExploreAtomicAuto", "DSTSEED=1")
	for _, sut := range []string{"caswinner", "storeorder", "addload", "swaporder", "orand", "store64", "casptr", "typedcas", "lenobserve"} {
		if exploreField(t, out, sut+"Exhausted") != "true" {
			t.Fatalf("unmodified %s SUT did not cleanly exhaust under DPOR:\n%s", sut, out)
		}
		if n, err := strconv.Atoi(exploreField(t, out, sut+"Outcomes")); err != nil {
			t.Fatalf("bad %sOutcomes field in %q: %v", sut, out, err)
		} else if n != 2 {
			t.Fatalf("atomic decision-point emission missing or ineffective: DPOR reached %d "+
				"outcomes on the unmodified %s SUT, want 2 (both orders). Without the emission "+
				"the atomic commits in NOSPLIT race assembly with no recorded transition, so "+
				"DPOR cannot reverse the order and silently under-explores:\n%s", n, sut, out)
		}
	}
}

// TestDSTFaultRandStreamIsolation verifies the fault RNG is a stream independent
// of the scheduling RNG (DST-FAULT-REPLAY): drawing from the fault stream never
// advances the scheduling stream (so a fault policy's draw count cannot shift the
// goroutine interleaving), and the two streams derive from distinct roots for the
// same seed (not one a phase-shift of the other).
func TestDSTFaultRandStreamIsolation(t *testing.T) {
	before := runtime.DstSchedRandPeek()
	var acc uint64
	for i := 0; i < 256; i++ {
		acc ^= runtime.DstFaultRandDraw()
	}
	if after := runtime.DstSchedRandPeek(); after != before {
		t.Errorf("fault-RNG draws advanced the scheduling RNG (%#x -> %#x): streams not isolated", before, after)
	}
	if acc == 0 {
		t.Errorf("256 fault-RNG draws XORed to zero; the stream looks dead")
	}
	for _, seed := range []uint64{1, 2, 42, 0xDEADBEEF, 1<<63 + 7} {
		if !runtime.DstFaultSchedRootsDiffer(seed) {
			t.Errorf("seed %#x: fault and scheduling RNG roots coincide (not independent streams)", seed)
		}
	}
}

// TestDSTForeignGCActivationStretch: the foreign forced-GC refusal is keyed
// on the published simulated-process env, so it already holds in the
// run-entry stretch between the activation seed store and the bubble's
// creation — where a foreign cycle would silently stale the dstHeapBase
// baseline. Mutation: keying the refusal on an existing simulation bubble
// instead (dstSimBubble != nil) passes the mid-run guard tests but lets this
// stretch through ("not refused").
func TestDSTForeignGCActivationStretch(t *testing.T) {
	out := runTestProgDST(t, "DSTForeignGCActivationStretch")
	if strings.TrimSpace(out) != "refused" {
		t.Fatalf("forced GC in the activation stretch (got %q, want \"refused\")", out)
	}
}

// TestDSTUntaggedCodeFootprint pins the zero-code-footprint contract
// (design.md, "Untagged footprint"): in a non-`-tags dst` build, the
// dstBuild-guarded legs on the anchored paths below (panic, finalizer
// execution, NumCPU, generic AddCleanup, GC, gcForce, goroutine exit) fold
// out — the compiled symbols reference no runtime.dst* symbol. (The synctest legs
// are covered by the same constant-guard pattern but are not reachable from a
// plain main; they were verified by objdump at the change.)
func TestDSTUntaggedCodeFootprint(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips build-mode objdump checks")
	}
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := []byte(`package main

import "runtime"

type big struct{ buf [64]byte }

func main() {
	println(runtime.NumCPU())
	b := &big{}
	runtime.SetFinalizer(b, func(*big) { println("f") })
	runtime.AddCleanup(new(int), func(int) {}, 0)
	b = nil
	runtime.GC()
	defer func() { recover() }()
	panic("x")
}
`)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), src, 0o666); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "app")
	cmd := testenv.CleanCmdEnv(exec.Command(testenv.GoToolPath(t), "build", "-o", exe, "main.go"))
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, probe := range []struct {
		pattern       string
		mustBePresent bool // guards vacuity: a rename/inlining drift must fail loudly, not pass silently
	}{
		{"runtime.gopanic$", true},
		{"runtime.runFinqBlocks$", true},
		{"runtime.NumCPU$", false},   // may be fully inlined away — trivially clean
		{"runtime.AddCleanup", true}, // the user-package generic instantiation (prefix match)
		{"runtime.GC$", false},       // the foreign-caller refusal folds; GC may inline into callers
		{"runtime.gcForce$", true},   // the forced-cycle protocol behind GC
		{"runtime.gdestroy$", true},  // the per-g DST clear + drain unhook fold into one gated call
	} {
		cmd = testenv.CleanCmdEnv(exec.Command(testenv.GoToolPath(t), "tool", "objdump", "-s", probe.pattern, exe))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("objdump %s: %v\n%s", probe.pattern, err, out)
		}
		if !strings.Contains(string(out), "TEXT ") {
			if probe.mustBePresent {
				t.Errorf("untagged binary has no symbol matching %s — the anchor drifted; re-anchor the fold test", probe.pattern)
			}
			continue
		}
		if strings.Contains(string(out), "runtime.dst") {
			t.Errorf("untagged %s references runtime.dst symbols:\n%s", probe.pattern, out)
		}
	}
}

// TestDSTDisabledVisibilityDirect is the DIRECT white-box pin for the
// disabled-goroutine visibility rule: during a GC schedEnableUser(false)
// window, user goroutines must be invisible to every candidate-selection
// scan — at() reports them nil, hasSimG must not feed one into the
// alternation hand-off, anyVisible must not see one, and simCount must not
// count one. The composed regressions cross a real window only when the
// wall-timed GC-worker quota happens to decline into the DST seam, so a
// partial weakening (one scan bypassing the filter, or the hand-off probe
// counting invisible candidates) was caught only probabilistically; this
// probe flips the window deterministically over a fabricated candidate view
// of real goroutines. Mutation: dropping the disableUser filter from at(), or
// routing hasSimG/anyVisible/simCount through the unfiltered raw accessor,
// flips an in-window assertion here.
func TestDSTDisabledVisibilityDirect(t *testing.T) {
	gs := make(chan unsafe.Pointer, 2)
	release := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			gs <- runtime.DSTProbeG()
			<-release
		}()
	}
	g1, g2 := <-gs, <-gs
	defer close(release)
	inAt, inAnySim, inAnyVisible, inSimCount, outAt, outAnySim := runtime.DSTDisabledVisibilityProbe(g1, g2)
	if inAt {
		t.Error("a user goroutine is visible to at() inside a disabled-user window")
	}
	if inAnySim {
		t.Error("hasSimG counts an invisible (disabled) simulation candidate; the alternation hand-off would burn a slot on a goroutine the window hides")
	}
	if inAnyVisible {
		t.Error("anyVisible sees a candidate the window hides; the all-invisible nil-return leg would never fire")
	}
	if inSimCount != 0 {
		t.Errorf("simCount = %d inside the window, want 0", inSimCount)
	}
	if !outAt || !outAnySim {
		t.Error("candidates invisible outside the window; the probe view is wrong (the pin would be vacuous)")
	}
}

// Goroutine exit clears every per-g DST field, so a recycled g cannot carry
// a dead goroutine's identity or RNG root into its next life — the
// unseeded-crypto gate keys on dstrand == 0 and the stable-index paths on
// dstSeq == 0, and a stale nonzero defeats both silently. Waves of stamped
// goroutines exit and waves of checkers reuse their gs. Reachability is
// asserted, not assumed: at least one checker must land on a stamped g, or
// the test reports itself vacuous instead of passing.
func TestDSTGoroutineExitClearsPerGState(t *testing.T) {
	if !runtime.DSTBuild {
		t.Skip("untagged build: goroutine exit does not clear DST fields (nothing stamps them)")
	}
	const n = 100
	stamped := map[uintptr]bool{}
	var mu sync.Mutex
	recycled := false
	for wave := 0; wave < 3; wave++ {
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				addr := runtime.DSTTestStampSelfG()
				mu.Lock()
				stamped[addr] = true
				mu.Unlock()
				wg.Done()
			}()
		}
		wg.Wait()
		runtime.Gosched()
		var checkers sync.WaitGroup
		var residue atomic.Bool
		checkers.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				addr, r := runtime.DSTTestSelfGResidue()
				if r {
					residue.Store(true)
				}
				mu.Lock()
				if stamped[addr] {
					recycled = true
				}
				mu.Unlock()
				checkers.Done()
			}()
		}
		checkers.Wait()
		if residue.Load() {
			t.Fatal("a recycled g carries per-g DST state after goroutine exit")
		}
	}
	if !recycled {
		t.Fatal("no checker landed on a stamped g: the test is vacuous, re-shape the churn")
	}
}
