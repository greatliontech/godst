# Fault-path backlog reset can still be handed out by Accept

A fault-injected reset that tears down a connection still QUEUED in the
accept backlog (`dstInjectReset`'s pre-established arm, and the identical
older `resetConn` path before it) stores the reset flag and closes the
transport but leaves `acceptState == 0` — unlike the listener-close backlog
teardown, which claims `0→2` (`dst.go`, the backlog drain). A later `Accept`
can therefore CAS `0→1` and hand the torn-down connection out; its first
read fails `ECONNRESET`.

Two readings, one of which must win:

- The handed-out shape matches Linux (an RST-aborted, never-accepted child
  is observable through accept on some paths, first read `ECONNRESET`), so
  the behavior may be declared correct — then the `dstConn.acceptState`
  comment's claim that "a torn-down connection is never handed out by
  Accept" over-promises for the fault path and must be narrowed to the
  listener-close teardown it describes.
- Or the invariant is the contract — then `dstInjectReset`'s backlog arm
  (and any fault-path teardown of a queued conn) must claim `0→2` like the
  listener drain, so `Accept` skips the victim.

Pre-existing on the fault paths before the kernel-faithful reset rework
(the older `resetConn`-only matchers had the same window); found by review
of that rework, not introduced by it.

Lands: 7
