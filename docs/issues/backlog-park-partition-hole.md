# Dial parked on a full backlog completes across an active partition

The backlog-send select in `dstDial` (net/dst.go) has no partition case, and
`dstConnectSYNACK` never consults the cut table. Reachable interleaving: fill
the accept backlog → `Partition(A,B)` (permanent, bidirectional) → the server
Accepts, freeing a slot → the parked send lands → SYNACK's checks (reset/done
only) pass → Dial returns success. Production: the retransmitted SYN is
dropped for the cut's whole duration; a permanent cut ends in ETIMEDOUT,
never success. A false negative (sim-only success under a permanent cut).

The recorded mid-flight collapse (dstConnectSYN's comment) covers only the
bounded half-RTT SYN traversal; the backlog park is unbounded, so this shape
is outside that record. The fix wants the park to model the SYN's
undeliverability: when the parked send lands (and at SYNACK completion),
consult the cut table for the respective direction and wait for heal bounded
by the already-armed retransmit horizon (ETIMEDOUT) — the same clear-path
wait the dial's front-door blackhole loop performs for cuts existing at dial
start.

Lands: when the backlog-park and SYN-ACK legs of dstDial gate completion on
the partition table (heal-or-horizon), with pins for permanent-cut ETIMEDOUT
and heal-before-horizon completion in both one-way orientations.
