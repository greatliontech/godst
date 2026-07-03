# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number, a condition, or "pending feature" for planned roadmap work). The
chunk-start gate (sub-chunk `N.1`) scans this index for entries resolving to the
current chunk; the close-out gate promotes any load-bearing rationale inline and
deletes the resolved entry.

## Open

- [clock-drift-dynamic](clock-drift-dynamic.md) — `Lands:` seeded/fault-RNG drift with
  fault orchestration. The declared constant-rate (`Drift`) and mid-run (`DriftClock`)
  drift legs landed; only the spec's **fault-RNG-drawn** (seeded `BoundedDrift`) leg is
  deferred.

The remaining planned roadmap work (the rest of fault orchestration) is tracked in
the design.md Roadmap, not here — it gets an issue doc when it is scoped into chunks.
