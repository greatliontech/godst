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
| [dst-l2-range-access-filtering](dst-l2-range-access-filtering.md) | when dst-race range/composite access hooks participate in shared-address filtering | Range/composite race hooks currently give DST only the base address, so overlapping range-vs-field accesses can have different conflict identities and be filtered as independent. |
| [dst-l2-timer-hb-edge](dst-l2-timer-hb-edge.md) | when timer-based interleavings are explored | Re-validate Level-2 DPOR's happens-before pruning against timer-fire `goready` edges: a `time.Sleep` wake records an edge that is "spurious" in memory-model terms and could over-order a timer-gated race (a silent completeness risk). Defensible under DST's fake clock and no failing case found, but unverified — add a timer-gated completeness SUT, drop the timer edge from the HB set if it over-orders. |
