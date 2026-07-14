# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number when an active plan exists, a self-contained condition, or
"pending feature" for planned roadmap work). At close-out, lasting rationale is
promoted into a kept-current artifact and the resolved entry is deleted.

## Open

- [dead-socket-blocked-peer-no-rst.md](./dead-socket-blocked-peer-no-rst.md) —
  a peer parked in a blocked read/write when its counterpart socket dies at
  the retransmit horizon is never re-probed and hangs where production's
  probes/retransmissions elicit an RST (`ECONNRESET`). Recorded ⊆-real
  completeness limit. Lands: when a parked op against a dead counterpart
  stream over a live link is woken into the one-shot ECONNRESET, pinned.
