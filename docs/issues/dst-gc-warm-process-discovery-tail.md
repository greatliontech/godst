# DST: warm-process runs shift late GC discovery at equal NumGC

Lands: when the cold-vs-warm discovery divergence is diagnosed and the
in-process repeatability contract either holds for the tail or records the
bound

## Gap

Severity M (found 2026-07-10 while pinning the GC trigger-start confinement;
reproduced on the pre-fix HEAD, so pre-existing). Two in-process
`simulation.Run`s at one seed, whose body first holds ~1500 goroutines
concurrently live (forcing fresh g/stack allocation in run 1, pure reuse in
run 2) and then churns a finalizable ring, produce EQUAL NumGC (3) and — with
the trigger-start confinement landed — equal mid-run partial discovery, but
run-end discovery totals that differ by ~3k finalizers (e.g. 51544 vs 48351,
seed 12345). The sequential-spawn variant (the shape
`DSTGCPoolCarryoverDeterministic` pins) is identical across runs, so the
divergence needs the concurrent-goroutine phase.

Mechanism hypothesis (not yet confirmed): with boundaries driven by the
deterministic per-object counter and partials equal, the late GOGC-scaled
boundary can still shift via `bubbleMarked` — what the mark retains differs
between a cold process (fresh stacks) and a warmed one (stacks reused from
the goroutine phase, with different stale-slot contents), moving the
last cycle's target and hence its discovery tail. `DSTGCSysstackAlloc`
asserts the partial only and records this effect as not-owned.

## Required outcome

Same-seed in-process repeat runs agree on the full per-cycle discovery
sequence including the tail, or the spec records the warm-process bound (what
can shift, by how much, and why) as a modeled limit. The
`DSTGCSysstackAlloc` prog's total columns become assertable (or the spec
names why they never will be).
