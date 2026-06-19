# Clock drift: seeded / fault-RNG-drawn rate

`Lands:` when a fault policy / Explore needs to draw a drift rate, with the rest of
fault orchestration.

Drift landed in two chunks: the **declared, constant** per-host rate (`Host("h",
HostConfig{Clock: Drift(ppb)}, …)`) and the **mid-run rate change** (`DriftClock(host,
ppb)`, which re-anchors the wall and re-maps every armed timer of the host). One leg of
the spec remains deferred.

The spec (`docs/dst/faults.md` §"Clock faults") defines drift as "a host's clock rate
departs from 1 over a window … **drawn from the fault RNG or declared**." The landed work
covers the **declared** leg (`Drift`/`DriftClock` pin a rate by hand). The **fault-RNG**
leg — a per-host rate drawn deterministically from the run seed within a bound, the drift
analogue of `BoundedSkew` — is deferred. It would add a `BoundedDrift(maxPPB)` ClockConfig
that resolves to a seeded ppb via a stateless hash of (seed, host id) (mirroring
`runtime.dstHostSeededClockOffset`, advancing no RNG stream), so sweeping the seed sweeps
the bounded drift-assignment space. It lands with the broader fault-policy / Explore
exploration, where seeded fault parameters are the exploration dimension.

Filed per No-silent-downscoping: the declared constant-rate and mid-run legs ship; the
seeded leg is tracked here, not silently dropped.
