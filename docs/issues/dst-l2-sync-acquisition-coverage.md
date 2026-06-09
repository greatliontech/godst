# DST Level-2 sync-acquisition auto-hook coverage

**Lands:** when extending runtime sync-acquisition auto-hooks beyond `sync.Mutex.Lock`
and blocking channel send/recv, or when an unmodified SUT's outcome depends on one of
the acquisition orders listed below.

## Context

Level 2's completeness boundary requires every outcome-determining synchronization
acquisition order to be recorded as a `dstSyncAcquire` transition. The implemented
auto-hooks cover `sync.Mutex.Lock` (including `sync.RWMutex.Lock` through `rw.w.Lock()`)
and blocking channel send/recv. `TestDSTExploreSyncAutoInstrument` enforces those two
families.

## Gap

The design currently names acquisition-order surfaces that are not auto-hooked:

- `sync.RWMutex.RLock` reader admission through the reader sema path.
- `select` cases where channel send/recv completes through non-blocking select logic.
- `sync.Mutex.TryLock`.
- `sync.Once`'s successful first execution path.

An unmodified SUT whose observable outcome turns on one of those acquisition orders can
still drop a Mazurkiewicz class under DPOR, because the deciding transition records no
conflict identity.

## Validation Contract

- Add an unmodified SUT for each hooked surface whose observable outcome distinguishes
  both acquisition orders.
- Assert DPOR exhausts and reaches every expected outcome under `-tags dst -race`.
- Keep non-dst, plain `-race`, and dst-without-race builds free of the hook, matching
  the existing `dstSyncAcquire` gate.
