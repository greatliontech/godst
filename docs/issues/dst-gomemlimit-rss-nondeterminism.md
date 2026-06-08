# GOMEMLIMIT and RSS-derived MemStats are nondeterministic under DST

**Lands:** open — decide what GOMEMLIMIT should mean under DST. Not blocking: under DST memory is bounded deterministically by the GOGC-relative trigger (and, for GOGC=off, by the heapMinimum floor — `mgc.go` `gcTrigger.test`); GOMEMLIMIT is currently ignored by the trigger. This issue is a placeholder for a real design pass, not a settled "not modeled" decision.

## What was observed (Chunk D probes)

- `gcController.mappedReady` (total mapped, unreleased memory) at bubble entry is
  nondeterministic across same-seed runs: measured `4888840 / 4856072 / 4970760`
  (~115 KB spread). Source: mmap-arena history + ASLR + scavenger-off accumulation
  + span layout.
- `MemStats.HeapReleased` is nondeterministic (~0.5 MB swing across runs) — sweep-time
  `madvise` on heap-layout-dependent freed spans, even with the scavenger parked (D5).
- `memoryLimitHeapGoal = memoryLimit − nonHeapMemory − overage`, and both
  `nonHeapMemory` and `overage` derive from `mappedReady`. So if the DST heap
  trigger honors the GOMEMLIMIT goal, numGC inherits the `mappedReady` noise: at a
  tight limit (GOMEMLIMIT=8 MiB ≈ 8 cycles) the per-cycle noise accumulates past a
  crossing and numGC wobbles (measured 8 / 9 / 8 / 8). At looser limits it is
  stable (fewer cycles → less accumulation).

## Why A.5's technique does not transfer

A.5 made the GOGC trigger deterministic by making it **bubble-local**: the target
is proportional to the bubble's *own* live set (`heapMarked − dstHeapBase`), which
is deterministic for a deterministic workload. GOMEMLIMIT's semantics are
inherently **total-process-memory** (`limit − total-non-heap-overhead`); the
non-bubble overhead (`mappedReady`-derived) is not bubble-local and carries
process-history noise that an entry snapshot only captures, not cancels. So a
deterministic GOMEMLIMIT under DST would have to *discard* the real total-memory
baseline and approximate it — which abandons GOMEMLIMIT's actual meaning (bound
total RSS), itself not faithfully modeled under DST because the scavenger is
parked.

`HeapReleased`, `HeapIdle`, and GOMEMLIMIT are therefore one root cause: total
mapped/RSS memory is not deterministically modeled under DST.

## Current behavior (after Chunk D)

- GOMEMLIMIT is **ignored** by the DST heap trigger (the A.5 relative trigger uses
  only GOGC). A GOGC=on + GOMEMLIMIT SUT is memory-bounded by the GOGC trigger
  deterministically; a tighter GOMEMLIMIT is not enforced.
- GOGC=off + GOMEMLIMIT: memory is bounded by the heapMinimum floor (Chunk D fix),
  deterministically — but at the floor, not at the GOMEMLIMIT goal.
- A SUT that reads `HeapReleased` / `HeapIdle` (RSS-derived stats) sees
  nondeterministic values.

## Options to weigh later

1. **Leave GOMEMLIMIT ignored** under DST; the GOGC-relative trigger (+ heapMinimum
   floor) is the memory bound. Honest about RSS being unmodeled, but a SUT that
   sets GOMEMLIMIT to bound memory tighter than GOGC does not get that.
2. **Honor GOMEMLIMIT faithfully**, accepting ±1 numGC nondeterminism for
   GOMEMLIMIT-tight configs (a documented relaxation of GC-set-level determinism
   for those configs).
3. **Deterministic bubble-local approximation**: define a budget from GOMEMLIMIT
   that ignores the (noisy, non-bubble) total-memory baseline — deterministic, but
   changes GOMEMLIMIT's semantics under DST from "bound total RSS" to "bound bubble
   heap growth." Needs its own validation.
4. **Zero/synthesize RSS stats under DST** (`HeapReleased` etc.) so `ReadMemStats`
   is fully deterministic at observable granularity — separate from the trigger
   question.

Reproduce with the throwaway probes in the Chunk D history (the `DSTMemProbe` /
`DSTMappedReadyProbe` testprog functions, and the `memoryLimitHeapGoal` patch to
`gcTrigger.test`).
