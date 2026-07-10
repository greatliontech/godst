# Plan: DST full-surface audit fixes

Derived from the 2026-07-10 full-surface audit, filed in `docs/issues/`
(index: `docs/issues/README.md`). Spec: `docs/dst/design.md`,
`docs/dst/faults.md`, `docs/dst/gc.md` — authoritative. Chunks ordered
bottom-up by layer and grouped by function; each chunk names the issue doc(s)
it folds. WIP = 1.

Audit reproducers (ephemeral, from the audit session):
`/tmp/claude-1000/-home-nikolas-repos-github-com-thegrumpylion-go/58cb7d98-2163-47bc-b596-6578c6a85bfc/scratchpad/audit-*`
— `audit-build/cryptoprobe` → 1; `audit-sched/{spinprobe,spintest}` → 2;
`audit-syscall/sockprobe` → 5; `audit-pagecache/dstprobe` → 6,
`audit-pagecache/prod_mprotect.go` → 12; `audit-osfs/probe` → 7–9;
`audit-net/{crashdrain,crashdial,smallwrite,bindclash,backlog,closeunread}` →
18–23; `audit-sim/teartest` → 26, `audit-sim/outside` → 27.

## Runtime core (src/runtime)

- [x] 1. crypto/rand entropy gate keys on membership in the run-seeded
  goroutine tree, not `dstrand != 0`; an unseeded root cannot taint itself or
  its subtree into the deterministic stream. Test: spawn from a
  pre-activation goroutine during a run, assert crypto/rand output varies
  across seeds. (dst-audit-crypto-rand-taint)
- [x] 2. an always-runnable foreign goroutine cannot starve the bubble, or the
  starvation produces a loud deterministic diagnostic. Test: pre-run Gosched
  spinner, run completes or fails loudly. (dst-audit-sched-foreign-livelock)
- [x] 3. the DST heap-trigger crossing starts a GC only from within the
  bubble-allocation gate; an inner `checkGCTrigger` reached by a foreign
  allocation cannot start the DST-armed cycle. Test: foreign allocation
  interleaved against a near-threshold bubble heap, cycle boundaries
  seed-stable. (dst-audit-gc-foreign-trigger)
- [x] 4. a periodic fake timer whose `when == bubble.now` exactly at the
  `DriftClock` instant adopts the new rate. (dst-audit-low-fidelity-cluster,
  timer nit)

## Syscall boundary (src/syscall)

- [x] 5. on socketcall architectures (386, s390x) the socket-family wrappers
  consult the fence exactly as the trampolines do: bubble caller gets the
  standard refusal, non-bubble callers fall through. The 386 fence test
  exercises the `syscall.Socket` wrapper path, not only the raw trampoline.
  (dst-audit-socketcall-fence)
- [x] 6. harness page-cache memfds are unreachable from the SUT-visible fd
  number space: reserved range refused at the syscall boundary, or bubble
  `close` of a page-cache fd is EBADF/no-op. Test: close-loop over low fd
  numbers, then a resize and an mmap. (dst-audit-memfd-fd-space)

## Filesystem & durability (src/os)

- [x] 7. the initial tree a run boots with (root, `/tmp`) is durable from
  birth — the mkfs image is part of the durable image. Test: crash test with
  files under `/tmp` and directory fsync on `/tmp`, not `/`.
  (dst-audit-fs-tmp-durability)
- [x] 8. a host-crash restore commits the restored image as the new durable
  image; a second crash with no intervening writes changes nothing. Test:
  double-crash, byte-identical post-crash images, torn and untorn.
  (dst-audit-fs-tear-durable-image)
- [x] 9. a write whose effective slice is empty (zero-length, or fully refused
  by the ENOSPC cap) leaves size, content, resident-byte accounting, and mtime
  unchanged. (dst-audit-fs-refused-write-grows; subsumes the
  failed-writes-bump-mtime nit of dst-audit-low-fidelity-cluster)
- [x] 10. `dstRoot` handles (and files opened through them) carry and check
  the run epoch like `dstFile`: a Root leaked across a run boundary is refused
  deterministically — never a nil deref, never a read of a prior run's tree.
  (dst-audit-leaked-root-epoch)
- [x] 11. fsync-EIO models post-fsyncgate Linux: a faulted fsync drops the
  affected pages from the writeback set, so a retried fsync succeeds without
  the data reaching the durable image. Spec amended accordingly (user decision
  2026-07-10, supersedes the kinder model faults.md chose).
  (dst-audit-low-fidelity-cluster, fsync-EIO item)
