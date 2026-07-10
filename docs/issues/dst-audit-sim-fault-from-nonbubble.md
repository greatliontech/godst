# DST audit: fault APIs from a non-bubble goroutine execute nondeterministically or silently no-op

Lands: chunk 27 of docs/plans/dst-audit-fixes.md

## Gap

Severity M (full-surface audit, 2026-07-10; demonstrated). `crashHost`/
`crashProcess` gate only on `runActive` and victim liveness, never on the caller
being a bubble goroutine (`src/testing/simulation/node.go:477-502, 533-597`);
`dstNetPartitionOp`/`dstDiskFaultOp` are unconditional hook relays
(`src/runtime/dst.go:1913, 1960`). A goroutine started before `Run` that calls
`CrashHost("h")` mid-run executes the crash at an instant determined by OS
wall-clock timing, not the seed — a SUT that tolerates the crash diverges
silently between identical-seed executions. From the same misuse position
`StepClock`/`DriftClock` silently no-op (`gp.bubble == nil` → return true,
`dst.go:461-463, 582-584`) — the "fault that silently tests nothing" the
package's own victim-naming rule panics on. A background fault-injector
goroutine is a plausible user shape; neither behavior is loud or deterministic.

## Required outcome

Fault-injection and clock-fault APIs invoked from outside the run's bubble
during an active run fail loudly and deterministically (as the victim-naming
rule already does), rather than executing at a wall-clock instant or silently
doing nothing. Pinned by tests calling each API from a pre-run goroutine.
