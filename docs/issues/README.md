# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number when an active plan exists, a self-contained condition, or
"pending feature" for planned roadmap work). At close-out, lasting rationale is
promoted into a kept-current artifact and the resolved entry is deleted.

## Open

- **testlog buffer lock schedule coupling** — Lands: when the
  determinism sweep gains a testlog-contention leg (host goroutines
  hammering os.Open/Getenv while bubble goroutines run). The
  -test.testlogfile flush now takes the granted host-I/O path from
  bubble goroutines (no fence crash), but the shared bufio buffer's
  mutex can still park a bubble flush behind a HOST goroutine's
  wall-clock work — the same coupling the -v printer's lock-free
  bubble path was built to avoid; the testlog needs that treatment.
- **process-identity divergence modeling** — Lands: when a client needs
  pid REUSE (same pid, new start-time) or pid-namespace divergence
  constructible in-simulation. Pids are allocated monotonically and
  never reused; every process sees namespace pid:[1] — so a client's
  reused-pid and cross-namespace staleness-classification legs cannot
  be exercised end-to-end.
- **page-cache region reclamation (aggregate capacity)** — Lands: when a
  SUT legitimately holds more near-limit files than the mapping region
  carries (~8 at s_maxbytes; fewer under incremental doubling — the
  region is carved, never reclaimed). Growth of an IN-BOUNDS sibling
  then still dies with the mapping-reserve fatal s_maxbytes closed for
  the single-file class. No sound errno exists (a real kernel evicts
  page cache rather than failing), so the honest fix is reclamation or
  an eviction model; until then the loud fatal is the recorded shape.
- **densification-free durable images** — Lands: when a SUT syncs a
  near-limit sparse file. commitDataLocked copies node.data into an
  ordinary slice, so the sync densifies the sparse view and the harness
  dies as an UNTYPED kernel OOM kill — the least loud failure shape,
  same in-spec-input-kills-harness class s_maxbytes fixed for growth.
  Needs a sparse (or COW page-referencing) durable-image
  representation.
