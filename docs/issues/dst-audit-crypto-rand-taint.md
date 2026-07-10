# DST audit: crypto/rand hands seed-independent constant bytes to unseeded goroutine subtrees

Lands: when the deterministic-entropy gate keys on membership in the seeded goroutine tree

## Gap

Severity H (full-surface audit, 2026-07-10; reproduced; security-relevant).
`dstReadRandom` (`src/runtime/rand.go:283`) gates deterministic entropy on
`gp.dstrand != 0`, treating zero as "unseeded — use real OS entropy". But
`dstrandUint64` (`src/runtime/rand.go:251`) mutates the caller's `dstrand`
in place, and `newproc1` (`src/runtime/proc.go:5456`) draws from the parent
to seed every child while a run is active. So a goroutine created before
activation (`dstrand == 0`) that spawns any child during a run has its own
`dstrand` bumped to the fixed constant `0x9e3779b97f4a7c15`, and the child
inherits a value derived from the zero root. Both then pass the gate and
receive crypto/rand bytes that are deterministic and identical across seeds
(verified: same output for seeds 1, 7, 42). Reachable in-spec: any background
goroutine coexisting with `simulation.Run` — connection pool, logger,
TestMain worker — that spawns during a run, and all its descendants. Violates
the INV-CRYPTO unseeded-goroutine leg the gate's own comment claims to close.

## Required outcome

Out-of-bubble goroutines receive real OS entropy under all spawn orderings:
the gate predicate is membership in the run-seeded goroutine tree, not
`dstrand != 0` — an unseeded root must not be able to taint itself or its
subtree into the deterministic stream. Pinned by a test that spawns from a
pre-activation goroutine during a run and asserts its crypto/rand output
varies across seeds.
