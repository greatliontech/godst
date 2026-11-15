// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

// Sometimes evaluates a named deterministic inline fault point ("buggify")
// inside simulated system-under-test code — such as forcing a non-blocking
// cache eviction, early Raft snapshot compaction, short batch flush, or early
// B+Tree page split.
//
// Argument validation runs uniformly in both untagged (!dst) and -tags dst
// builds before checking run liveness: an empty name or a probability outside
// [0, 1] (including NaN or ±Inf) panics immediately. With valid arguments,
// Sometimes returns false whenever the caller is outside an active simulation
// bubble (including all untagged builds, inactive runs, background cleanups or
// finalizers registered outside the run, foreign goroutines, and foreign
// synctest bubbles) with zero allocation or side effect.
//
// Inside an active simulation bubble (under -tags dst), Sometimes uses a
// two-level FoundationDB-style deterministic gate:
//  1. Per-(seed, name) site enablement: prob == 0 always returns false and
//     prob == 1 always returns true. For 0 < prob < 1, a stateless hash of
//     (seed, name) enables each distinct named site on approximately 50% of
//     seeds and disables it completely on the remaining seeds so multiple
//     independent fault sites do not fire simultaneously on every seed.
//  2. Stream-isolated per-goroutine firing: when enabled, Sometimes draws from
//     a dedicated per-goroutine RNG fork-tree isolated from math/rand,
//     crypto/rand, map/select randomness, scheduler decisions, and
//     harness-level fault draws, without emitting synchronization or memory
//     access events to Level-2 DPOR or the race detector. Adding, removing, or
//     evaluating a false-returning Sometimes site leaves schedules, application
//     randomness, heap accounting, and Explore equivalence-class reduction
//     unchanged.
//
// Every named site that returns true during a run is recorded off-heap and
// included in the failure-only simulation diagnostics rendered by Test,
// TestWith, WorldTest, and ExploreTest.
//
// Libraries that build production binaries with stock Go (which does not ship
// testing/simulation) typically wrap Sometimes behind a two-file internal
// build-tag helper returning false under //go:build !dst and forwarding to
// simulation.Sometimes under //go:build dst.
func Sometimes(name string, prob float64) bool {
	if name == "" {
		panic("testing/simulation: Sometimes name must be non-empty")
	}
	if !(prob >= 0 && prob <= 1) {
		panic("testing/simulation: Sometimes prob must be in [0, 1]")
	}
	return dstSometimes(name, prob)
}
