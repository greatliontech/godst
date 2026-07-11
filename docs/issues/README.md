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
| [in-flight allowlisted read/write can straddle host-fd creation](./dst-fd-inflight-io-straddles-creation.md) | M | non-close dispatch atomic w.r.t. host-fd creation, or window proven empty |
| [lazy-fire timestamp mixes regimes on drifted hosts](./dst-clock-lazy-fire-timestamp-drift.md) | L | delivered-value contract for rate≠1 lazily-fired timers stated in faults.md |
| [IP-less explicit LocalAddr collapses to a concrete-IP bind](./dst-net-wildcard-localaddr-bind-collapse.md) | L | wildcard bind conflict modeled, or the collapse recorded in design.md |
| [foreign Process exit teardown can span a run activation](./dst-sim-process-exit-teardown-spans-activation.md) | M | teardown cannot execute into an undeclared-in run, or acceptance recorded |
| [foreign-GC-workload invariance test flakes ~20%](./dst-explore-gc-workload-churn-flake.md) | H | test stable in both modes with mechanism diagnosed, or contract narrowed with bound recorded |
| [explore recording gates key on bubble-ness, not membership](./dst-explore-recorder-gates-not-membership-keyed.md) | H | recorders keyed on sim membership, or per-gate unreachability recorded |
| [foreign SetGCPercent mid-run silently perturbs the trigger](./dst-gc-foreign-setgcpercent-perturbs-trigger.md) | M | mutation refused or latched out of the trigger, with a test arm |
| [AllThreadsSyscall bypasses the interception boundary](./dst-syscall-allthreads-bypasses-fence.md) | M | fenced at entry, or recorded as an accepted bypass at the boundary spec |
| [caller-gate per-API hold extent has no killing tests](./dst-sim-guard-hold-extent-unpinned.md) | M | representative site-local release regressions each fail a test, or convention recorded |
| [gate reader killed while parked strands deactivation](./dst-sim-guarded-reader-killed-while-parked.md) | M | release precedes kill-exposed parks, or the kill path drains stranded readers |
| [forced-GC entry funnels pinned only structurally](./dst-gc-forced-entry-funnels-unpinned.md) | L | mid-run foreign arms for both funneled entries, or funnel reliance recorded |
| [O_DSYNC conflates to full O_SYNC](./dst-fs-odsync-conflates-osync.md) | L | data-only commit wired, or conflation recorded |
| [pooled cancellation's _defer leg has no killing test](./dst-gc-pooled-defer-leg-unpinned.md) | L | heap-defer shape pins the arm, or untested status recorded |
| [GC counter gates key on the live bubble field](./dst-gc-counter-gates-live-bubble-field.md) | L | gates keyed on the sticky bit, or per-window unreachability recorded |
