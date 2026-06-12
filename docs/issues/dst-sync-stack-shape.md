# Untagged sync stack-shape regression (trace/pprof frame skip shift)

`Lands:` when `task test:inert-std` is required to gate green (it is red on
exactly these tests until this lands).

## Demonstrated fault

`task test:inert-std` (untagged `go test -count=1 -short std`) fails:

- `internal/trace` `TestTraceStacks/{Default,AsyncPreemptOff}`: no match for the
  Running→Waiting transition stack `[sync.(*Mutex).Lock, main.main.func7]`.
- `runtime/pprof` `TestBlockProfile/{debug=1,proto}`: the mutex entry's top
  frame is `internal/sync.(*Mutex).Lock` where the test demands
  `sync\.(\*Mutex)\.Lock`; the proto view renders the same shifted stack
  (`containsStack` is a prefix match at index 0).
- `runtime/pprof` `TestMutexProfile/{debug=1,proto,records}`: recorded stack is
  `internal/sync.(*Mutex).Unlock → sync.(*Mutex).Unlock → blockMutexN.func1`;
  the tests expect it to start at `sync.(*Mutex).Unlock`. All three views
  render the identical `saveblockevent` stack, so they fail together.

All three reproduce on the dst branch untagged and do not exist on stock
go1.26.4 — an untagged behavior change from the DST sync hooks (the build-mode
inertness contract, design.md "Completeness boundary" / DST-L2-4 hook-inert
claim, covers hooks compiling away; the *call-shape* change below escaped it).

## Mechanism (root cause)

The DST Level-2 hook refactor of `internal/sync.Mutex` turned each public
method into a thin wrapper over a parameterized body:

- `Lock()` → `m.lock(true)`; `LockNoDstHB()` → `m.lock(false)`; likewise
  `tryLock(hb)`, `unlock(hb)` (`src/internal/sync/mutex.go`).

Every body still inlines (verified with `-gcflags=-m`: `lock`, `Lock`,
`tryLock`, `unlock`, and the sync package wrappers all "can inline"), so the
fast-path performance is unchanged. But the inlined `lock`/`unlock` body is one
extra **logical** frame, and runtime traceback skip counts are inline-aware.
The fixed skip constants in `src/internal/sync/mutex.go` —
`runtime_SemacquireMutex(&m.sema, queueLifo, 2)` (line ~190) and
`runtime_Semrelease(&m.sema, _, 2)` (lines ~283, ~294) — were tuned to the
upstream depth, so every blocking/contention record now starts one frame too
deep: at `internal/sync.(*Mutex).Lock/Unlock` instead of the `sync` wrapper.
The same skipframes value feeds the trace stack via `semacquire1` → `gopark`,
which is why `TestTraceStacks` shifts identically.

A second, related untagged divergence: `sync/rwmutex.go` routes through
`lockNoDstHB`/`unlockNoDstHB` unconditionally, so untagged RWMutex contention
stacks symbolize through `sync.(*Mutex).lockNoDstHB` instead of upstream's
`sync.(*Mutex).Lock` (no std test asserts this today; same defect class).

## Candidate fixes (decide at the chunk's design step, spec read first)

1. **Restore upstream call shape untagged**: duplicate the short fast-path
   bodies so `Lock`/`TryLock`/`Unlock` keep upstream's exact logical frame
   structure (the `hb` plumbing exists only in the dst-tagged variants /
   `dst_on.go`), and tag-gate the rwmutex NoDstHB call sites. No skip-constant
   edits; untagged symbolization is byte-identical to upstream everywhere,
   including RWMutex.
2. **Bump the three skip constants 2→3**: uniform because `lockSlow`/
   `unlockSlow` are only reachable through the inlined `lock`/`unlock` bodies.
   Restores the failing tests but leaves the RWMutex `lockNoDstHB` symbol
   divergence in place, and edits upstream-tuned constants the next rebase must
   re-reconcile.

Either shape must re-run: `task test:inert-std` (all failing subtests above),
`task test` (all fast legs), and the dst-race leg (the hook paths are shared
with `-tags dst`).
