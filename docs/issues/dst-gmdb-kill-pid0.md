# DST gmdb Kill pid-zero liveness

Lands: 7

## Gap

gmdb uses `Kill(pid, 0)` to test whether a peer process is alive. DST currently fences raw `Kill`, and simulated process liveness is not exposed through the syscall surface.

## Required outcome

`Kill(pid, 0)` consults the simulated pid registry. It returns success for live simulated processes and `ESRCH` for dead or unknown simulated processes. It never signals or observes a host process. Non-zero signals remain outside this issue unless a separate signal model is settled.
