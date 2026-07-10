# DST audit: low-severity fidelity and hygiene divergences

Lands: chunks 4, 9, 11–17, 24, 25, 28, 29 of docs/plans/dst-audit-fixes.md (per item; chunk 29 is the last)

## Gap

Severity L/nit (full-surface audit, 2026-07-10). A cluster of small,
deterministic divergences from host behavior and minor hygiene defects, each
verified, none blocking:

- **Untagged zero-footprint claim overstated.** `finalizer` (mfinal.go) grew
  5→6 words and `cleanupFn` (mcleanup.go) 3→4 words unconditionally, so
  untagged builds carry a dead `dstSeq` word and fit fewer entries per block;
  `NumCPU` (debug.go:267) branches on a runtime var, not a build const, so it
  is not dead-code-eliminated untagged. No behavior change; the spec's "zero
  footprint" note is inaccurate.

- **Same-host connections get an unbounded send buffer and no horizon**
  (dst.go:986): two co-located peers each writing ≫1 MiB before reading
  deadlock in production but succeed in sim (masks a real deadlock, unbounded
  sim memory). Recorded in a code comment, not the spec — spec-amend candidate.

- **nits:** Explore misattributes
  fan-out overflow as BudgetHit under `MaxSteps` (explore.go:452-455).

## Required outcome

Each item is either corrected to match host behavior or recorded in the spec as
a deliberate modeled limit with rationale. The proc-fd `Fd()` contradiction and
the zero-footprint claim are the two that touch stated contracts; the rest are
fidelity notes.
