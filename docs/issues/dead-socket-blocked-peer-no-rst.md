# A peer parked in a blocked read/write never learns its counterpart socket died

The dead-socket RST is detected only at the push seam (`dstWireEnd.write`'s
dead-push handling): a peer whose operation was ALREADY parked when the other
end died is never re-probed, and hangs where production surfaces
`ECONNRESET`. Concretely this is the retransmit-horizon death: every
injectRST path tears down, or has already closed, the other end in the same
fault op (the survivor's parked ops wake via its own injection or the
victim's transport close), so only the horizon death leaves a live, unaware
peer. Two reachable in-spec executions:

- Blocked writer, full send buffer, live link: the peer fills the send buffer
  toward the victim (all delivered, unread); the victim horizon-dies via its
  own outbound cut and its application leaks the conn (neither reads nor
  closes). The peer's write is parked in the buffer-full loop; the death's
  `wakeWriter` wakes it once, it re-checks `buffered >= capacity` (the freeze
  dropped nothing — everything had arrived) and re-parks forever. Production:
  the sender's zero-window probes meet the CLOSED socket, an RST returns, the
  blocked send fails `ECONNRESET`.
- Blocked reader, heal inside the disarm window: symmetric cut; the peer
  writes during the cut (bytes destroyed at the victim's death-freeze,
  `deadDropped` set) and blocks in Read; the heal lands after the victim's
  death but before the peer's `horizonCheck`, which then sees `cut=false` and
  disarms. The read hangs unless the application writes again. Production: the
  post-heal retransmission of the destroyed bytes meets the CLOSED socket, an
  RST returns, `ECONNRESET`.

Both hangs reproduce on the pre-freeze HEAD too (which additionally resurrected
delivery), so this is a completeness limit, not a regression: the sim MISSES a
real failure (⊆-real, the safe direction — never a false one). It is recorded
as such in design.md's Retransmission-horizon bullet ("a peer already PARKED in
a blocked read or write when the death lands is not re-probed").

Direction: a probe seam for parked operations — the death wakes the peer's
blocked read/write (the wake channels already fire); the woken operation
re-evaluates against the counterpart stream's frozen state and, when the link
is live in both directions, surfaces the RST via `dstDeadPushRST` (blocked
writer: its out stream frozen; blocked reader: its out stream `deadDropped`
models the retransmission that elicits the RST). The cut arms stay as they
are: a forward cut is the peer's own horizon (`heldBeyond`/`deadDropped`), a
return-only cut swallows the RST (the recorded flow-level ACK-starvation
limit). Timing collapse: the probe fires at the wake, not after a
zero-window-probe interval — the same zero-round-trip simplification the
dead-push RST records.

Lands: when a read or write parked against a dead (frozen) counterpart stream
over a live link is woken into the one-shot `ECONNRESET` instead of parking
forever, pinned by the two executions above.
