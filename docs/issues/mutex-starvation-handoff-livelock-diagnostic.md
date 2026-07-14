# Mutex starvation-handoff livelock: detect, model, or leave recorded?

`Lands:` when the starvation-handoff diagnostic question is ruled on.

## Context

Under `-tags dst`, `sync.Mutex`'s starvation-mode switch is measured on the
bubble's fake clock (`runtime.internal_sync_nanotime`, `src/runtime/sema.go`).
Mutex waits are not durably blocking, so fake time never passes while a waiter
is pending: in-bubble handoff stays barging-mode, deterministically. On the
wall clock the 1ms threshold decision was a demonstrated same-seed schedule
escape (`TestMutexStarvationHandoffDeterministic`,
`src/testing/simulation/determinism/mutex_test.go`), so determinism won and
the gap was recorded — see docs/dst/design.md, "Nondeterminism sources and who
owns them", the `sync.Mutex` starvation-mode row: sound for every finite
execution prefix, NOT sound for liveness.

## The gap (the recorded false-positive hang class)

A production-legal SUT whose termination depends on the starvation-mode
handoff livelocks in the simulation where production cannot:

- the holder loops Lock / work / yield / Unlock, re-barging on every round;
- the waiter must acquire the mutex to set the exit flag;
- in production the waiter's wall wait crosses 1ms, the mutex flips to
  starvation mode, the unlock hands off directly, and the program terminates
  — always;
- in-sim the flip never fires (fake-clock wait is always ~0), the holder
  re-barges forever, and the run hangs UNDETECTABLY: the mutex wait is
  non-durable, so fake time never advances over it and the bubble-deadlock
  panic never fires.

## The open question (user's call)

Should the fork:

1. **Detect the shape and diagnose loudly** — e.g. a bubbled goroutine parked
   on a mutex beyond N scheduler decisions while the holder keeps
   re-acquiring → a panic/report naming this recorded gap (turning the silent
   livelock into a directed diagnostic), without changing handoff order; or
2. **Model the flip on virtual progress** — switch to starvation mode after a
   deterministic count of scheduler decisions (or another seed-pure measure)
   instead of 1ms of wall time, restoring the liveness property at the cost
   of a handoff order production would reach at a different point; or
3. **Leave it recorded** — the design.md row and the
   `runtime.internal_sync_nanotime` comment stand as the only artifacts.

Options 1 and 2 both keep the wall clock out of the schedule; they differ in
whether the simulation reproduces starvation-mode semantics or merely refuses
to hang silently where production would have engaged them.
