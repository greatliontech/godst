# Pending feature: in-memory deterministic network under DST

**Lands:** pending feature (the first of the I/O features on the Roadmap). No
`Lands:` chunk — it is planned work, picked up when the network axis is built.

## Goal

Under `simulation.Run`, virtualize the `net` package to a fully **in-memory,
deterministic** network so that *unmodified* networked Go code is reproducible —
without the program having to model the network itself. `net.Dial`/`Listen`/
`Conn`/`PacketConn` and DNS resolution stop touching the OS and run on an
in-process simulated network keyed by address.

This is also the reliable, in-order **base** that network faults
(partition/drop/reorder/duplicate/latency) are later layered on (see the fault
feature) — build it reliable first, fault it later.

## Why it fits DST cleanly

Determinism comes for free from the existing machinery:

- A per-bubble **address registry** maps a listen address → listener.
- `Dial` ↔ `Accept` hand each other a connected pair of in-memory conns
  (`net.Pipe`-like, but address-aware), with deadlines on the **synctest fake
  clock**.
- The conns are **channel-backed**, so they are already synctest-durable (a
  blocked `Read` registers as durably-blocked → fake time advances correctly).
- Connection / accept / delivery **order is just the goroutine schedule**, which
  is already deterministic under DST. No new seed plumbing, no new RNG.

So the hard part is not determinism — it is faithfully reimplementing a slice of
`net` in userspace.

## Seam

Intercept at the *exported* surface (`net.Dial`/`DialContext`/`Listen`/
`Resolver`/`Interfaces`), gated on `dstActive()` — the same altitude as
`os.Getpid`. net's internal lookups (`interfaceTable`, the poller) stay real;
under DST the program does not exercise real sockets, so that is correct and far
less invasive than virtualizing `netFD`/the poller.

`net.Interfaces`/`InterfaceAddrs` (interface identity — MACs/IPs) fold in here:
return a fixed synthetic interface set consistent with the simulated network's
addressing, gated like the rest of the identity surface. (This subsumes the
earlier standalone net.Interfaces deferral; in fork scope there is no per-node
virtualized-network subsystem to source from, so a fixed synthetic set — with
per-run `Options` variation if ever needed — is the correct shape.)

## Scope / increments

1. **TCP core** — Dial/Listen/Accept/Conn Read/Write/Close/deadlines + the address
   registry. Prove a two-node send/recv replays identically across runs.
2. **DNS / Resolver** — `LookupHost` etc. resolve names in-memory.
3. **UDP** — `PacketConn` (ReadFrom/WriteTo) datagram semantics.
4. **Unix sockets** — address = path.
5. (later) **net.Interfaces** synthetic set; **faults** as policies on the
   registry+conns.

## Known fidelity caveat

`Dial` returns the `net.Conn` *interface*, but code that type-asserts to the
concrete `*net.TCPConn` (for `SetNoDelay`, `syscall.Conn`, raw fds) will not get
one. Most code uses the interface; this is the main limitation and must be
documented. Real raw sockets / fds are out of scope.

## Contract note

This is a change to the fork's stated contract: real network I/O moves from
"out of scope, program models it in-memory" to "owned by the fork." Record the
in-memory-net model and the fidelity caveats as a top-tier section in design.md
when this lands.
