# Plan: dst-17 — per-link network policy, inline fault points, and failure-only causal timeline

Spec: docs/dst/faults.md ("Network faults", "Inline fault-point activation (`Sometimes`)",
"Project invariants") and docs/dst/design.md ("Deterministic process identity",
"In-memory deterministic network", "Failure-only causal event timeline and active fault-site
diagnostics").

- [x] 1. Spec, documentation, plan, and deferred-work tracking: amend `docs/dst/faults.md` and
  `docs/dst/design.md` with spec-tier contracts for `LinkConfig` (`Options.Network.CrossHost`,
  `ConfigureLink`, `ConfigureLinkOneWay`, `UnconfigureLink`), `ResetLinkConns` /
  `ResetProcessConns`, `Options.PIDMax`, `Sometimes`, and the off-heap failure-only causal event
  timeline; document virtual-time & quiescence driver mechanics (`time.Sleep` vs `synctest.Wait`,
  `0ns` CPU execution rule, `time`/`sync`/`rand` surface) and fix Pattern 2 in `docs/README.md`;
  record user-authorized zero-retrofit future-work deferrals in `docs/issues/`.
- [ ] 2. Per-link network policy & public API consolidation (`LinkConfig`,
  `NetworkConfig.CrossHost`, `ConfigureLink`, `ConfigureLinkOneWay`, `UnconfigureLink`,
  `ResetLinkConns`, `ResetProcessConns`, `Options.PIDMax`): collapse `NetworkConfig`'s flat
  cross-host fields onto `CrossHost LinkConfig` with unified fail-loud negative/overflow
  validation; rename `ResetLink`/`ResetProcess` to `ResetLinkConns`/`ResetProcessConns` and
  `Options.PidMax` to `Options.PIDMax`; implement symmetric and directional `(Latency, Jitter,
  Bandwidth)` overrides across `testing/simulation`, `runtime`, and `net` (`dstDial` directional
  `SYN`/`SYN-ACK` lookup, dynamic outbound wire lookup on `Write`/`CloseWrite`/graceful `Close`
  preserving `DST-NET-FIFO` and `max(now, linkFreeAt)` serialization for both unthrottled segments
  and FIN, `coarseClose` endpoint tracking, `HealLink`/`CrashHost` survival, `api/go1-godst.txt`
  parity); promote spec clauses in `docs/dst/faults.md`, `docs/dst/design.md`, and
  `docs/dst/gc.md` to enforced and update `docs/README.md`.
- [ ] 3. Inline SUT fault-point activation (`Sometimes(name string, prob float64) bool`) and
  active-site diagnostics: implement uniform `!dst`/`dst` argument validation, two-level
  `(seed, name)` stateless ~50% gate, stream-isolated DPOR-commutative per-goroutine `dstBugRand`
  fork-tree on `dstG` (two-root seeded on `bubble.root`/`bubble.gcDrain` and `bubble.main`),
  off-heap `256`-slot active-site ledger (`sysAlloc`, `dstDiagLock`, deduplicated on `(nameHash,
  nameLen, namePrefix[:min(nameLen, 128)])`) rendered on `Test`/`TestWith`/`WorldTest`/`ExploreTest`
  failure, and `api/go1-godst.txt` parity; promote spec clauses in `docs/dst/faults.md` and
  `docs/dst/design.md` to enforced and document (including the consumer `//go:build dst` / `!dst`
  wrapper pattern) in `docs/README.md`.
- [ ] 4. Failure-only causal event timeline and plan close-out: implement off-heap `256`-slot
  `dstCausalEvent` circular ring buffer (`sysAlloc`, `dstDiagLock`, zero Go heap allocations, zero
  in-run formatting, zero TSan/DPOR sync edges) recording topology/lifecycle, fault/policy calls,
  TCP lifecycle/timeout/reset transitions, and filesystem `EIO`/`fsyncgate`/`ENOSPC`/`CorruptFile`
  fault hits; render chronologically on failure in `testing/simulation`; promote spec clauses in
  `docs/dst/faults.md` and `docs/dst/design.md` to enforced, update `docs/README.md`, delete this
  completed plan, and pass the full `godst` verification matrix for `go1.27.4-dst.17`.
