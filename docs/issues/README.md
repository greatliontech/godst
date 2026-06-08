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
| [dst-percycle-gc-discovery-determinism](dst-percycle-gc-discovery-determinism.md) | RESOLVED (Phase 2a/2b) — retained for provenance | **Full determinism under `-race` ≡ a race-invariant GC trigger — DONE.** The Phase-1 map confirmed the byte-based GC trigger was the sole remaining within-build `-race`/composition source (after the scheduler fix) plus a second cross-build source (the map hash key). Both fixed: the hash key is derived position-independently (2b), and the GC trigger fires on per-object allocated bytes (`dstHeapAlloc`, 2a) for both the floored and GOGC-scaled regimes — the latter is *not* the feared "logical live set" remainder once instrumentation showed `heapMarked` is deterministic. design.md is authoritative. |
| [dst-memlimit-percycle-determinism](dst-memlimit-percycle-determinism.md) | when a SUT needs per-cycle GC determinism under `Options.MemoryLimit` | **The one regime Phase 2a left span-granular.** The GOGC trigger now fires on per-object `dstHeapAlloc` (per-cycle `-race`-deterministic), but the `Options.MemoryLimit` crossing still fires on physical `heapLive − base`, so a limit-governed cycle is set-level deterministic (`numGC`) but not per-cycle. Fix: a cumulative per-object counter for the limit crossing. Elective. |
