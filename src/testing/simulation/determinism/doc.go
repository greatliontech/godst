// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package determinism holds the same-seed determinism sweep for the
// deterministic simulation (docs/dst/design.md is the authoritative
// contract: within a run, the schedule and every seeded observable are a
// pure function of the seed). The package carries no library code:
// everything lives in dst-tagged Linux test files, run by the
// `test:determinism` Taskfile leg and, for the race-detector
// configuration, by `test:dst-race`.
//
// Method: one representative in-bubble program — multiple hosts with skewed
// clocks, co-located processes, the in-memory network under cross-host
// latency, the simulated filesystem with fsync/rename durability points,
// timers and tickers on the virtual clock, channel and select races,
// value-keyed map iteration, math/rand and crypto/rand draws, and a
// contended mutex — records a transcript of both its values and its
// scheduling order. The sweep pins that transcript byte-identical:
//
//   - across repeated runs in one process, over a seed sweep;
//   - across PROCESSES (the test binary re-executes itself with the seed in
//     the environment), which subsumes address-layout perturbation — every
//     child has its own heap and mmap layout, so value-keyed map iteration
//     equality across children is the ASLR-class probe;
//   - under environmental perturbation: the child processes run with a
//     changed TZ, LC_ALL/LANG, working directory, and GOMAXPROCS — none may
//     reach a decision inside a run;
//   - under the race detector (the test:dst-race leg runs this package):
//     instrumentation must not shift the seeded schedule.
//
// The map hash key needs no GODEBUG perturbation axis: under -tags dst the
// key is a fixed constant derived position-independently (design.md, "Map
// hash key requires -tags dst"), so it is identical across builds and
// processes by construction; the untagged legs keep upstream's per-process
// key.
//
// The package also carries the schedule-DIVERSITY suite (the complement of
// determinism: different seeds must explore genuinely different schedules,
// or N seeds silently re-test one interleaving N times). It drives the
// runtime's seeded-decision trace — a default-off, observation-only
// diagnostic recording each seeded schedule-bearing decision's site,
// candidate-set size, and chosen index (runtime/dst.go, dstTraceState;
// measured record in docs/dst/exploration.md, "Measured seeded-path
// diversity") — and pins two invariants: enabling the trace leaves the
// schedule byte-identical (observation neutrality), and seed entropy
// visibly reaches every seeded choice site (scheduler pick, select poll
// order, fake-timer tie-break) with a distinct-schedule floor across the
// sweep. The diversity sweep's default width (32 seeds) is a bounded CI
// budget; DST_DIVERSITY_SEEDS=<n> in the environment widens it (read before
// any simulation starts).
//
// Recorded coverage bounds (deliberate, per the spec): pointer-keyed map
// iteration order and other address-derived observables are program
// discipline, outside the sweep; a pollable (source-nonblocking) inherited
// capability's deadline waits ride host readiness and are host-coupled by
// that mode's nature — same-seed transcript equality is NOT guaranteed
// while a pollable capability deadline (or EAGAIN-park) is armed, a scoped
// one-directional exclusion (design.md, "Determinism scope of the pollable
// arm"): programs that never park a pollable capability keep the full
// guarantee, and the sweep deliberately does not drive that arm; foreign
// (non-bubble) goroutines are scheduled around
// the simulation RNG-free, so their wall-timed wakes change when harness
// work runs, never which simulation decision comes next; a wall-blocked
// granted raw write delays the run in wall time only.
//
// A further recorded bound: PRE-RUN-minted entropy. Values drawn before a
// run starts and captured for in-run use — init-time map group placement,
// init-time maphash.MakeSeed / math/rand values, a pre-run-populated
// sync.Map — are stream-position-dependent host randomness, excluded from
// the same-seed guarantee (design.md, the sources table's pre-run
// qualifiers); the sweep carries no pre-run-state axis, deliberately, until
// pre-run entropy is seed-pure (an axis added first would flake at the
// probe-demonstrated ~8% divergence rate). Mint inside the run.
package determinism
