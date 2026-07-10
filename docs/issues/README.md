# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number, a condition, or "pending feature" for planned roadmap work). The
chunk-start gate (sub-chunk `N.1`) scans this index for entries resolving to the
current chunk; the close-out gate promotes any load-bearing rationale inline and
deletes the resolved entry.

## Open

Full-surface DST audit (2026-07-10), severity-ordered. Every entry is folded
into the active plan, `docs/plans/dst-audit-fixes.md`; `Lands:` names its
chunk(s) there.

| Issue | Severity | Lands |
|---|---|---|
| [host crash lets peer drain in-flight bytes](./dst-audit-net-crash-drain.md) | H | chunk 18 |
| [rejected Run mutates active crash-tear policy](./dst-audit-sim-crash-tear-guard.md) | M | chunk 26 |
| [fault APIs from a non-bubble goroutine misbehave](./dst-audit-sim-fault-from-nonbubble.md) | M | chunk 27 |
| [net divergences from kernel shape](./dst-audit-net-kernel-shape.md) | M | chunks 19–23 |
| [spec-hygiene defects](./dst-audit-spec-hygiene.md) | M/L | chunk 30 |
| [low-severity fidelity/hygiene cluster](./dst-audit-low-fidelity-cluster.md) | L | chunks 4, 9, 11–17, 24, 25, 28, 29 |

Found during plan execution:

| Issue | Severity | Lands |
|---|---|---|
| [explore recording admits foreign-bubble goroutines](./dst-explore-foreign-bubble-seq-pollution.md) | M | sim-bubble membership enforced at the recording chokepoints |
| [-race explore yield placement is foreign-sensitive](./dst-explore-race-foreign-yield-sensitivity.md) | M | sensitivity diagnosed; removed or recorded as a bounded, reported limit |
| [warm-process runs shift late GC discovery](./dst-gc-warm-process-discovery-tail.md) | M | tail discovery repeatable in-process, or the warm-process bound recorded |
| [sizespecializedmalloc experiment bypasses the GC trigger](./dst-gc-sizespecialized-experiment.md) | M | combo supported or refused at a testable level |
| [foreign runtime.GC() shifts the trigger stream](./dst-gc-foreign-runtime-gc.md) | M | foreign GC cannot perturb the bubble's accounting, or fails loudly |
| [overdue ticker crossing DriftClock has a phase error](./dst-clock-overdue-ticker-phase.md) | M | backwards remap honors the spec formula's negative remainder, sentinel-safe |
| [in-flight host close can straddle memfd creation](./dst-memfd-inflight-close-toctou.md) | M | creation atomic w.r.t. in-flight dispatch, or the window proven empty |
| [HostFS inspection allocates inodes](./dst-hostfs-inspection-allocates-inodes.md) | L | inspection side-effect-free on simulation state |
| [zero-length O_SYNC write over-commits](./dst-fs-osync-zero-write-overcommits.md) | L | O_SYNC commit fires only for writes that wrote |
