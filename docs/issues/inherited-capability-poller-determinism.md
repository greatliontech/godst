# Pollable inherited capability: poller/deadline wakes break same-seed replay

`InheritFile` preserves the source fd's `O_NONBLOCK`, so a nonblocking source
(e.g. an `os.Pipe` end — the natural relay shape) becomes a
poller-registered capability. A read parked with a future deadline (or
EAGAIN-parked I/O) wakes via a host timer/netpoll at a WALL instant; if other
bubble goroutines are runnable, the seeded scheduler's pick set at the next
boundary depends on wall time. Probe-demonstrated: capability read with a
40ms deadline against an empty pipe plus two continuously-runnable goroutines
— same seed, run 1 wake at iteration 2029, run 2 at ~2040+, byte-level
transcript divergence with both runs "passing". The determinism pins cover
only the BLOCKING arm (`TestInheritedWriteContentionSim` forces
`SetNonblock(fd, false)`; its comment states "a granted write blocks in the
syscall, never parks on the poller"), while pollability itself is pinned
supported (`TestDSTInheritFilePreservesNonblockingPollability`) and deadlines
are documented as supported operations — the contract text and the code
admit different arms. The EAGAIN-park arm shares the mechanism but was not
observed diverging in the tried configuration (mechanism-cited, unverified).

Resolution is a user ruling on the supported surface (spec-amend candidate):
either (a) the pollable arm is unsupported — capability creation strips
O_NONBLOCK or fences SetDeadline/poller parks loudly, and the pollability pin
inverts; or (b) the pollable arm is supported and its wakes must re-enter the
schedule deterministically (e.g. delivered at seeded scheduler boundaries),
which is substantial poller integration work.

Lands: when a capability poller/deadline wake either refuses loudly or
re-enters the seeded schedule at a deterministic boundary, with a same-seed
transcript-equality pin covering the deadline arm.
