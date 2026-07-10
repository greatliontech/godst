# DST audit: a rejected Run mutates the active run's crash-tear policy before its guard panics

Lands: chunk 26 of docs/plans/dst-audit-fixes.md

## Gap

Severity M (full-surface audit, 2026-07-10; reproduced). `RunWith`/`TestWith`
call `runOptions` — whose first act is `setCrashTear(opts.CrashTear)`
(`src/testing/simulation/simulation.go:542`) — before `enterSimulation`
(`simulation.go:506-524`, `:616`), so the process-global `os.dstCrashTear` is
flipped even when the call is then rejected (nested Run, concurrent Run, FIPS,
TestWith-during-cleanup). Reproduced: an outer `RunWith(seed, Options{CrashTear:
true}, …)` whose body attempts a nested `RunWith(…, Options{CrashTear: false},
…)` and recovers the documented panic; a later `CrashHost` restores the pure
durable image on every seed (dirtyBytesSurvived 0/0/0/0 vs 0/8192/6885/3516
without the nested attempt). The run's declared tear policy is silently ignored
for the rest of the run — a seed sweep that believes it sweeps torn-crash
outcomes sweeps nothing — and Explore records the flipped policy into
`Failure.CrashTear` (`explore.go:328`). `ExploreWith`/`Replay` already order
guard-then-policy correctly (`explore.go:191-194, 214-219`); the comment at
`simulation.go:537-541` claims an ordering the code does not implement.

## Required outcome

A rejected Run/RunWith/Test/TestWith leaves every process-global policy
untouched: options apply only after `enterSimulation` succeeds, matching
Explore/Replay. Pinned by a test that attempts a nested run inside a
CrashTear run and asserts torn outcomes still occur.
