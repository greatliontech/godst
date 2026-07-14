# Tracking artifact for the recorded pending net-model increments

Three divergences/pending increments are recorded inline where they bite
(never silent) but had no tracking artifact pulling them forward:

- **Unowned-address dial**: a dial to an address no declared host owns
  returns `ECONNREFUSED` today; the faithful shape is the blackhole +
  `ETIMEDOUT` (nothing answers an RST for an unowned address). Recorded in
  design.md's connect-cost paragraph as "Pending (lands with the FIN/RST
  follow-on)".
- **First write after a peer's full close**: fails instantly `ECONNRESET`
  today; the production shape is accept-into-the-send-buffer, RST on a
  SUBSEQUENT op (the RST round trip). Recorded in design.md's FIN/RST leg
  as the follow-on's work.
- **Sim-DNS and UDP/PacketConn**: dial-by-hostname needs "a planned minimal
  sim-DNS increment" (faults.md, per-host address space); UDP faults are the
  packet-granular follow-on faults.md's non-foreclosure invariant names.

Lands: as each increment is built — the FIN/RST follow-on (first two items),
the sim-DNS increment, and the UDP/PacketConn axis respectively; this doc is
deleted when the last lands (each landing promotes its inline record to the
implemented contract).
