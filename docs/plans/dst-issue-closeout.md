# Plan: close the open issue backlog

Derived from the 13 open entries in `docs/issues/README.md`; ordered bottom-up
through the stack, grouped by function. Each chunk resolves the named issue doc
(promote-then-delete at close-out) or converts it to a recorded, spec-tier
bound where the issue's own outcome clause allows it.

## Runtime — GC determinism

- [x] 1. Foreign `runtime.GC()` mid-run cannot perturb the bubble trigger stream, or fails loudly (`dst-gc-foreign-runtime-gc`)
- [x] 2. `GOEXPERIMENT=sizespecializedmalloc` refusal made precise (instrumented builds exempt) and pinned at a testable level (`dst-gc-sizespecialized-experiment`)
- [x] 3. Warm-process late-discovery divergence diagnosed; tail repeatable in-process or the bound recorded (`dst-gc-warm-process-discovery-tail`)

## Runtime — simulated clock

- [x] 4. Overdue-timer backwards remap honors the spec formula's negative remainder, sentinel-safe (`dst-clock-overdue-ticker-phase`)

## Runtime — exploration

- [x] 5. Explore recording keys on sim-bubble membership at the seq/access chokepoints (`dst-explore-foreign-bubble-seq-pollution`)
- [x] 6. `-race` yield-placement foreign sensitivity diagnosed; removed or recorded as a bounded, reported limit (`dst-explore-race-foreign-yield-sensitivity`)
- [x] 7. DPOR truncated-child continuation pinned by a discriminating SUT, or brittleness demonstrated (`dst-explore-dpor-truncation-continuation-pin`)

## Filesystem & page cache

- [x] 8. O_SYNC commit fires only for writes that wrote (`dst-fs-osync-zero-write-overcommits`)
- [ ] 9. HostFS inspection is side-effect-free on simulation state (`dst-hostfs-inspection-allocates-inodes`)
- [ ] 10. Page-cache creation atomic w.r.t. in-flight host close, or the window proven empty (`dst-memfd-inflight-close-toctou`)

## Network

- [ ] 11. TIME_WAIT close-time hold modeled for active-close dialer 2-tuples, or divergence confirmed kept (`dst-net-time-wait-unmodeled`)

## Simulation API surface

- [ ] 12. `Host`/`Process` gain the fault APIs' caller-position guard, or the declaration-caller contract is recorded (`dst-sim-topology-apis-from-nonbubble`)
- [ ] 13. Fault-guard activation-edge TOCTOU closed, or the acceptance recorded at the guard (`dst-sim-fault-guard-runstart-toctou`)
