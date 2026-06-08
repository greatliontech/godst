# net.Interfaces/InterfaceAddrs are not virtualized under DST (foreclosure)

**Lands:** when the virtualized-network subsystem is designed (the in-process
transport, seam map row "Network | dragonboat"; Seq 2–4). Until then a SUT that
enumerates network interfaces under a run observes the **real** host's interfaces
(MAC addresses, IPs), which vary per host — a determinism hole for any SUT that
derives node identity from a MAC/IP.

## Why it is deferred, not built

`net.Interfaces` and `net.InterfaceAddrs` were considered alongside the other
identity knobs (pid/hostname/uid/gid/NumCPU/crypto-rand, all landed). They were
**not** built because their correct shape is a *foreclosure* in the sense of the
spec-first discipline: it depends on a top-tier contract that does not exist yet.

The other knobs are scalar, host-varying, identity-only values with an obvious
fixed simulated form (pid 1, uid 7777, …). `net.Interfaces` is different:

- Its result is **structured** (interfaces, flags, hardware addrs, addr lists).
- Under the planned virtualized network, each simulated node gets its **own**
  virtual IP/MAC from the network subsystem. The correct `net.Interfaces` return
  is therefore **per-node**, sourced from that subsystem — not a single global
  fixed stub.

A global fixed-fake stub built now (one loopback + one synthetic `eth0` with a
constant MAC/IP) would be the **wrong shape**: when per-node virtual identity
lands it would have to be torn out and rebuilt to read from the network subsystem.
That is the throwaway-retrofit a foreclosure warns against. It could also mislead
a SUT that tries to *bind* to the reported IP while the network is not yet
virtualized (real binds to a fake IP fail).

## What to do when this lands

Design `net.Interfaces`/`InterfaceAddrs` as a thin read over the virtualized
network's per-node interface table (the same subsystem that assigns the node its
virtual IP/MAC for the in-process transport), gated on `dstActive()` like the
other seams, returning real interfaces outside a run. Add a determinism +
restored-outside regression test mirroring `TestDSTIdentityExtra`.

Until then the determinism contract documents the boundary: the simulated
identity covers pid/ppid/hostname/uid/gid/euid/egid/NumCPU/`os/user.Current` and
crypto/rand, **not** network interfaces.
