# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number when an active plan exists, a self-contained condition, or
"pending feature" for planned roadmap work). At close-out, lasting rationale is
promoted into a kept-current artifact and the resolved entry is deleted.

## Open

- [dst-spin-wedge-diagnosis.md](./dst-spin-wedge-diagnosis.md) — a call-free
  in-bubble spin loop (and the sibling same-process-consumer blocked
  capability write) wedges the whole process undiagnosed (no watchdog, dead
  SIGQUIT); the boundary is recorded in design.md, the loud out-of-schedule
  diagnosis is this issue. Lands: when a sysmon-side detector can flag it
  without perturbing non-wedged seeded schedules.
- [prerun-entropy-seed-purity.md](./prerun-entropy-seed-purity.md) —
  pre-run-minted randomness (init-time map placement, sync.Map seeds,
  maphash.MakeSeed, captured math/rand values) is stream-position-dependent:
  probe-demonstrated ~8% same-seed divergence; spec coverage-table row
  overclaims; hash-function/arch replay boundary and sweep blindness ride
  along. Lands: when pre-run entropy is seed-pure (or spec narrowed by user
  ruling) plus a sweep axis catching the class.
- [backlog-park-partition-hole.md](./backlog-park-partition-hole.md) — a
  dial parked on a full backlog completes across an active permanent
  partition (production: ETIMEDOUT). Lands: when the backlog-park and
  SYN-ACK legs gate on the cut table (heal-or-horizon), pinned both
  orientations.
- [inherited-capability-poller-determinism.md](./inherited-capability-poller-determinism.md)
  — pollable InheritFile capabilities wake off host timers/netpoll:
  probe-demonstrated same-seed transcript divergence on the deadline arm;
  contract text and pollability pin disagree on the supported surface.
  Lands: when poller/deadline wakes refuse loudly or re-enter the schedule
  deterministically (user ruling), with a same-seed pin.
- [stale-capability-refusal-identity.md](./stale-capability-refusal-identity.md)
  — cross-run/post-run capability use refuses as os.ErrClosed (misdirecting;
  the capability is not closed), against the settled refusal taxonomy.
  Lands: when the temporal refusal carries a typed non-closed identity
  recorded in design.md, pinned.
- [fdatasync-directory-refusal.md](./fdatasync-directory-refusal.md) —
  recorded sim EINVAL for directory fdatasync vs host success; the
  rename-then-fdatasync-dir durability idiom false-positives. Lands: user
  ruling — commit entry durability host-faithfully or re-record the refusal
  against the fidelity mandate.
- [sparse-file-quota-model.md](./sparse-file-quota-model.md) — LimitDisk
  counts logical bytes: false ENOSPC after sparse truncate-grow, no ENOSPC
  filling holes, truncate growth uncapped. Lands: allocation-granular
  accounting or the sparse window recorded by user ruling.
- [crash-tear-intermediate-sizes.md](./crash-tear-intermediate-sizes.md) —
  binary crash-size draw misses real intermediate writeback i_sizes
  (completeness, sim ⊆ real). Lands: intermediate-size draws or the recorded
  collapse, plus a conformance row.
- [hostconfig-ip-field.md](./hostconfig-ip-field.md) — faults.md's canonical
  example uses `HostConfig.IP`, which the landed struct lacks (example does
  not compile). Lands: field lands or spec example amended (user ruling),
  with a compile-checked example test.
- [net-model-increments.md](./net-model-increments.md) — tracking artifact
  for the recorded pending net increments: unowned-address dial blackhole,
  FIN/RST first-write shape, sim-DNS, UDP/PacketConn. Lands: as each
  increment is built; deleted when the last lands.
- [post-reset-identity-collapse.md](./post-reset-identity-collapse.md) — the
  recorded stable-ECONNRESET post-reset identity vs the kernel's one-shot
  sk_err (second read EOF, write EPIPE, CLOSE_WAIT arm) — host-probed.
  Lands: user ruling — one-shot semantics land (with conformance rows) or
  the record is extended with the explicit divergence.
- [scheduler-pin-strengthening.md](./scheduler-pin-strengthening.md) — three
  latent hardening items with no reachable fault: disabled-visibility direct
  white-box pin, select-site trace-ident fold strength, Root teardown map
  order. Lands: with the next determinism test-surface extension; the Root
  item MUST land with any observable Root.Close side effect.
