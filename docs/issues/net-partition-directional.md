# Network partition: one-directional (asymmetric)

`Lands:` when asymmetric-partition scenarios are needed (a later network-fault increment)

The partition fault (landed in the L3 network-faults chunk) implements **symmetric**
partitions: `Partition(a,b)` / `Isolate(h)` cut the link in BOTH directions. The spec
(`docs/dst/faults.md` §"Network faults", the Partition bullet) allows a partition
"between a host-pair (symmetric **or one-directional**)". The one-directional form
(a→b cut while b→a still flows) is deferred.

It is a real failure mode (asymmetric routing, a firewall dropping one direction) and
a known distributed-systems adversary. Implementing it needs:

- the partition table (`net/dst_partition.go`) to also key by ORDERED direction
  (a→b) alongside the symmetric set, with the API gaining a directional form (e.g.
  `PartitionOneWay(from, to)`);
- the read-side blackhole (`net/dst_wire.go` `dstWireEnd.read`) to check the
  direction matching the conn end's `localHost`→`peerHost` (each `dstWireEnd`
  already carries `localHost`/`peerHost`, so the directional check is available);
- the Dial-connect check to use the dialer→target direction;
- a regression test: a→b cut blocks a's writes reaching b while b→a still delivers.

Filed per No-silent-downscoping: symmetric-only is a partial of the spec's
"symmetric or one-directional". The symmetric mechanism does not foreclose it — each
wire end already records its directional host pair.
