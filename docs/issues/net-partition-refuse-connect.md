# Network partition: refuse connect-mode

`Lands:` when peer-down connect semantics are needed (the next network-fault increment)

The partition fault (landed in the L3 network-faults chunk) implements the
**blackhole** connect mode: a Dial across a cut link blocks until the link heals or
the dial's context/deadline expires (packets-dropped semantics). The spec
(`docs/dst/faults.md` §"Network faults", the Partition bullet) settles the connect
mode as **selectable** between blackhole and **refuse** (`ECONNREFUSED`, peer-down
semantics) — both are real TCP outcomes a SUT tests against. Only blackhole (the
harder, more realistic case) is implemented; refuse is deferred.

Refuse is a small additive variant: at the Dial partition check (`net/dst.go`
`dstDial`, the `for { dstPartWakeCh(); dstPartitioned(...) ... }` loop), return
`ECONNREFUSED` immediately instead of blocking when the targeted link's mode is
refuse. It needs:

- the partition record to carry a mode — the imperative `simulation.Partition` /
  `Isolate` API gains a refuse form (e.g. `PartitionRefuse` / a mode argument), or
  a per-link mode stored in `net/dst_partition.go`'s table;
- a regression test: a Dial across a refuse-partition fails fast with
  `ECONNREFUSED` (vs the blackhole test's block-until-deadline).

Filed per No-silent-downscoping: the blackhole-only implementation is a partial of
the settled selectable-mode spec. When the per-fault control surface lands in L4
(`Options.Faults` carrying the mode), the mode plumbs through there too.
