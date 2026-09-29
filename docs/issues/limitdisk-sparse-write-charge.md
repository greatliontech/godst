# LimitDisk never fails a write into a sparse hole, so a slab-write engine's ENOSPC edge is unreachable

`LimitDisk` accounts logical bytes: a sparse `Truncate` grow is charged
against the cap (and not itself ENOSPC-checked), and a later write that
fills the hole is never charged, so it never fails with ENOSPC. The
documented modeling boundary says as much ("a SUT relying on sparse
preallocation is outside this fault's honest surface").

The consumer this bounds: pando grows its store file with a sparse
truncate and writes every page with a pwrite into the hole (its
map-write windows fallocate first, but the dst build compiles them
out). A real disk refuses the pwrite that first allocates the block —
or, on delayed-allocation filesystems, the data barrier — and the
engine's commit aborts whole. Under LimitDisk none of those refusals
can happen: the only reachable ENOSPC is a file create on a full disk
(pando's `TestDSTFullDiskRefusesCreate`), and the engine pins the
in-commit edge through its own vfs seam instead of the simulator.

What would close it: charge a hole-filling write for the blocks it
first backs (the cap counts allocated blocks, not logical bytes), or
ENOSPC-check the truncate grow the way a non-sparse filesystem would.
Either changes the recorded boundary and the `TestDSTDiskENOSPC*`
expectations.

Lands: user decision — the accounting model (logical bytes versus
allocated blocks) is a modeling choice with a stated boundary today;
the consumer need is a slab-write engine's full-disk edge inside a
commit, which no other fault axis reaches.
