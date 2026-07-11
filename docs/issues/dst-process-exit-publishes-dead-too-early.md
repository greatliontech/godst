# Normal process exit publishes dead pid before threads and resources stop

Lands: when normal exit follows one observable lifecycle order from thread
death through pid/procfs removal and resource close

## Gap

Severity M. The Process exit defer calls `dstSetPidLive(false)` before restoring
identity, acquiring `procTeardownMu`, or marking that invocation's child
goroutines crashed. For the last live invocation it also precedes logical-
process resource teardown. Level-2 preemption or mutex contention can expose
`ESRCH` and missing procfs while the invocation still executes and, on the last
exit, still owns locks, files, and listeners.

## Required outcome

No observer sees a dead pid while a thread of that invocation remains live. On
the last invocation, resource teardown completes in the specified lifecycle;
resources shared with another live same-name invocation remain intact. A
controlled schedule covers both last and non-last exits.
