# DST gmdb crash resource teardown

Lands: 10

## Gap

gmdb's stale recovery depends on process death releasing kernel-owned resources and making peer state reclaimable. DST crash/restart is specified but pending, and resource ownership for virtual fds, locks, mappings, connections, goroutines, and pid liveness needs a single teardown path.

## Required outcome

Each simulated process owns its fds, flocks, mappings, conns, goroutines, and pid liveness. Crash-time teardown permanently deschedules the process goroutines, closes its fds, releases its flocks, marks its pid dead for liveness probes, resets its connections according to the network fault contract, and leaves host filesystem content intact for process crashes.

Shared mmap file contents are not process memory and remain host file state; stale slots and heartbeats become reclaimable through liveness and starttime checks.
