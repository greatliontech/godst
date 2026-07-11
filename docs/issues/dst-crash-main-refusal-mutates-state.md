# Refusing a crash of the run-main process mutates liveness first

Lands: when Crash preflights every victim before clearing registrations or pid
liveness

## Gap

Severity H. `crashProcess` clears `activeProcs` and calls
`dstCrashProcessPid` before the runtime scanner discovers that the victim owns
the bubble main and panics. Recovering the documented refusal leaves the still-
running process with `Kill(pid, 0) == ESRCH`, no procfs entry, and no active
registration; multi-invocation victims can also be partially killed first.

## Required outcome

A refused crash has no observable side effect. Tests recover inside an inline
Process and verify pid/procfs, active registration, resources, and all same-name
invocations remain live.
