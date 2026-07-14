# Crash-tear size draw is binary; real writeback exposes intermediate sizes

`dst_crash_tear.go`'s size draw picks either `len(synced)` or `len(data)`.
A file grown by several unsynced appends can, on real Linux, crash at an
INTERMEDIATE on-disk i_size (per-page writeback advances the inode size
progressively), so those outcomes are unreachable in simulation — a
completeness gap (sim ⊆ real; misses real post-crash shapes, never invents
one) inside the contract's own "MAY be lost" language. The within-page
prefix-only tear is the documented sound-subset stance; this size-axis
analogue is unrecorded.

Lands: when the tear model draws crash sizes over the per-page-advanced
intermediate range (or the binary collapse is recorded in faults.md's
crash-tear section as a sound-subset boundary), with the conformance fs
grammar extended to the multi-append crash shape.
