# DST gmdb shared writable lock-file mmap

Lands: 6

## Gap

gmdb maps its lock file `MAP_SHARED` read/write and uses atomic loads, stores, and CAS operations for reader slots, writer slots, and heartbeats. DST currently isolates process Go memory and has no shared mmap channel backed by the host-level simulated filesystem.

## Required outcome

`Mmap(fd, offset, length, PROT_READ|PROT_WRITE, MAP_SHARED)` on a simulated regular file returns a mapping backed by host-owned shared page-cache state. Co-located simulated processes mapping the same file observe each other's writes through the mapping. The model keeps Go heaps process-isolated; the shared channel is the mapped file state.

Atomic operations over shared mappings are scheduler decision points so cross-process races on lock-file slots are explored deterministically.
