# DST Level-2 Race Failure Force Replay

**Lands:** when `Explore` exposes replay for race failures discovered before access-force convergence.

## Fault

Shared-address filtering may discover a data race in a pass that later promotes an inline access to a
forced replay yield. TSan reports are process-global and deduplicated by signature, so the converged pass
may not increment `RaceErrors` for the same race again.

`Explore` therefore carries race failures from the first pass that observed them. That preserves D5 race
visibility, but the returned `Failure.Schedule` does not include the internal access-force set active in
that pass. If the race was first seen after at least one force was installed, replaying only the schedule
prefix may not reproduce the exact internal interleaving.

## Required Shape

Race-failure replay needs to include enough promotion state to reproduce the observing pass, or the race
oracle needs a way to report the same race again after force convergence. Assertion failures are not
carried across force-set growth; only TSan races have this dedup constraint.

## Validation

Add an auto-instrumented SUT whose only failure signal is a data race first observed after a replay
promotion, then verify the returned failure is replayable by the public reproduction path. A mutation that
drops the force metadata or fails to re-report the race after convergence must fail.
