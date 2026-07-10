# DST audit: an always-runnable outside goroutine livelocks the run with no diagnostic

Lands: when foreign-goroutine scheduling cannot starve the bubble, or the starvation is diagnosed

## Gap

Severity M (full-surface audit, 2026-07-10; reproduced). `dstFindRunnable`
schedules any foreign/system goroutine (`bubble == nil || bubble != dstSimBubble`,
via `firstSystemG`, `src/runtime/proc.go:7904, 7980` and
`src/runtime/dst_explore.go:1160-1162`) ahead of the seeded bubble selection on
every decision. The in-code soundness claim holds only for runtime-infrastructure
goroutines; a user harness goroutine started before `simulation.Run` that yields
but never blocks (`for { runtime.Gosched() }`, which lands on the global runq the
seam enumerates) is re-selected forever and the bubble never runs. Reproduced:
identical run completes without the spinner and hangs with it (5s, no output, no
diagnostic — bubble goroutines never chosen, so synctest's durably-blocked
deadlock detection never fires). The same shape completes under upstream
`testing/synctest` at GOMAXPROCS=1 (FIFO round-robin); the fork introduces the
liveness inversion. Run's doc warns only about in-bubble never-blocking
goroutines (`simulation.go:477-479`).

## Required outcome

A persistently-runnable goroutine outside the bubble either cannot starve the
bubble's progress, or produces a loud deterministic diagnostic rather than an
undiagnosed hang. Pinned by a test with a pre-run Gosched spinner asserting the
run still completes or fails loudly.
