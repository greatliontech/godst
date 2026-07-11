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
| [in-flight host close can straddle memfd creation](./dst-memfd-inflight-close-toctou.md) | M | creation atomic w.r.t. in-flight dispatch, or the window proven empty |
| [HostFS inspection allocates inodes](./dst-hostfs-inspection-allocates-inodes.md) | L | inspection side-effect-free on simulation state |
| [lazy-fire timestamp mixes regimes on drifted hosts](./dst-clock-lazy-fire-timestamp-drift.md) | L | delivered-value contract for rate≠1 lazily-fired timers stated in faults.md |
| [net TIME_WAIT is unmodeled](./dst-net-time-wait-unmodeled.md) | M | close-time hold modeled, or divergence confirmed kept |
| [Host/Process from non-bubble goroutines mid-run](./dst-sim-topology-apis-from-nonbubble.md) | M | declaration APIs guarded, or caller contract recorded |
| [fault-caller guard run-start TOCTOU](./dst-sim-fault-guard-runstart-toctou.md) | L | activation edge closed, or window confirmed accepted |
