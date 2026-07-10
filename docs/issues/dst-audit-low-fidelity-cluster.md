# DST audit: low-severity fidelity and hygiene divergences

Lands: chunks 28–29 of docs/plans/dst-audit-fixes.md (per item; chunks 4, 9, 11–17, 24, 25 landed)

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

- **nits:** Explore misattributes
  fan-out overflow as BudgetHit under `MaxSteps` (explore.go:452-455).

## Required outcome

Each item is either corrected to match host behavior or recorded in the spec as
a deliberate modeled limit with rationale. The zero-footprint claim touches a
stated contract; the rest are fidelity notes.
