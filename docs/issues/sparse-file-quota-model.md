# LimitDisk's logical-byte accounting misfires on sparse files

The capacity fault counts LOGICAL regular-file content (`residentLocked`),
not allocated blocks. Probe-verified divergences under `LimitDisk(h, 4096)`:

- `f.Truncate(1<<20)` succeeds (host-faithful in isolation — a hole allocates
  nothing: host shows `size=1048576 blocks=0`), but the hole's zeros are then
  COUNTED, so a subsequent `os.Create` fails ENOSPC where a real disk at that
  quota has all blocks free. Sparse preallocation is the classic WAL/journal
  pattern — a false positive.
- Writes INTO the hole are "pure overwrite: no growth" and succeed where a
  real full disk would ENOSPC allocating blocks — the paired false negative.
- Truncate growth is charged by `residentLocked` but never CHECKED against
  the cap (no ENOSPC leg in `truncateLocked`), so the disk silently enters
  the over-quota state.

The byte-content model is recorded (faults.md: "caps total regular-file
content"), but the LimitDisk doc sells it as "modeling a full disk" and the
sparse-file window is unrecorded. Spec-amend candidate (user ruling): either
record the logical-bytes stance's sparse window explicitly as a modeling
boundary, or move the accounting to allocated-extent granularity (charge on
materialization, check truncate growth), which closes both directions.

Lands: when the capacity model either charges allocation (hole-aware, with
ENOSPC on materializing writes and a checked truncate-grow leg) or the
logical-bytes stance's sparse-file window is recorded in faults.md by user
ruling.
