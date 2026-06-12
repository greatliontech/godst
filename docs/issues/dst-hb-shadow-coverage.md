# HB shadow: raceignore gating and record-coverage completion

`Lands:` when the dst-race HB recording paths (chan.go/select.go sync records,
`dstAtomicYield`'s HB contribution) are next modified.

Two related gaps in the HB shadow's fidelity/enforcement, both pre-existing
(present at the dst-sync-stack-shape chunk's base; that chunk narrowed the
first by gating the mutex/RWMutex bridges).

## 1. Channel/select/atomic HB records do not honor `raceignore`

The raceignore gate (mirroring `raceacquireg`/`racereleaseg`) covers only the
four sync-package HB-record bridges in `runtime/dst_explore_race.go`. The
channel paths call `dstRecordSyncAcquireID`/`ReleaseID` directly (chan.go,
select.go), and `dstAtomicYield` records its conservative HB contribution,
none of them consulting `g.raceignore`. TSan suppresses both under
`runtime.RaceDisable` (`__tsan_go_ignore_sync_begin` covers atomic sync), so a
bubble SUT bracketing a channel op or atomic with RaceDisable gives DST an HB
edge the race detector lacks — a disagreement with the agree-with-TSan clause
(design.md, HB recording section), reachable only by code that calls
`runtime.RaceDisable` around those ops. No std code does: the
sync.RWMutex/Pool/WaitGroup `race.Disable` brackets contain no channel ops,
and while they do contain atomics (`readerCount.Add` etc.), `sync` is a
noRaceFunc package — `s.instrumentMemory` is false there, so the compiler's
`dstAtomicYield` emission gate never fires inside it (see the noRaceFunc
comment in cmd/compile ssa.go).

Fix shape: route those records through the same raceignore check (a gated
helper, or the check inside `dstRecordSyncEventID` if runtime-internal callers
are audited to never run under a user g's raceignore); then un-scope the
parenthetical in design.md's sync-hook mechanism passage. Extend
`DSTSyncHBSuppress` with RaceDisable-bracketed channel/atomic cases as the
teeth.

## 2. Contended-path HB records have no event-stream enforcement

`DSTSyncHBSuppress`'s `mutexPair` covers the uncontended `Lock` fast path
only, and `TestExploreRecordsMutexHB`'s scenario also acquires uncontended. A
mutant dropping only `lockSlow`'s tail `dstSyncAcquireHB` (or the
`TryLock`-success record) survives the suite: HB records only prune, so
outcome-based tests cannot see them, and no event-stream assertion covers the
contended acquire. Extend the white-box fixture with a contended scenario
(blocked Lock woken by Unlock → assert the woken goroutine's acquire event)
and a successful-TryLock case.
