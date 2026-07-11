# PID exhaustion leaks a failed process identity onto the caller

Lands: when Process allocates and validates its pid before publishing node or
pid stamps, or installs rollback before any panic

## Gap

Severity M. `Process` calls `dstSetNode` before `dstAllocPid`. If pid allocation
panics at the finite-field boundary, the restoration defers have not yet been
installed and the caller remains attributed to a process whose body never
started.

## Required outcome

PID exhaustion is fail-loud and state-neutral. A run rooted at `MaxInt32`
recovers the first Process allocation panic and proves root node, pid,
hostname, filesystem, and victim registries are unchanged.
