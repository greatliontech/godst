# Issue docs

Tracked follow-ups deferred out of a chunk. Each entry carries a `Lands:` trigger
(a chunk number or a condition). The chunk-start gate (sub-chunk `N.1`) scans this
index for entries resolving to the current chunk; the close-out gate promotes any
load-bearing rationale inline and deletes the resolved entry.

| Issue | Lands | Summary |
|-------|-------|---------|
| [dst-net-interfaces-virtualization](dst-net-interfaces-virtualization.md) | when the virtualized-network subsystem is designed (Seq 2–4) | net.Interfaces/InterfaceAddrs are not virtualized under DST, so a SUT enumerating interfaces sees the real host's MACs/IPs. Deferred (not built) as a foreclosure: the correct shape is per-node interfaces sourced from the not-yet-designed virtualized network, so a global fixed stub now would be torn out later. The other identity knobs (pid/ppid/hostname/uid/gid/NumCPU/user/crypto-rand) landed. |
