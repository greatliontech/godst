# A later blackhole cut cannot override an existing refuse cut

Lands: when overlapping partition sources preserve blackhole dominance for
each direction

## Gap

Severity M. `dstCutDir` stores one first-cut-wins record. Applying
`PartitionRefuse(A,B)` and then `Partition(A,B)` discards the blackhole source,
so a dial receives immediate `ECONNREFUSED`. The contract requires any active
drop source to swallow the RST and make the connection blackhole.

## Required outcome

Directional cut state composes active sources and heals them without losing
mode precedence. Tests cover both application orders, one-way overlap, and
partial heal.
