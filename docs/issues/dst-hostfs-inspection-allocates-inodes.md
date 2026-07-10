# DST: HostFS inspection allocates inodes from the shared counter

Lands: when HostFS inspection is side-effect-free on simulation state (its
throwaway disk stops drawing from dstFS.nextIno)

## Gap

Severity L (review-found 2026-07-10; pre-existing). `dstHostFS.Open` builds
a throwaway disk for a host that never touched its filesystem via
`newDstFSDisk` → `dstFSNewNode` → `dstFSAllocIno`, which increments the
SHARED per-run `dstFS.nextIno` — contradicting the adjacent comment that
inspection must not mutate simulation state. A harness `HostFS` call on an
untouched host mid-run shifts every subsequently created file's `st_ino` by
two, observable through `Stat_t` by inode-keyed SUTs (the SQLite/LMDB
per-file lock-dedup pattern the ino comment itself cites). Deterministic per
seed (no replay break), but inspection is not side-effect-free as documented.

## Required outcome

`HostFS` on an untouched host allocates no simulation inodes (a throwaway
tree with out-of-band identity, or the real disk created without observable
counter movement) and the inspection-is-read-only comment is true. Pinned by
a test comparing `st_ino` sequences with and without a mid-run HostFS
inspection.
