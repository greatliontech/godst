# A horizon-killed end that heals can drain post-death deliveries before its ETIMEDOUT

`dstStream.pop` serves arrived data and a delivered FIN ahead of the endpoint
error checks, and a retransmit-horizon death (`dstWireEnd.horizonCheck` /
the inline write-horizon) sets `timedOut` without freezing the INBOUND
stream. Reachable in-spec path: a one-way (or healed) cut kills end A at the
horizon (its outbound bytes exhausted); the link then heals (or the inbound
direction was never cut); the peer's bytes and FIN, delivered after A's
death, are drained by A's reads to `io.EOF` — the pended one-shot
`ETIMEDOUT` never surfaces on the read ladder. Production cannot produce
this: retransmission exhaustion runs `tcp_write_err → tcp_done` (socket
CLOSED), and a CLOSED socket answers late segments with RST — it never
queues them; the first read after the death reports the pended `ETIMEDOUT`
after draining only what arrived BEFORE the death. A sim-only execution
(⊆-real violated in the false-negative direction: the SUT sees a clean
EOF where production sees a timeout), adjacent — the pop ordering and the
non-freezing kill predate the one-shot identity work.

Direction: the horizon kill freezes the inbound stream at the kill instant
exactly as `injectRST` does (`freezeAtHorizon` — delivered-before-death bytes
drain, later arrivals are never queued), and the peer's late segments meeting
the dead socket surface on the PEER as its own failure per the reset
machinery. Bounded by the drain rule (`TestDSTNetHorizonDeathDrainsDeliveredData`
must keep passing).

Lands: when the retransmit-horizon kill freezes the victim's receive
direction at the death instant, pinned by a heal-after-death read that
reports the one-shot ETIMEDOUT instead of draining post-death deliveries.
