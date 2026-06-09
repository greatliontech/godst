# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number, a condition, or "pending feature" for planned roadmap work). The
chunk-start gate (sub-chunk `N.1`) scans this index for entries resolving to the
current chunk; the close-out gate promotes any load-bearing rationale inline and
deletes the resolved entry.

## Pending features

The I/O and fault features on the design.md Roadmap — planned work that brings real
I/O into the bubble and then layers fault injection on top.

| Feature | Order | Summary |
|---------|-------|---------|
| [dst-disk](dst-disk.md) | next | In-memory deterministic filesystem (os file ops, deterministic ReadDir order) so unmodified file-using code is reproducible. The base for disk faults (latency/EIO/ENOSPC/torn writes). |
| [dst-io](dst-io.md) | after disk | Deterministic file/pipe/stdio I/O for whatever network and disk don't cover (os.Pipe, std streams). |

## Deferrals

| Issue | Lands | Summary |
|-------|-------|---------|
| [dst-l2-shared-address-filtering](dst-l2-shared-address-filtering.md) | when shared-address filtering proper, the explosion measurement, and the auto-instrumented equivalence acceptance land | DST Level-2 shared-address filtering (explosion control) for the dst-race auto-instrumentation. Runtime per-bubble access log + commit-order log-based source-DPOR foundation is green (`TestDSTExploreSweep` mismatches=0). Filtering itself (yield-only-on-conflict-not-HB-ordered), the explosion measurement, and the auto-instrumented equivalence acceptance are not yet implemented. |
| [dst-l2-sync-acquisition-coverage](dst-l2-sync-acquisition-coverage.md) | when extending runtime sync-acquisition auto-hooks beyond `sync.Mutex.Lock` and blocking channel send/recv, or when an unmodified SUT depends on one of those acquisition orders | Track the remaining Level-2 synchronization-acquisition surfaces (`RWMutex.RLock`, non-blocking select send/recv, `Mutex.TryLock`, `sync.Once`) whose order can affect an unmodified SUT but is not yet auto-recorded as a `dstSyncAcquire` transition. |
| [dst-l2-timer-hb-edge](dst-l2-timer-hb-edge.md) | when timer-based interleavings are explored | Re-validate Level-2 DPOR's happens-before pruning against timer-fire `goready` edges: a `time.Sleep` wake records an edge that is "spurious" in memory-model terms and could over-order a timer-gated race (a silent completeness risk). Defensible under DST's fake clock and no failing case found, but unverified — add a timer-gated completeness SUT, drop the timer edge from the HB set if it over-orders. |
