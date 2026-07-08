# DST gmdb crash tear fidelity

Lands: 13

## Gap

gmdb's crash-consistency value depends on adversarial unsynced writes. DST tracks current and synced file images, but true subset, reorder, and intra-page tear behavior requires enough unsynced write history to choose outcomes more precise than all-current or all-synced.

## Required outcome

The filesystem records enough unsynced write history for host crash to persist an arbitrary subset of unsynced page-cache writes, reorder their persistence, and tear individual page writes at byte granularity. Synced bytes and synced directory entries remain stable. The crash policy is deterministic for a replayed seed and fault schedule.
