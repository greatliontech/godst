# DST Level-2 shared-address filtering (explosion control)

**Lands:** when shared-address filtering proper, the explosion measurement, and the
auto-instrumented equivalence acceptance land.

## Goal (design.md Level 2, D1 "Shared-address filtering" + increment 6)

The dst-race compiler auto-instruments **every** memory access, so an unmodified SUT
yields at every access. A single-owner / private / stack access is Mazurkiewicz-
independent — yielding there explores nothing new while multiplying transitions (a
trivial unsynchronized RMW already hits Exhaustive ≈ 19 448 schedules). Filtering: a
transition is meaningful only at an address ≥2 goroutines access and the current access
*conflicts* with a prior access that is not happens-before-ordered before it (different
goroutine, same addr, ≥1 write). Single-owner and HB-ordered accesses must "record but
not yield" so the search shrinks without dropping a Mazurkiewicz class (DST-L2-3).

## Spec-first decision (settled)

- **Runtime-side filtering** (per D1 "record but do not yield"), not brain-side: a
  brain-side Exhaustive reduction would corrupt the DPOR==Exhaustive oracle (it would
  compare two reductions). Keep Exhaustive naive over a *smaller* (conflict-only)
  transition set.
- This forces **decoupling access-recording from yielding**: today recording is at the
  scheduling decision (commit), so a single-owner access that does not yield has no
  decision and would not be recorded — losing the dependency for the first accessor of
  a conflicting pair. So add a per-bubble **access log** and source the DPOR
  dependency/HB relation from it (not from per-decision addrs).

## Foundation state

Foundation files: `src/runtime/dst_explore.go`, `src/testing/simulation/explore.go`,
`src/testing/simulation/explore_test.go`, `src/testing/simulation/simulation.go`. The
`dstExploreInit`/FP linkname interface changed, so the runtime and brain halves are a
coupled foundation.

Implemented so far = the **foundation only** ("step 2a"): a per-bubble access log
(`dstAccLog*` + `dstRecordAccess` + `dstAccLog*FP`) and the source-DPOR engine
re-sourced from the log (`exploreTrace.acc*`, `dporClocks`/`dporTraceClocks`/
`dporConcurrent`/`dporHB`/`addSourceBacktrack` over log entries; `indepSets` for
multi-access intervals). Yields are still emitted at **every** access (filtering not yet
turned on), so the log is one entry per scheduling decision. The foundation now passes
the standing equivalence gate: `TestDSTExploreSweep` mismatches=0.

**NOT yet done:** (b) filtering proper — `dstAccessYield` yielding only on a conflict
that is not HB-ordered before it (a pre-sized per-address conflict/HB tracker;
single-owner/HB-ordered accesses logged inline at announce, since with no reschedule
announce-order == commit-order); the explosion measurement (Exhaustive count
before/after on the RMW and a realistic SUT); a new auto-instrumented DPOR==Exhaustive
acceptance plus unfiltered-vs-filtered outcome comparison.

## Foundation bug fixed before filtering

`TestDSTExploreSweep` is the standing completeness net. During the foundation refactor it
dropped a class on 8 programs, all in the 3-goroutine families (3)+(4) — e.g. `prog#274`
(`G1=R0, G2=W0, G3=R0`): Exhaustive reached 4 outcomes, DPOR 3 (dropped the
`G3.R < G2.W < G1.R` ordering, i.e. G1 reads after the write while G3 reads before it).
These were exactly the multi-level **weak-initial composition** cases design.md flags as
"the riskiest remaining algorithmic piece".

### What was already fixed (the major insight, keep it)

The access log MUST be in **commit order** — an access logged when the goroutine is
*resumed to execute the memory op* (in `dstScheduledSelect`, the chosen goroutine's
pending access, step = `dstScheduleStep+1`), NOT in **announce order** (when the
goroutine reaches `dstAccessYield` and yields; commit happens later, after a reschedule,
so the orders differ). Logging at announce was the first bug — it took the sweep to
215/289. Switching to commit-order logging (plus logging **every** decision, addr==0 for
coarse points, so the log mirrors the decision sequence the HB clocks/witness range
over) cut it to 8/289.

### The residual divergence (fixed)

The first suspect was an index/ordering difference, but the actual root was cross-run
raw-address instability in **sleep-set pruning**. Sleep entries are carried across
stateless re-executions; the same logical `vars[0]` allocation in the sweep can have a
different numeric address in sibling runs after the explorer allocates. A sleeping write
from the previous run could therefore compare as "different address" against the current
run's read and stay asleep, pruning the weak-initial needed for `prog#274`.

Fix: keep the race/dependency relation precise within a run (per-run address equality +
HB), but make sleep-set independence conservative across re-executions: `addr=0` and
read/read commute; any nonzero pair with at least one write wakes the sleeper. This can
only under-prune, not drop a Mazurkiewicz class. Validation after the fix:
`sweep programs=289 mismatches=0 totExh=13402 totDpor=1962 maxExh=888 maxDpor=69
optimal=55`.

## Validation contract

- `TestDSTExploreSweep` mismatches=0 is the **hard gate** for the foundation — now green.
  Keep it green while filtering (b) is added (a reduction layered on an incomplete search
  drops more).
- Add a new auto-instrumented DPOR==Exhaustive check (RMW): Exhaustive tractable (count
  drops far below the ~19 448 baseline) **and** == DPOR; plus an unfiltered-vs-filtered
  outcome comparison so a filter that drops a conflict is caught (the sweep's manual
  hooks alone do not exercise truly-private accesses).
