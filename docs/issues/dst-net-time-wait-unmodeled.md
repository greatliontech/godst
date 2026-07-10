# DST net: TIME_WAIT is unmodeled — a closed 2-tuple is immediately re-bindable

Lands: when the bind model gains a close-time hold (or the divergence is
deliberately kept, with the design.md record as the durable statement)

## Gap

Adjacent finding, chunk-21 review (2026-07-11). `dstConnBindInUse` scans only
live registered conns; `Close`/`resetConn` deregister immediately
(`src/net/dst_reset.go`, `src/net/dst.go`). A SUT that dials with an explicit
`LocalAddr`, closes (an active close enters TIME_WAIT in production), and
immediately re-dials the same 2-tuple succeeds in-sim where production
`bind(2)` without `SO_REUSEADDR` fails EADDRINUSE for 2·MSL — the
false-negative direction. The listener side is unaffected (`SO_REUSEADDR`
binds over TIME_WAIT). Recorded in design.md's bind paragraph as an unmodeled
divergence.

## Required outcome

Either the conn registry holds a closing dialer-end 2-tuple for a simulated
2·MSL (with a deterministic clock basis), or the divergence is confirmed as
deliberately unmodeled and this doc closes with design.md's record standing.
