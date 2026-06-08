# Pending feature: in-memory deterministic filesystem under DST

**Lands:** pending feature (the second I/O feature on the Roadmap, after network).
No `Lands:` chunk — planned work.

## Goal

Under `simulation.Run`, virtualize filesystem / disk I/O to a fully **in-memory,
deterministic** filesystem so that *unmodified* file-using Go code is reproducible.
`os.Open`/`Create`/`Read`/`Write`/`Mkdir`/`ReadDir`/`Remove`/`Rename`/`Stat` (and
`*os.File`) operate on an in-process simulated filesystem instead of the host's.

This is the base on which **disk faults** are later layered: latency, `EIO`/
`ENOSPC`, and torn/lost-unsynced-writes on a simulated crash/restart.

## Why it fits DST cleanly

Like the network feature, determinism rides the existing machinery — the
filesystem is a per-bubble in-memory tree, all operations run on the calling
goroutine under the deterministic schedule, and there is no real I/O to introduce
nondeterminism. Directory iteration order is made deterministic (sorted, or a
seeded order). `fsync` is a no-op against memory until crash-faults model
lost/torn unsynced writes.

## Seam

Intercept at the *exported* `os` file surface (`os.OpenFile` and friends, and the
`*os.File` methods), gated on `dstActive()`. As with the network, the program does
not exercise real fds under DST, so virtualizing the userspace surface (not the
`syscall`/poller layer) is the right altitude.

## Scope / increments

1. **Core file ops** — open/create/read/write/seek/close on an in-memory file
   tree; `Mkdir`/`Remove`/`Rename`/`Stat`. Deterministic `ReadDir` order.
2. **Metadata / modes** — permissions, mod times (from the synctest clock),
   symlinks if needed.
3. (later) **crash semantics** — a `Sync`/durability model so crash-faults can
   drop or tear unsynced writes deterministically; **disk faults** as policies.

## Open questions to settle when it lands

- Whether to expose this through `os` directly or via an `fs.FS`-shaped seam.
- The durability/crash model (what survives a simulated crash) — this is the part
  that matters most for storage-engine SUTs and should be designed with the
  fault feature.

## Contract note

Like the network feature, this moves real file I/O from "out of scope" into the
fork. Record the model and caveats in design.md when it lands.