- [x] 12. the mapping entry records fd writability; mprotect permits any
  protection the fd's access mode allows, including PROT_NONE — matching
  Linux and the spec's creation-time-only access checks (user decision
  2026-07-10). Also removes the dead `dstMMapEntry.seq` and its stale
  tie-breaking comment. (dst-audit-mprotect-fidelity;
  dst-audit-low-fidelity-cluster, seq nit)
- [x] 13. lseek on a directory fd at a nonzero offset is permitted, as Linux
  permits. (dst-audit-low-fidelity-cluster)
- [x] 14. a blocked flock waiter whose fd is closed elsewhere wakes to the
  grant Linux gives, not EBADF. (dst-audit-low-fidelity-cluster)
- [x] 15. raw `syscall.Pwrite` on an O_APPEND file appends, as Linux does.
  (dst-audit-low-fidelity-cluster)
- [x] 16. the proc-overlay fd identity contract is reachable and holds:
  `Fd()`/fstat over a proc-overlay file agree with the spec's zero
  `(st_dev, st_ino)` contract. (dst-audit-low-fidelity-cluster)
- [x] 17. the 16K-page / VA-39 host refusal is stated at its true scope —
  every dst file op, not just mappings — so the capability claim matches
  reality. (dst-audit-low-fidelity-cluster)

## Network (src/net)

- [x] 18. a host crash resets each of its connections at the surviving peer
  with RST semantics: queued and in-flight bytes discarded, the peer's next
  read returns ECONNRESET without draining. Test: crash the writer host with
  bytes in flight, peer's first read fails. (dst-audit-net-crash-drain)
- [x] 19. dialing a crashed declared host blackholes (connect ETIMEDOUT), not
  instant ECONNREFUSED — refusal requires a live kernel to answer RST.
  (dst-audit-net-kernel-shape, item 2)
- [x] 20. the retransmission horizon arms on any write against an unhealable
  path, not only on a full send buffer — undeliverable bytes never
  succeed-and-forget. (dst-audit-net-kernel-shape, item 1)
- [x] 21. dial-side local binds and ephemeral allocation check listener
  bindings: binding to a live listener's 2-tuple fails EADDRINUSE.
  (dst-audit-net-kernel-shape, item 3)
- [x] 22. a full accept backlog fails a deadline-less dial with ETIMEDOUT
  instead of hanging forever. (dst-audit-net-kernel-shape, item 4)
- [x] 23. `Close()` with unread inbound data resets the peer (read fails
  ECONNRESET), reusing the existing `unreadInbound` predicate, consistent with
  process-exit teardown (user decision 2026-07-10). "Read fails ECONNRESET"
  means WITHOUT draining, and governs both arms — app `Close()` and the
  process-exit RST arm, whose current single-end teardown lets the peer drain
  first. (dst-audit-net-kernel-shape, item 5)
- [x] 24. the `Listen(":0")` ephemeral port allocator wraps and reclaims
  closed ports, as real kernels do. (dst-audit-low-fidelity-cluster)
- [x] 25. same-host connections get the same bounded send buffer as
  cross-host, so the co-located write-write deadlock reproduces in sim (user
  decision 2026-07-10). (dst-audit-low-fidelity-cluster)

## Simulation API (src/testing/simulation)

- [ ] 26. run options apply only after `enterSimulation` admits the run: a
  rejected Run/RunWith/Test/TestWith leaves every process-global policy
  untouched, matching Explore/Replay. Test: nested run attempt inside a
  CrashTear run, torn outcomes still occur. (dst-audit-sim-crash-tear-guard)
- [ ] 27. fault-injection and clock-fault APIs invoked from outside the run's
  bubble during an active run fail loudly and deterministically, as the
  victim-naming rule already does. Test: each API from a pre-run goroutine.
  (dst-audit-sim-fault-from-nonbubble)
- [ ] 28. Explore attributes fan-out overflow distinctly, not as BudgetHit
  under `MaxSteps`. (dst-audit-low-fidelity-cluster)

## Spec & docs (docs/dst, untagged-build surface)

- [ ] 29. the untagged zero-footprint claim is made true: `finalizer`/
  `cleanupFn` carry no dead dst word untagged, `NumCPU`'s branch is
  dead-code-eliminated; if a leg is disproportionate to restore, surface it as
  a spec-amend instead — no silent divergence.
  (dst-audit-low-fidelity-cluster)
- [ ] 30. spec docs reconciled with the landed surface: DST-MEMALLOC-DET's
  contradictory normalization claim corrected, stale "pending" markers
  removed, DST-FIN-1/2/3 and DST-CLEANUP-1 defined or removed, planning
  codenames replaced with self-contained conditions, stale `Lands:` and
  pre-L2 shape descriptions corrected. (dst-audit-spec-hygiene)
