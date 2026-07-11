# DST explore: the DPOR truncated-child continuation contract lacks a discriminating test

Lands: when a deterministic branch-dependent-fan-out SUT pins that a truncated
DPOR child does not end the walk (schedules seeded by earlier untruncated runs
still run)

## Gap

Test-surface extension, chunk-28 review (2026-07-11). The DPOR walk continues
past a truncated child (no break; extension skipped), but the pinning test's
root run is itself the truncating run, so reverting the continuation (break
restored) passes the suite — both shapes yield one schedule with Overflow
reported. A discriminating SUT needs an untruncated first run that seeds
multiple backtracks (a concurrent conflicting-access pair) followed by a
branch whose fan-out truncates only on one arm; the chunk-28 reviewer graded
this constructible but default-order-brittle. Coverage loss either way is
reported (Overflow, Exhausted=false), never silent — hence a test-surface gap,
not a correctness hole.

## Required outcome

A deterministic test where the walk's continuation past a truncated child is
observable (schedule count or explored-failure set), or the brittleness is
demonstrated and the contract remains pinned by review.
