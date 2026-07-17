// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package sync

// dstMutexVirtualStarvation compile-time gates the in-bubble starvation-mode
// measure in lockSlow: true only in a -tags dst build, false in
// dst_mutex_off.go, so the branch is a guaranteed const fold and untagged
// builds keep upstream's exact wall-clock path.
//
// Inside a simulation bubble the wall clock cannot decide the starvation
// flip: whether a waiter's wall wait crossed 1ms made lock-acquisition order
// a function of machine speed, host load, and GC pause wall time — a
// demonstrated same-seed schedule escape
// (TestMutexStarvationHandoffDeterministic). And the bubble's fake clock
// was rejected too: when this landed, mutex waits were not durably
// blocking, so virtual time never passed while a waiter was pending — the
// flip could never fire, and a production-legal SUT whose progress depends
// on the starvation handoff (a holder re-barging at every release while
// the parked waiter must acquire for the program to advance) livelocked
// in-sim where production always terminates, undetectably. In-bubble mutex
// waits have since become DURABLE (runtime.dstDurableMutexWait), which
// removes that impossibility — but not the choice: the lost-wakeup count
// below stays the seed-pure trigger, independent of any clock.
//
// The in-bubble measure is therefore the waiter's own LOST-WAKEUP count — a
// pure function of the seeded schedule: the waiter counts its returns from
// semacquire within one Lock call (evaluated at every wake, exactly where
// the wall measure is; a count beyond the threshold means that many lost
// handoffs) and flips its mutex to starvation mode past
// dstStarvationWakeThreshold, mirroring production's per-waiter structure
// (production measures the waiter's own wait at each wake, sticky until it
// acquires; the exit conditions are shared — see lockSlow). Soundness holds
// in both directions: production flips after ANY wait exceeding 1ms, and a
// waiter that lost N wakeups has waited >1ms in some real execution
// (scheduling wall time is arbitrary), so every flipped in-sim execution is
// production-producible — while an execution that never flips is production
// below the threshold. Only the flip POINT differs from any single
// production run, never the reachable behavior set.
const dstMutexVirtualStarvation = true

// dstStarvationWakeThreshold is the in-bubble starvation flip point: a
// bubbled waiter that has lost this many consecutive wakeups flips its mutex
// to starvation mode on the next one. Large enough that barging stays the
// common case (production engages starvation mode rarely; a short barge
// streak must not flip in-sim where production would not have crossed 1ms),
// small enough that a starvation-dependent SUT terminates promptly.
const dstStarvationWakeThreshold = 64
