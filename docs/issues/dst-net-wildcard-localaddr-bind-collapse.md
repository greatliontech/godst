# DST net: an IP-less explicit-port LocalAddr collapses to a concrete-IP bind

Lands: when the explicit-LocalAddr bind probe models the wildcard bind
production performs (an IP-less `LocalAddr{Port: p}` conflicting with any
local binding at p, live or TIME_WAIT), or the concrete-IP collapse is
recorded in design.md's bind paragraph

## Gap

Severity L (review-found 2026-07-11, adjacent — the shape predates the
TIME_WAIT holds and governs the live-conn probe identically). The dial path
substitutes the concrete route-selected source IP for an IP-less explicit
`LocalAddr` before probing and before stamping the conn's local address
(`src/net/dst.go`, the localTCPAddr resolution), so both the live-conn scan
and the TIME_WAIT holds see only concrete-IP 2-tuples. Production
`Dialer{LocalAddr: &TCPAddr{Port: 33000}}` binds `0.0.0.0:33000`: it
conflicts with ANY local binding at 33000 — a live conn or a TIME_WAIT hold
on a different local IP of the same host blocks it with EADDRINUSE. In-sim
the probe compares the substituted concrete IP, so a hold or live end on the
host's other IP (e.g. the routable IP, when dialing a loopback target) is
invisible and the dial succeeds — a sim-only success.

## Required outcome

An IP-less explicit-port LocalAddr conflicts with any local binding at the
port on that host (live and TIME_WAIT alike), matching bind(2)'s wildcard
rule — or the concrete-IP substitution is recorded as a deliberate collapse
beside the bind paragraph's 2-tuple rule.
