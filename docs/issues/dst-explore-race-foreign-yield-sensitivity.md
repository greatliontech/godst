# DST: -race exploration yield placement is foreign-sensitive (coverage shrinks under churn)

Lands: when the dst-race yield placement's foreign sensitivity is diagnosed
and either removed or recorded in the spec as a bounded, reported limit

## Gap

Severity M (found 2026-07-10 while landing the foreign-livelock fix;
reproduced, mechanism undiagnosed). Under `-race` (dst-race
auto-instrumentation), an exhaustive exploration of a race-free finalizer
workload explores 12 schedules foreign-free but only 6 with two foreign
Gosched spinners churning, and single-episode traces diverge alone-vs-spun —
while without `-race` the same shapes are byte-identical
(`TestExploreForeignSpinner`, `TestExploreForeignSpinnerDrainCallback`). The
foreign goroutines never enter recorded schedules or enabled sets (pinned),
so the sensitivity is in where the auto-instrumented yield points fall —
candidate mechanisms: the shared-address promotion filter's cross-goroutine
observations, TSan-side state, or safe-point guard interactions — not in the
selection seam itself.

Before the foreign-livelock fix this composition livelocked outright
(unconditional infrastructure-first starvation), so no coverage claim was
possible at all; the fix made it complete, and `ExploreResult.ForeignSched`
now reports foreign presence at simulation decisions and downgrades
`Exhausted` — the loss is loud, not silent. This issue tracks the root cause:
why instrumented yield placement depends on foreign activity, and whether it
can be made insensitive (restoring full-coverage claims under churn) or must
be recorded as a bounded limit.

Also in scope: the silent-replay-miss path. `Failure` carries no enabled
sets, and the replay-divergence check aborts only when a prefix entry names a
non-enabled goroutine — under `-race` with churn, a shifted auto-yield can
keep every prefix seq enabled while silently executing a different
interleaving, returning `failed=false` with no diagnostic.
`Failure.ForeignSched` marks such replay tokens best-effort; the root-cause
work must either make replay divergence detectable in this regime or record
the limit.

## Required outcome

Under `-race`, either exploration coverage is foreign-insensitive (the
spinner tests' trace-equality assertions extend to race builds and the
`ForeignSched` downgrade can be narrowed), or the spec records the
sensitivity as a deliberate bounded limit with its mechanism named, with
`ForeignSched` remaining the reporting surface.
