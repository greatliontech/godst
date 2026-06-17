# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number, a condition, or "pending feature" for planned roadmap work). The
chunk-start gate (sub-chunk `N.1`) scans this index for entries resolving to the
current chunk; the close-out gate promotes any load-bearing rationale inline and
deletes the resolved entry.

## Open

- [net-partition-refuse-connect](net-partition-refuse-connect.md) — `Lands:` when
  peer-down connect semantics are needed (the next network-fault increment). The
  partition fault implements the blackhole connect mode; the settled spec also
  allows a selectable **refuse** (`ECONNREFUSED`) mode, deferred.
- [net-partition-directional](net-partition-directional.md) — `Lands:` when
  asymmetric-partition scenarios are needed. The partition fault implements
  **symmetric** cuts; the spec also allows **one-directional** partitions, deferred.

The remaining planned roadmap work (the rest of fault orchestration) is tracked in
the design.md Roadmap, not here — it gets an issue doc when it is scoped into chunks.
