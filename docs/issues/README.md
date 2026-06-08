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
| [dst-percycle-gc-discovery-determinism](dst-percycle-gc-discovery-determinism.md) | when a program demonstrably needs deterministic per-cycle GC-discovery timing (under -race or in general) | Per-cycle GC/finalizer-discovery timing (which cycle discovers a given object) is deliberately out-of-contract — set-level (numGC + total set) is the guarantee and is -race-robust. The byte-based span-granular trigger makes the per-cycle split move ±1 span under -race redzones or a change in binary composition. Elevating per-cycle to deterministic needs a race-invariant logical-allocation trigger (a real GC-trigger redesign; the runtime tracks only the physical live set) for a benefit no program has demonstrated. Tracks that optional work + root cause. |
