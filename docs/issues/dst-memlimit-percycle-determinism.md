# `Options.MemoryLimit`-governed cycles: per-cycle GC discovery still span-granular

**Lands:** when a SUT needs per-cycle GC-discovery determinism *under a
`Options.MemoryLimit`*, or alongside the next change to the memlimit trigger. No
demonstrated SUT need today — elective, completeness only.

## What holds and what doesn't

Phase 2a made per-cycle finalizer/weak discovery deterministic under `-race` by
driving the DST heap trigger off **per-object** allocated bytes (`dstHeapAlloc`,
summed at the `mallocgc` dispatcher) instead of span-granular physical `heapLive`.
That covers the two GOGC branches of `gcTrigger.test` (`mgc.go`): the floored case
(`target == heapMinimum`) and the GOGC-scaled case
(`target == (heapMarked − dstHeapBase)·GOGC/100`). Both now fire on
`dstHeapAlloc ≥ target`, so *which cycle* discovers a given object is a
deterministic function of the seed in normal and `-race` builds, and across binary
compositions.

**The `Options.MemoryLimit` crossing was left physical.** The memlimit check is the
first branch of the DST `gcTriggerHeap` case:

```go
if live := gcController.heapLive.Load(); dstMemLimit > 0 && live > base && live-base >= uint64(dstMemLimit) {
    return true
}
```

It fires on physical `heapLive − dstHeapBase` (bubble heap growth from entry). When
a `MemoryLimit` is set and *this* crossing governs a cycle — reachable with
`GOGC=off` + a limit, or `GOGC` on where the limit is hit before the GOGC target —
that cycle's boundary is span-granular again, so its per-cycle discovery split is
**not** `-race`-deterministic. The cycle is still **set-level** deterministic: the
GC *count* under the limit is reproducible (the limit is on `heapLive − base`, a
bubble-local quantity), guarded by `TestDSTMemoryLimit`. So a SUT that uses
`MemoryLimit` and asserts only `numGC` / the total discovered set is unaffected; one
that uses `MemoryLimit` *and* asserts per-cycle discovery timing hits the gap.

## The fix (when pursued)

Drive the memlimit crossing off a deterministic per-object measure too. The limit
bounds *total* bubble growth (from entry), not per-cycle growth, so it needs a
**cumulative** per-object allocated-bytes counter (sum of `elemsize` since bubble
entry, never reset per cycle) compared against `dstMemLimit` — the cumulative
analogue of `dstHeapAlloc` (which resets at each `resetLive`). Both can share the
`mallocgc`-dispatcher accumulation site. Note the redefinition this implies: the
limit then bounds the bubble's *logical/per-object* total growth rather than
physical `heapLive`; both are bubble-local and deterministic at the set level, but
the per-object one is also per-cycle race-deterministic. Confirm the memlimit
set-level tests (`TestDSTMemoryLimit`, incl. its baseline-independence check) still
hold under the switch, and add a per-cycle assertion (extend
`TestDSTGCPerCycleDiscoveryDeterministic` with a `MemoryLimit` regime).

## Provenance

Surfaced by the Phase 2a adversarial review (finding M1): the layered-contract
table in design.md claimed per-cycle "holds" unconditionally, but the memlimit
branch was untouched by Phase 2a. The table now scopes the claim to the GOGC
trigger and points here.
