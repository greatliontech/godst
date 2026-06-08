# Issue docs

Tracked follow-ups deferred out of a chunk. Each entry carries a `Lands:` trigger
(a chunk number or a condition). The chunk-start gate (sub-chunk `N.1`) scans this
index for entries resolving to the current chunk; the close-out gate promotes any
load-bearing rationale inline and deletes the resolved entry.

| Issue | Lands | Summary |
|-------|-------|---------|
| [dst-race-percycle-gc-timing](dst-race-percycle-gc-timing.md) | when a SUT needs byte-exact per-cycle GC timing under -race, or when a race-invariant DST GC trigger is designed | Under -race the per-cycle GC-discovery split is bimodal (±1-span flip, run-to-run): numGC and the total finalizer set stay deterministic, but which cycle discovers a given object can flip. The byte-based span-granular trigger is perturbed by -race within the bubble's own accounting, which A.5's baseline subtraction can't cancel. Clean fix needs a logical-allocation trigger (a real redesign; runtime lacks a logical live-set) for a narrow benefit. |
