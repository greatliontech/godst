# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number, a condition, or "pending feature" for planned roadmap work). The
chunk-start gate (sub-chunk `N.1`) scans this index for entries resolving to the
current chunk; the close-out gate promotes any load-bearing rationale inline and
deletes the resolved entry.

## Open issues

| Issue | Lands | Summary |
|-------|-------|---------|
| [dst-sync-stack-shape](dst-sync-stack-shape.md) | when `task test:inert-std` is required to gate green | Untagged trace/pprof stack-shape regression: the DST mutex hook refactor adds one inlined logical frame, shifting the fixed semaphore skip constants; `TestTraceStacks`, `TestBlockProfile`, `TestMutexProfile` fail untagged. |

## Pending features

The I/O and fault features on the design.md Roadmap — planned work that brings real
I/O into the bubble and then layers fault injection on top.

| Feature | Order | Summary |
|---------|-------|---------|
| [dst-disk](dst-disk.md) | next | In-memory deterministic filesystem (os file ops, deterministic ReadDir order) so unmodified file-using code is reproducible. The base for disk faults (latency/EIO/ENOSPC/torn writes). |
| [dst-io](dst-io.md) | after disk | Deterministic file/pipe/stdio I/O for whatever network and disk don't cover (os.Pipe, std streams). |
