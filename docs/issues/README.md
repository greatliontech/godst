# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number of the active plan, a self-contained condition, or "pending
feature" for planned roadmap work). The chunk-start gate (sub-chunk `N.1`) scans
this index for entries resolving to the current chunk; the close-out gate
promotes any load-bearing rationale inline and deletes the resolved entry.

## Open

| Issue | Severity | Lands |
|---|---|---|
| [-race membership-gate doors lack reaching test arms](./dst-explore-race-door-reaching-arms.md) | L | the two -race membership mutants each have a killing test, or unreachability recorded |
| [foreign work in a sim-idle window diverges replay](./dst-explore-foreign-idle-window-divergence.md) | M | idle-window composition replays deterministically, or diagnosed and recorded |
| [lazy-fire timestamp mixes regimes on drifted hosts](./dst-clock-lazy-fire-timestamp-drift.md) | L | delivered-value contract for rate≠1 lazily-fired timers stated in faults.md |
| [IP-less explicit LocalAddr collapses to a concrete-IP bind](./dst-net-wildcard-localaddr-bind-collapse.md) | L | wildcard bind conflict modeled, or the collapse recorded in design.md |
| [foreign Process exit teardown can span a run activation](./dst-sim-process-exit-teardown-spans-activation.md) | M | teardown cannot execute into an undeclared-in run, or acceptance recorded |
| [explore recording gates key on bubble-ness, not membership](./dst-explore-recorder-gates-not-membership-keyed.md) | H | recorders keyed on sim membership, or per-gate unreachability recorded |
| [same-seed broadcast scheduling can diverge across runs](./dst-schedule-broadcast-replay-flake.md) | H | random broadcast scheduling replays identically under focused and full-suite repetition |
| [same-seed scheduler probes diverge under host contention](./dst-scheduler-load-replay-flakes.md) | H | foreign-bubble, PCT, exploration-sweep, and network-reset-order probes replay under contention |
| [caller-gate per-API hold extent has no killing tests](./dst-sim-guard-hold-extent-unpinned.md) | M | representative site-local release regressions each fail a test, or convention recorded |
| [gate reader killed while parked strands deactivation](./dst-sim-guarded-reader-killed-while-parked.md) | M | release precedes kill-exposed parks, or the kill path drains stranded readers |
| [O_DSYNC conflates to full O_SYNC](./dst-fs-odsync-conflates-osync.md) | L | data-only commit wired, or conflation recorded |
| [mapping-fault process death skips resource teardown](./dst-mapping-fault-skips-resource-teardown.md) | H | mapping faults route through complete process teardown |
| [graceful EOF can strand a concurrent reader](./dst-net-concurrent-eof-strands-reader.md) | H | persistent EOF wakes every eligible reader |
| [SYN-ACK traversal ignores cancellation and teardown](./dst-net-synack-ignores-cancel-and-teardown.md) | H | full handshake observes context and endpoint death |
| [accept handoff precedes connection ownership](./dst-net-accept-handoff-precedes-ownership.md) | H | endpoints are lifecycle-owned before handoff is observable |
| [concurrent same-name Process starts can acquire two hosts](./dst-process-cross-host-admission-race.md) | H | different-host validation and registration are atomic |
| [Crash refusal mutates run-main process state](./dst-crash-main-refusal-mutates-state.md) | H | crash preflight is state-neutral for every victim |
| [host crash misses a nested root-process goroutine](./dst-host-crash-misses-nested-root-goroutine.md) | H | host ancestry makes every machine thread killable |
| [MIPS Syscall9 bypasses the fence](./dst-mips-syscall9-bypasses-fence.md) | H | chunk 61, with qemu-user available for the runtime witness |
| [MIPS clock_gettime64 lacks a runtime witness](./dst-mips-clock-gettime64-runtime-unverified.md) | L | chunk 61, with qemu-user available for the runtime witness |
| [fake-timer rollover can erase a registration](./dst-clock-fake-timer-roll-loses-registration.md) | M | epoch rollover preserves every new-epoch timer |
| [foreign races can be attributed to the SUT](./dst-explore-foreign-races-misattributed.md) | M | reported races are simulation-attributed or explicitly foreign |
| [PCT silently clamps depths above sixteen](./dst-pct-depth-silently-clamped.md) | M | public PCT validation matches runtime capacity |
| [network base delays scale with host drift](./dst-net-base-delay-scaled-by-host-drift.md) | M | all network waits consume universe base time |
| [FIN bypasses link delay](./dst-net-fin-bypasses-link-delay.md) | M | graceful close pays link latency and jitter |
| [LocalAddr lacks a complete bind lifecycle](./dst-net-local-bind-lifecycle-incomplete.md) | M | explicit binds validate, reserve, and release atomically |
| [ephemeral port allocation couples independent hosts](./dst-net-port-allocator-cross-host-coupling.md) | M | allocator state is host-scoped |
| [blackhole cannot override an existing refuse cut](./dst-net-blackhole-cannot-override-refuse.md) | M | overlapping cuts preserve blackhole dominance |
| [network handles cross run epochs](./dst-net-handles-cross-run-epochs.md) | M | connections and listeners reject stale epochs |
| [process restart inherits cwd](./dst-process-restart-inherits-cwd.md) | M | process teardown resets cwd |
| [process restart inherits environment](./dst-process-restart-inherits-environment.md) | M | process teardown resets environment state |
| [crash aliases double-count disk capacity](./dst-fs-crash-alias-double-counts-capacity.md) | M | resident bytes count unique nodes |
| [crash directory aliases permit a cycle](./dst-fs-crash-directory-alias-allows-cycle.md) | M | rename containment is node-based and acyclic |
| [Root handles are not owned by teardown](./dst-root-not-owned-by-process-or-host.md) | M | Root participates in process and host teardown |
| [Host declaration failure is not atomic](./dst-host-declaration-failure-not-atomic.md) | M | failed Host declarations publish no partial state |
| [PID exhaustion leaks a process stamp](./dst-process-pid-exhaustion-leaks-stamp.md) | M | failed Process declarations restore caller identity |
| [process exit publishes dead pid too early](./dst-process-exit-publishes-dead-too-early.md) | M | exit liveness follows thread and resource teardown coherently |
| [environment dispatch straddles run edges](./dst-env-dispatch-straddles-run-edge.md) | M | environment world selection is atomic with run edges |
| [loong64 Fstat exposes page-cache descriptors](./dst-loong64-fstat-exposes-pagecache-fd.md) | M | chunk 62, with qemu-user available for the runtime witness |
| [buffered direct handoff omits HB events](./dst-explore-buffered-direct-handoff-misses-hb.md) | L | every buffered slot reuse records the same HB relation |
| [Host NumCPU wraps through int32](./dst-host-numcpu-wraps-int32.md) | L | accepted NumCPU values are exact or fail loudly |
| [network delay arithmetic can overflow](./dst-net-delay-arithmetic-overflows.md) | L | accepted network delay arithmetic cannot wrap |
| [terminal dot checks bypass physical walking](./dst-fs-terminal-dot-bypasses-walk.md) | L | physical path errors precede terminal dot restrictions |
| [creation drops special mode bits](./dst-fs-create-drops-special-mode-bits.md) | L | create and Chmod preserve the same modeled bits |
| [SlowDisk skips Chdir](./dst-disk-latency-skips-chdir.md) | L | Chdir pays one path traversal delay |
| [unknown Explore modes fall back to Exhaustive](./dst-explore-unknown-mode-falls-back.md) | L | unknown modes fail before SUT execution |
