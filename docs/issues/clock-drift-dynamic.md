# Clock drift: mid-run rate change + seeded drift

`Lands:` mid-run rate change — the next drift chunk (D2). Seeded/fault-RNG drift —
when a fault policy / Explore needs to draw drift, with the rest of fault orchestration.

The drift fault landed (D1, the constant-rate chunk) as a **declared, constant** per-host
rate: `Host("h", HostConfig{Clock: Drift(ppb)}, …)` runs h's clock at rate `1 + ppb/1e9`
for the host's whole life. The spec (`docs/dst/faults.md` §"Clock faults") defines drift
as "a host's clock rate departs from 1 **over a window** … **drawn from the fault RNG or
declared**." Two legs of that are deferred:

## 1. Mid-run rate change (`DriftClock`) — D2, the immediate next chunk

The constant-rate D1 covers the maximal window (host lifetime). A *sub-window* — drift
starts at base T, an NTP slew re-syncs at T2 — needs an imperative
`simulation.DriftClock(host, ppb)` (like `StepClock`) that changes the rate mid-run.

The hard part, and why it is its own chunk: a rate change at base T must re-map every
**already-armed** timer of that host. D1 converts a host-duration `d` to base `d/r` once,
at arm (`runtime.dstTimerArmForDrift` at the `(*timer).modify` choke), so a pending
timer's base `when` was fixed under the OLD rate. Under a new rate its *remaining*
host-duration re-maps: `when' = T + (when − T)·r_old/r_new`. So D2 adds (a) a per-bubble
timer-owner registry (to find host h's pending timers — the timer struct records no
owner), (b) a re-walk + re-heapify of those timers on each `DriftClock`, and (c) a wall
re-anchor that folds the drift accumulated so far into `offset` and resets `driftT0`/`rate`
so the wall stays continuous (`offset += (T − driftT0)·ppb_old/1e9`).

D1 does **not** foreclose this: the per-host clock element already carries
`{offset, driftPPB, driftT0}` (the mutable rate + anchor), and D2 only *extends* the
`modify` hook to also register the timer's owner — additive, no reshape. The wall-drift
formula and all of D1's tests carry over unchanged (constant rate is a subset).

## 2. Seeded / fault-RNG-drawn drift (`BoundedDrift`)

D1 only implements the spec's **declared** leg (`Drift(ppb)` pins a rate by hand). The
**fault-RNG** leg — a per-host rate drawn deterministically from the run seed within a
bound, the drift analogue of `BoundedSkew` — is deferred. It would add a
`BoundedDrift(maxPPB)` ClockConfig that resolves to a seeded ppb via a stateless hash of
(seed, host id) (mirroring `runtime.dstHostSeededClockOffset`, advancing no RNG stream),
so sweeping the seed sweeps the bounded drift-assignment space. It lands with the broader
fault-policy / Explore exploration, where seeded fault parameters are the exploration
dimension.

Filed per No-silent-downscoping: D1 ships the declared, constant-rate leg; the mid-run and
seeded legs are tracked here, not silently dropped.
