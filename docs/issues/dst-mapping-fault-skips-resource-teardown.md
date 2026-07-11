# Mapping-fault process death skips resource teardown

Lands: when a simulated mapping fault routes through complete process death,
including logical-process bookkeeping and every owned resource

## Gap

Severity H. `dstMappingFault` calls `dstCrashProcessPid` and parks the faulting
thread, but never runs the process teardown used by `Crash` and normal exit.
The pid becomes dead while files, virtual fds, flocks, mappings, connections,
listeners, and the active-process registration can remain live. A same-name
restart can then inherit or be blocked by resources of a process the model says
died.

## Required outcome

Mapping-fault death has the same complete, ordered lifecycle as process crash:
threads stop, liveness changes coherently, and resources close exactly once.
A subprocess test faults while owning each resource class and proves a restart
can reacquire them.
