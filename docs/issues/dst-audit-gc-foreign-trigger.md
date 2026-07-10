# DST audit: a latched GC trigger can be started by a foreign allocation, shifting cycle boundaries

Lands: chunk 3 of docs/plans/dst-audit-fixes.md

## Gap

Severity M (full-surface audit, 2026-07-10; latent, mechanism cited). The
gc.md M4 closure states the GC a DST trigger arms "is started inside the
bubble-allocation gate … never for a foreign/infra allocation that happens
next". Mechanism that violates it: (1) a bubble-attributed allocation on g0 is
counted — `mallocgc`'s DST gate keys on `m.curg` (`src/runtime/malloc.go:1208`),
and `allgadd` (`proc.go:695`, reached from `newproc1` on systemstack) grows
`allgs`, whose `[]*g` backing array is not an excluded pooled type, so
`dstHeapAlloc.Add` runs; (2) if that Add crosses the target, the dispatcher-gate
`gcStart` bails on g0 (`mgc.go:820-824`), leaving `gcTrigger{gcTriggerHeap}.test()`
latched true; (3) the next allocation to run an inner `checkGCTrigger` site
(`malloc.go:1396,1508,1648,1741,1816`, arena.go:814 — not bubble-gated, they
evaluate the DST condition for any allocator under `dstActive`) starts the GC.
If that allocator is a foreign goroutine, `resetLive` zeroes `dstHeapAlloc` at a
wall-clock-dependent point, shifting every later crossing — `numGC` and
per-cycle finalizer/weak discovery (DST-GC-1/DST-MEM-1, pinned by
`TestDSTGCPerCycleDiscoveryDeterministic`) diverge run-to-run. Requires foreign
allocation activity mid-run to actually diverge, so current tests pass.

## Required outcome

The DST heap-trigger crossing is started only from within the bubble-allocation
gate; an inner `checkGCTrigger` reached by a foreign allocation cannot start the
DST-armed cycle. The structural gap between the enforced code and the gc.md M4
claim is closed. Pinned by a test with foreign allocation interleaved against a
near-threshold bubble heap, asserting cycle boundaries are seed-stable.
