# DST sim: a foreign Process's exit teardown can span a run activation

Lands: when Process's exit-teardown leg cannot execute into a run it was not
declared in (e.g. teardown latches the runActive observation made under the
declaration gate, or keys on a run epoch), or the window is confirmed
accepted and recorded beside the caller-gate paragraph in faults.md

## Gap

Severity M (review-found 2026-07-11, adjacent — pre-existing; the caller-gate
chunk verified it neither introduced nor widened it). Process's exit-teardown
defer and park-forever defer run at f's RETURN, outside the caller gate, and
load `runActive` at that instant. A foreign goroutine's pre-run
`Process("db", f)` whose f spans a run activation: the declaration completed
against pre-run state (gated, sound), but when f returns mid-run the teardown
defer loads the NEW run's `runActive=true` and executes
`dstCrashProcessPid(staleSimPid)` — pid counters are re-based per run, so a
stale pid can collide with a live in-run pid — plus proc-keyed teardown
(`dstProcessTeardown`, close-conns/close-listeners ops) on the interned proc
id, which the node registry persists across runs: a same-named process in the
new run has its connections closed by the dead foreign invocation. Reachable
only by user code racing a foreign Process against Run, but the failure is
silent state teardown inside a seeded run — the silent-nondeterminism class
every guard in this file exists to kill.

## Required outcome

A foreign Process invocation that began before a run cannot mutate that run's
state at f-return — its teardown either acts on the pre-run world it was
declared in or is refused loudly — or the window is confirmed accepted, with
the acceptance recorded beside faults.md's caller-gate paragraph (whose
"declaration mutations are gated, f runs ungated" statement must then also
name this teardown leg).
