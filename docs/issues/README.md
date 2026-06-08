# Issue docs

Tracked follow-ups deferred out of a chunk. Each entry carries a `Lands:` trigger
(a chunk number or a condition). The chunk-start gate (sub-chunk `N.1`) scans this
index for entries resolving to the current chunk; the close-out gate promotes any
load-bearing rationale inline and deletes the resolved entry.

| Issue | Lands | Summary |
|-------|-------|---------|
| [dst-net-interfaces-virtualization](dst-net-interfaces-virtualization.md) | when the virtualized-network subsystem is designed (Seq 2–4) | net.Interfaces/InterfaceAddrs are not virtualized under DST, so a SUT enumerating interfaces sees the real host's MACs/IPs. Deferred (not built) as a foreclosure: the correct shape is per-node interfaces sourced from the not-yet-designed virtualized network, so a global fixed stub now would be torn out later. The other identity knobs (pid/ppid/hostname/uid/gid/NumCPU/user/crypto-rand) landed. |
| [dst-percycle-gc-discovery-determinism](dst-percycle-gc-discovery-determinism.md) | when a SUT demonstrably needs deterministic per-cycle GC-discovery timing (under -race or in general) | Per-cycle GC/finalizer-discovery timing (which cycle discovers a given object) is deliberately out-of-contract — set-level (numGC + total set) is the guarantee and is -race-robust. The byte-based span-granular trigger makes the per-cycle split move ±1 span under -race redzones or a change in binary composition. Elevating per-cycle to deterministic needs a race-invariant logical-allocation trigger (a real GC-trigger redesign; the runtime tracks only the physical live set) for a benefit no SUT has demonstrated. Tracks that optional work + root cause. |
