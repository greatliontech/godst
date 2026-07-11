# DST explore: the foreign-GC-workload invariance test flakes ~20% on HEAD

Lands: when TestExploreForeignGCWorkloadInsensitive AND TestDSTExploreTimerHB
are stable across repeated invocations in both build modes, with the
divergence mechanism diagnosed, or the churn-invariance contract is narrowed
with the bound recorded

## Gap

Severity H (audit-found 2026-07-11). Reproducer: loop
`go test -tags dst [-race] -count=1 -run TestExploreForeignGCWorkloadInsensitive
testing/simulation` — measured 13/60 failures under -race, 8/60 non-race.
Three signatures: "DST-L2-2 violation: schedule prefix diverged on replay"
(from the FOREIGN-FREE leg as well as the churned leg); "replay diverged from
the recorded prefix (decision or enabled set changed at a followed step)";
and a trailing extra decision in the churned trace. The same signature
reproduces in runtime's TestDSTExploreTimerHB (~1-in-4 under machine load:
the DSTExploreTimerHB prog dies with the DST-L2-2 replay-divergence panic) —
a second victim with the same signature (the diagnosis must account for
both). Mechanism hypothesis
(unconfirmed): sticky membership fixed foreign CLASSIFICATION, but an
assist-parked simulation goroutine's CANDIDACY — its presence in the enabled
set at a decision — still tracks physical GC/assist timing, which drifts
across episodes in one process; a prefix recorded under one assist timing
replays under another and names a not-enabled goroutine. Contradicts the
unconditional byte-identical-in-both-build-modes contract in exploration.md
and design.md; the foreign-free-leg failures are outside the scope of the
recorded sim-idle-window divergence.

## Required outcome

The divergence is diagnosed at its mechanism and the test made stable, or the
churn-invariance contract in exploration.md/design.md is narrowed to what
holds, with the flake's bound recorded and the test's assertion matched to
the narrowed contract.
