# DST: foreign runtime.GC() mid-run shifts the deterministic trigger stream

Lands: when a foreign-goroutine runtime.GC() during an active run either
cannot perturb the bubble's trigger stream or fails loudly

## Gap

Severity M (review-found 2026-07-10; pre-existing). `runtime.GC()`
(`gcTriggerCycle`) is not gated on the caller being a simulation goroutine: a
non-bubble goroutine calling it during an active run starts a full cycle, and
the cycle's `resetLive` zeroes `dstHeapAlloc` at a wall-clock-dependent point
— the same downstream crossing shift as the allocation-path leak the
trigger-start confinement closed, through a different entry. User- or
harness-controlled (a background goroutine calling runtime.GC is a plausible
composition), so it sits outside the M4 allocation-gate contract but inside
the same determinism invariant.

## Required outcome

A foreign `runtime.GC()` during an active run either leaves the bubble's
trigger accounting unperturbed (e.g. deferred past the run or executed
without resetting the bubble counter), or fails loudly and deterministically
like other foreign interference with an active run. The chosen behavior is
recorded in gc.md beside the trigger-start contract.
