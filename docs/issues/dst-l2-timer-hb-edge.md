# Deferral: validate the timer-fire happens-before edge for Level-2 DPOR

**Lands:** when timer-based interleavings are explored (a `time.Sleep`/timer-gated
SUT is added to the Explore corpus, or the explorer is applied to SUTs that gate
shared accesses behind timers).

## Context

Level-2 DPOR (design.md "Level 2 — access-granularity interleaving + DPOR",
increment 2) prunes a conflicting access pair from the dependency relation when the
two accesses are **happens-before-ordered**. The happens-before relation is built
offline from `goready` edges recorded under the scheduled strategy
(`dstRecordReadyEdge` → `dporClocks`/`dporConcurrent` in
`testing/simulation/explore.go`): a `goready(readier, readied)` is taken as
"readier happens-before readied's resumption."

The soundness of pruning depends on every recorded edge being a **real**
happens-before edge: a spurious edge (claiming order where the memory model has
none) would let DPOR prune a genuinely-concurrent conflicting pair and **miss a
reachable interleaving** while still reporting `Exhausted=true` — a silent
completeness (DST-L2-3) violation.

## The specific concern

`goready` is also called from the timer path (`time.go` `goroutineReady`) to wake a
goroutine blocked in `time.Sleep`/a timer when virtual time advances. That records
an edge `(timer-processor) → (sleeping goroutine)`. In Go's memory model there is no
genuine happens-before from whatever goroutine advanced time to the sleeper, so this
edge is "spurious" in memory-model terms. If the processor is a bubble goroutine
that performs SUT memory accesses, the edge could transitively **over-order** a race
that is gated behind a `time.Sleep`, pruning a reachable interleaving.

It is **defensible within DST semantics** — under the fake clock the sleeper
genuinely cannot run until virtual time advances (driven by the synctest root after
quiescence), so the ordering reflects the only execution DST can produce — and the
adversarial review could not construct a failing case. But **no timer-based SUT
exercises it**, so it is unverified.

## What to do when this lands

- Add a timer-gated completeness SUT to the Explore corpus: two goroutines racing on
  a shared variable, one gated behind a `time.Sleep`, where the sleep-fire edge could
  transitively order the race. Assert HB-DPOR reaches the same outcome set as the
  exhaustive explorer (the existing `TestDSTExploreComplete` shape).
- If over-ordering is confirmed, exclude timer-fire `goready`s from the recorded HB
  edges (they are not application synchronization), or otherwise mark timer wakes as
  non-ordering. Under-recording an edge only ever *over-explores* (sound); the danger
  is exactly the opposite (a spurious ordering edge), so the fix is to drop the timer
  edge, not add one.

## Status

Not yet reachable (no timer-based interleaving exploration). Tracked so the HB
pruning is re-validated before timer-gated SUTs rely on it.
