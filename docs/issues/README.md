# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number when an active plan exists, a self-contained condition, or
"pending feature" for planned roadmap work). At close-out, lasting rationale is
promoted into a kept-current artifact and the resolved entry is deleted.

## Open

- [horizon-death-heal-resurrection.md](./horizon-death-heal-resurrection.md) —
  a retransmit-horizon-killed end whose link heals drains post-death
  deliveries to io.EOF, sidestepping its pended one-shot ETIMEDOUT (a
  CLOSED socket never queues late segments). Lands: when the horizon kill
  freezes the victim's receive direction at the death instant, pinned.
