# DST Explore child-goroutine panic failure reporting

**Lands:** before `dst-disk`

## Source

Fresh-eyes review of sequence item 4 in `docs/issues/dst-audit-hardening.md`.

## Finding

`simulation.Explore` reports panics from the top-level SUT callback as `Failure.Panic`, but a panic in a goroutine spawned by the SUT still unwinds through the runtime instead of returning replayable failure metadata.

## Failure Mode

`simulation.Explore(seed, mode, func() bool { go func() { panic("boom") }(); return false })` can crash the process rather than returning a `Failure` with the schedule that let the child panic run. A `recover` in the Explore driver cannot catch it because Go panic recovery is goroutine-local.

## Required Shape

Add a safe panic-reporting path for SUT-created bubble goroutines, likely by wrapping or isolating bubble goroutine entry in the runtime/synctest layer, then return a `Failure` with replay metadata for the panicking schedule.
