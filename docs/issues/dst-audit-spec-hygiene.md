# DST audit: spec-hygiene defects (stale status, undefined invariant IDs, planning codenames)

Lands: when the spec docs are reconciled with the landed surface and the artifact-homes contract

## Gap

Severity M/L (full-surface audit, 2026-07-10; spec walk). Defects in the
kept-current spec docs (docs/dst/), none a code bug but each a
reader-misleading or contract-violating artifact:

- **M — faults.md DST-MEMALLOC-DET internally contradictory.** faults.md:277-283
  claims `dstActivate` "normalizes the per-P runtime pools (sudog and defer
  caches, the gFree list) to a fixed empty state". No such normalization exists
  in the runtime; `dstActivate` (`src/runtime/dst.go:136`) does none, and the
  landed mechanism is the opposite shape — pooled-type exclusion
  (`dstIsInternalPooledType`, `dst.go:28-47`), which faults.md:168-172 itself
  describes, making the invariant's own noise scenario impossible. The gc.md
  cite is dangling (gc.md never mentions normalization).

- **M — design.md stale "pending" markers.** design.md:37-38 calls crash/restart
  and scheduling "the remaining pending features" though the same doc's source
  table (design.md:960-961) marks process and host crash ✅ and the code/tests
  are landed; design.md:962 lists "seeded drift" pending though `BoundedDrift`
  is landed and pinned; design.md:1107 says "no … locking yet" though BSD
  `syscall.Flock` is specified (design.md:551-560) and implemented.

- **L — undefined invariant IDs.** `DST-FIN-1`, `DST-FIN-2`, `DST-FIN-3`,
  `DST-CLEANUP-1` are cited as IDs in gc.md (:288, :527) and across runtime
  comments (proc.go:3472/3486/4568, mfinal.go:499/605, mcleanup.go:1006,
  traceback.go:1422) but gc.md never defines them — the sanctioned
  ID↔enforcement-pointer scheme cannot be resolved.

- **L — planning codenames in kept-current artifacts.** gc.md uses plan-chunk
  labels ~25× ("Chunk A/B/C/D/G", "lands with the GC-determinism chunk"
  gc.md:287, "lands with Chunk B's drain" gc.md:696); exploration.md:611 "As
  built so far (this session)"; code comments "exercises ONLY L1"
  (determinism_test.go:82), "before Seq 5" (dst_test.go:1205). With no plan
  files existing, every chunk-named `Lands:` is unresolvable. Per the
  artifact-homes contract these must be self-contained conditions or
  descriptive names. Also: faults.md:193 (DST-NODE-ISOLATION) carries a stale
  `Lands:` naming a chunk of a deleted plan for an already-landed, pinned env
  leg; faults.md:3/10/326 say "implementation pending" beside the doc's own
  "landed" rows; faults.md:62/85 describe pre-L2 code shapes that no longer
  exist.

## Required outcome

The spec docs state the current contract with no stale status narrative, no
internally contradictory invariant, defined-or-removed invariant IDs, and no
planning codenames — every `Lands:` a self-contained condition. Reconciled
against the landed surface as verified by the spec walk.
