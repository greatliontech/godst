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
| [caller-position guards run-start TOCTOU](./dst-sim-fault-guard-runstart-toctou.md) | L | activation edge closed for both guards, or window confirmed accepted |
