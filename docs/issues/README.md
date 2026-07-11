# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number of the active plan, a self-contained condition, or "pending
feature" for planned roadmap work). The chunk-start gate (sub-chunk `N.1`) scans
this index for entries resolving to the current chunk; the close-out gate
promotes any load-bearing rationale inline and deletes the resolved entry.

## Open

| Issue | Severity | Lands |
|---|---|---|
| [explore recording admits foreign-bubble goroutines](./dst-explore-foreign-bubble-seq-pollution.md) | M | sim-bubble membership enforced at the recording chokepoints |
| [-race explore yield placement is foreign-sensitive](./dst-explore-race-foreign-yield-sensitivity.md) | M | sensitivity diagnosed; removed or recorded as a bounded, reported limit |
| [overdue ticker crossing DriftClock has a phase error](./dst-clock-overdue-ticker-phase.md) | M | backwards remap honors the spec formula's negative remainder, sentinel-safe |
| [in-flight host close can straddle memfd creation](./dst-memfd-inflight-close-toctou.md) | M | creation atomic w.r.t. in-flight dispatch, or the window proven empty |
| [HostFS inspection allocates inodes](./dst-hostfs-inspection-allocates-inodes.md) | L | inspection side-effect-free on simulation state |
| [zero-length O_SYNC write over-commits](./dst-fs-osync-zero-write-overcommits.md) | L | O_SYNC commit fires only for writes that wrote |
| [net TIME_WAIT is unmodeled](./dst-net-time-wait-unmodeled.md) | M | close-time hold modeled, or divergence confirmed kept |
| [Host/Process from non-bubble goroutines mid-run](./dst-sim-topology-apis-from-nonbubble.md) | M | declaration APIs guarded, or caller contract recorded |
| [fault-caller guard run-start TOCTOU](./dst-sim-fault-guard-runstart-toctou.md) | L | activation edge closed, or window confirmed accepted |
| [DPOR truncation-continuation pin missing](./dst-explore-dpor-truncation-continuation-pin.md) | L | deterministic discriminating SUT, or brittleness demonstrated |
