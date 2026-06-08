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
| [dst-percycle-gc-discovery-determinism](dst-percycle-gc-discovery-determinism.md) | pursuing full `-race` determinism, or when a program needs per-cycle GC-discovery determinism | **Full determinism under `-race` ≡ a race-invariant GC trigger.** `-race` only perturbs the *physical* layer; the logical layer and GC set-level hold, so the lone in-contract-relevant thing it breaks is *which* GC cycle discovers an object — because the trigger fires on physical heap bytes (`heapLive`), which redzones shift ±1 span. The scheduler-determinism fix (d8f46779a6) removed the other physical leak. Plan: **map first** (trace-hash localization under `-race`, confirm GC is the sole source), **then a logical-allocation trigger** (tractable for the floored small-live-set case; the GOGC-scaled case is a harder remainder). Elective — no demonstrated SUT need. |
