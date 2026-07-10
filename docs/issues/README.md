# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number, a condition, or "pending feature" for planned roadmap work). The
chunk-start gate (sub-chunk `N.1`) scans this index for entries resolving to the
current chunk; the close-out gate promotes any load-bearing rationale inline and
deletes the resolved entry.

## Open

Full-surface DST audit (2026-07-10), severity-ordered. Each entry's `Lands:` is
the self-contained condition that resolves it.

| Issue | Severity | Lands |
|---|---|---|
| [socketcall arches bypass the syscall fence](./dst-audit-socketcall-fence.md) | H | fence refuses socket-family syscalls on socketcall arches |
| [crypto/rand taints unseeded goroutine subtrees](./dst-audit-crypto-rand-taint.md) | H | entropy gate keys on seeded-tree membership |
| [host crash lets peer drain in-flight bytes](./dst-audit-net-crash-drain.md) | H | crashed host's conns reset at peer without delivering queued bytes |
| [pre-seeded /tmp born unsynced, erased by crash](./dst-audit-fs-tmp-durability.md) | H | mkfs image is part of the durable image |
| [crash restore never advances the durable image](./dst-audit-fs-tear-durable-image.md) | H | restore commits the restored image as durable |
| [rejected Run mutates active crash-tear policy](./dst-audit-sim-crash-tear-guard.md) | M | options apply only after enterSimulation admits the run |
| [fault APIs from a non-bubble goroutine misbehave](./dst-audit-sim-fault-from-nonbubble.md) | M | fault APIs require a bubble-goroutine caller |
| [foreign goroutine livelocks the run](./dst-audit-sched-foreign-livelock.md) | M | foreign scheduling cannot starve the bubble, or is diagnosed |
| [latched GC trigger started by foreign allocation](./dst-audit-gc-foreign-trigger.md) | M | DST trigger started only inside the bubble-allocation gate |
| [refused/zero-length writes still grow the file](./dst-audit-fs-refused-write-grows.md) | M | empty-effective-slice write leaves size/mtime unchanged |
| [leaked *Root across runs defeats the epoch gate](./dst-audit-leaked-root-epoch.md) | M | dstRoot handles carry and check the run epoch |
| [page-cache memfds killable by SUT close](./dst-audit-memfd-fd-space.md) | M | page-cache fds isolated from the SUT-visible fd space |
| [net divergences from kernel shape](./dst-audit-net-kernel-shape.md) | M | wire model matches, or spec records each as a modeled limit |
| [mprotect refuses changes Linux allows](./dst-audit-mprotect-fidelity.md) | M | mprotect tracks fd writability, or spec records the restriction |
| [spec-hygiene defects](./dst-audit-spec-hygiene.md) | M/L | spec docs reconciled with the landed surface |
| [low-severity fidelity/hygiene cluster](./dst-audit-low-fidelity-cluster.md) | L | each divergence corrected or recorded as a modeled limit |
