# DST Explore deadlock failure reporting

**Lands:** before `dst-disk`

## Source

Sequence item 4 of `docs/issues/dst-audit-hardening.md` split after implementation showed that direct panic recovery and synctest-deadlock recovery have different safety requirements.

## Finding

`simulation.Explore` still lets a `synctest` deadlock panic unwind instead of returning a replayable `Failure`.

## Failure Mode

A schedule where all bubble goroutines are durably blocked is a reachable SUT failure, but returning it as metadata is unsafe unless the runtime can tear down or quarantine the blocked bubble goroutines. A naive outer `recover` around `synctest.Run` leaves those goroutines alive after `Run` cleanup; the post-run cleanup GC hit a marked-free-object fatal in that state.

## Required Shape

Add a safe deadlock-reporting path that either tears down the deadlocked bubble state or runs each schedule in an isolation boundary where leftover blocked goroutines cannot survive into later schedules, then return a `Failure` with replay metadata for the deadlocking schedule.
