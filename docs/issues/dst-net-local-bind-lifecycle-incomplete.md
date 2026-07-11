# Dialer LocalAddr lacks a complete bind lifecycle

Lands: when explicit local binds validate host ownership, reserve before any
blocking handshake phase, and release on every failure

## Gap

Severity M. `dstResolveLocalTCPAddr` accepts another simulated host's routable
IP, allowing source spoofing instead of `EADDRNOTAVAIL`. Bind conflict checks
occur after partition waiting and do not reserve pending handshakes, so an
occupied tuple can time out instead of failing immediately and two backlog-
blocked dials can both establish the same local 2-tuple.

## Required outcome

An explicit bind is validated and reserved with production-shaped error
precedence before the connect can park, and released on all terminal paths.
Tests cover foreign source IP, conflict under partition, and concurrent pending
dials of one tuple.
