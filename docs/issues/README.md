# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number of the active plan, a self-contained condition, or "pending
feature" for planned roadmap work). The chunk-start gate (sub-chunk `N.1`) scans
this index for entries resolving to the current chunk; the close-out gate
promotes any load-bearing rationale inline and deletes the resolved entry.

## Open

| Issue | Severity | Lands |
|---|---|---|
| [foreign Process exit teardown can span a run activation](./dst-sim-process-exit-teardown-spans-activation.md) | M | teardown cannot execute into an undeclared-in run, or acceptance recorded |
| [caller-gate per-API hold extent has no killing tests](./dst-sim-guard-hold-extent-unpinned.md) | M | representative site-local release regressions each fail a test, or convention recorded |
| [gate reader killed while parked strands deactivation](./dst-sim-guarded-reader-killed-while-parked.md) | M | release precedes kill-exposed parks, or the kill path drains stranded readers |
| [concurrent same-name Process starts can acquire two hosts](./dst-process-cross-host-admission-race.md) | H | different-host validation and registration are atomic |
| [Crash refusal mutates run-main process state](./dst-crash-main-refusal-mutates-state.md) | H | crash preflight is state-neutral for every victim |
| [host crash misses a nested root-process goroutine](./dst-host-crash-misses-nested-root-goroutine.md) | H | host ancestry makes every machine thread killable |
| [MIPS Syscall9 bypasses the fence](./dst-mips-syscall9-bypasses-fence.md) | H | chunk 61, with qemu-user available for the runtime witness |
| [MIPS clock_gettime64 lacks a runtime witness](./dst-mips-clock-gettime64-runtime-unverified.md) | L | chunk 61, with qemu-user available for the runtime witness |
| [Host declaration failure is not atomic](./dst-host-declaration-failure-not-atomic.md) | M | failed Host declarations publish no partial state |
| [PID exhaustion leaks a process stamp](./dst-process-pid-exhaustion-leaks-stamp.md) | M | failed Process declarations restore caller identity |
| [process exit publishes dead pid too early](./dst-process-exit-publishes-dead-too-early.md) | M | exit liveness follows thread and resource teardown coherently |
| [loong64 Fstat exposes page-cache descriptors](./dst-loong64-fstat-exposes-pagecache-fd.md) | M | chunk 62, with qemu-user available for the runtime witness |
| [Host NumCPU wraps through int32](./dst-host-numcpu-wraps-int32.md) | L | accepted NumCPU values are exact or fail loudly |
