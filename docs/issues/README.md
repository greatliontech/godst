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
- [backlog-park-partition-hole.md](./backlog-park-partition-hole.md) — a
  dial parked on a full backlog completes across an active permanent
  partition (production: ETIMEDOUT). Lands: when the backlog-park and
  SYN-ACK legs gate on the cut table (heal-or-horizon), pinned both
  orientations.
- [stale-capability-refusal-identity.md](./stale-capability-refusal-identity.md)
  — cross-run/post-run capability use refuses as os.ErrClosed (misdirecting;
  the capability is not closed), against the settled refusal taxonomy.
  Lands: when the temporal refusal carries a typed non-closed identity
  recorded in design.md, pinned.
- [crash-tear-intermediate-sizes.md](./crash-tear-intermediate-sizes.md) —
  binary crash-size draw misses real intermediate writeback i_sizes
  (completeness, sim ⊆ real). Lands: intermediate-size draws or the recorded
  collapse, plus a conformance row.
- [net-model-increments.md](./net-model-increments.md) — tracking artifact
  for the recorded pending net increments: unowned-address dial blackhole,
  FIN/RST first-write shape, sim-DNS, UDP/PacketConn. Lands: as each
  increment is built; deleted when the last lands.
- [scheduler-pin-strengthening.md](./scheduler-pin-strengthening.md) — three
  latent hardening items with no reachable fault: disabled-visibility direct
  white-box pin, select-site trace-ident fold strength, Root teardown map
  order. Lands: with the next determinism test-surface extension; the Root
  item MUST land with any observable Root.Close side effect.
