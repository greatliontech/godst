# Call-free spin loop in-bubble: undiagnosed process-wide wedge needs a loud diagnosis

A bubble goroutine executing a preemption-point-free loop (e.g.
`for !flag.Load() {}` — race-free, legal production Go that completes under
async preemption) permanently wedges the entire process under DST: the run
pins P=1, disables async preemption, and gates sysmon's retake, so
`dstFindRunnable` is never re-entered — no bubble goroutine, no foreign
goroutine, no in-process watchdog (including `go test -timeout`) ever runs
again, and the durably-blocked deadlock detector never fires over a RUNNING
goroutine. SIGQUIT produces no traceback; only SIGKILL ends the run.
Probe-verified (spinner + outside-bubble watchdog: watchdog starved, external
`timeout` killed the process). A false-positive-adjacent boundary: a legal
production shape becomes an undiagnosed wall-clock hang.

The boundary itself is now recorded in design.md's stdio/hang-modes paragraph
(the third hang mode). What remains is the DIAGNOSIS: sysmon still runs on
its own M and could detect a non-preemptible in-bubble goroutine exceeding a
wall bound and throw loudly (naming the goroutine and the boundary) without
perturbing seeded schedules on any run that does not wedge. The same
mechanism should consider the sibling wedge: a granted capability WRITE
blocked in the raw syscall whose unblocking consumer is a goroutine in the
same process (probe-verified: `anon_pipe_write` main thread, all peers
futex-parked, test timeout dead, SIGQUIT dead) — design.md's "a blocked write
delays the run in wall time but cannot reorder it" holds only when the
unblocker is out-of-process.

Lands: when an out-of-schedule detector (sysmon-side or equivalent) can
loudly diagnose a wall-bounded non-preemptible/syscall-wedged bubble
goroutine without perturbing any non-wedged run's seeded schedule.
