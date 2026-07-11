# Deterministic GC for DST

> Lower-tier mechanism design for **garbage collection under DST**, governed by the top-tier contract
> in [design.md](./design.md). Full-scope GC design (tiers, the per-object deterministic trigger, STW
> mark, the finalizer/cleanup drain); the landed state is Tier 2. Code conforms to the contract.

## Deterministic GC for DST (full scope)

Goal: GC under DST that is **deterministic**, **production-faithful** (the SUT sees real GC semantics —
finalizers run, weak refs clear, memory is bounded, memory-pressure behaviour is present), and
**general** (works for *any* program, not only blocking-heavy, finalizer-light ones). "It works for one
program today" is not the bar — the fork's DST must work for any program built with it.

### Why GC is nondeterministic under DST (derisked findings)

Empirical: with GC on, a heavy-alloc multi-goroutine bubble produced ~19 distinct interleavings/40
runs (same seed); `GODEBUG=gcstoptheworld=1` only cut it to ~10 — so STW alone is not enough.
Two independent sources:

1. **The pacer trigger is wall-clock-coupled.** `gcControllerState.endCycle` computes `consMark` from
   `assistDuration = now - markStartTime` (`mgcpacer.go:604`), the wall-clock mark duration, which
   feeds the next GC trigger heap goal (`mgcpacer.go:1327`). So *when* GC fires (which allocation
   point) varies run-to-run. Fractional worker scheduling also reads `nanotime` (`mgcpacer.go:859`).
2. **Concurrent mark interleaves** with runnable goroutines when GC runs while the bubble is *not*
   quiescent.

Validated *safe/deterministic* (3 reader agents + probes):
- GC **at a synctest quiescence point** (every bubble goroutine durably blocked) is deterministic —
  the mark worker runs alone (no runnable app goroutine to interleave with). Probe: 1 distinct/30,
  idle and under load.
- It does **not** corrupt bubble accounting: GC-subsystem goroutines (mark workers, `fing`, `bgsweep`,
  `bgscavenge`) are system goroutines with `g.bubble==nil` (`proc.go:5390` skips bubble inheritance),
  `changegstatus` only touches `gp.bubble` (`proc.go:1322`), and `gcStart` dissociates the caller's
  bubble for the GC (`mgc.go:746`).
- `runtime.GC()` is safe from the `synctestRun` quiescence context, works with `gcPercent<0`
  (`gcTriggerCycle` is independent of it), and mark/sweep is app-deterministic (mark order affects
  perf, not survival; weak-handle clearing is deterministic during sweep).

### The dimensions a *full* design must cover

Each dimension lists: the nondeterminism/hazard, what a general fix needs, and the gap a
quiescence-only MVP leaves.

1. **Trigger.** Wall-clock pacer (above). General fix: a deterministic trigger — a fixed heap-ratio
   (mimic GOGC without the wall-clock `consMark` feedback) *or* quiescence-driven. MVP gap: quiescence
   only fires when the bubble blocks, so it can't bound an alloc-heavy *non-blocking* span (see 11).
2. **Mark/sweep concurrency.** Concurrent mark interleaves when goroutines are runnable. General fix:
   force STW (`gcstoptheworld`) for any *mid-execution* GC; at quiescence it's already alone. MVP gap:
   MVP only GCs at quiescence, so it never needs STW — but also never GCs mid-execution.
3. **Finalizers — determinism.** `fing` runs finalizers asynchronously and unsynchronized with the
   deterministic schedule; *how many* have completed by a given point varies (probe: 7 vs 8 of 8).
   This is invisible **unless the SUT observes finalizer effects** (memory, or a side effect), but a
   *general* SUT may. General fix: **synchronous finalizer drain** at the deterministic GC point (run
   the queued finalizers on a controlled goroutine before resuming), not async `fing`. Order within
   the queue is already deterministic (reverse-LIFO, `mfinal.go:220`) given deterministic discovery.
4. **Finalizers — bubble-awareness.** `fing.bubble==nil`, so a finalizer that does **any channel op on
   a bubble channel fatals** ("channel from outside bubble", `chan.go:193/540`) — confirmed 5/5.
   General fix: run finalizers *in the owning object's bubble context* (set the running g's bubble to
   the object's bubble for the call), or exempt finalizer-context from the check. Required for any SUT
   whose finalizers touch channels.
5. **Finalizers — arbitrary behaviour.** A finalizer may allocate, spawn goroutines, or block. A
   synchronous drain must tolerate all three (a blocking finalizer must not deadlock the drain; a
   spawned goroutine must be accounted to the right bubble). General fix must define the contract.
6. **Cleanups (`runtime.AddCleanup`, Go 1.24+).** The modern finalizer replacement; same determinism +
   bubble-awareness considerations as finalizers. Must be handled identically.
7. **Weak pointers (`weak`).** Cleared during sweep in deterministic (offset) order
   (`mgcsweep.go:574`); deterministic given a deterministic GC schedule. Mostly free once GC is
   deterministic — confirm.
8. **Concurrent sweep (`bgsweep`).** Racing sweepers could make finalizer/weak *discovery* order vary.
   General fix: synchronous/serialized sweep under DST.
9. **Scavenger (`bgscavenge`).** Timer-driven memory return to the OS; logically transparent but runs
   nondeterministically. Likely disable under DST for cleanliness; confirm it's not observable.
10. **GC assists.** A goroutine allocating during a GC cycle does pacer-proportioned assist work
    (`mgcpacer`), wall-clock-influenced. With STW mid-execution GC there's no concurrent assist; with
    quiescence-only GC there's no in-flight cycle to assist. Confirm no assist path remains
    nondeterministic.
11. **Alloc-bound / non-blocking spans (the hard one).** A bubble goroutine that allocates heavily
    **without ever blocking** never reaches a quiescence point → quiescence-only GC never fires → heap
    grows unbounded (probe: 195MB and climbing). A *general* SUT can have compute/alloc-heavy phases.
    Handling this **forces mid-execution GC**, i.e. a deterministic *heap-triggered* GC (dimension 1's
    heap-ratio + dimension 2's STW), not merely quiescence. This is the single biggest reason the full
    scope is larger than the MVP.
12. **Memory-pressure-adaptive SUTs / `GOMEMLIMIT` / `ReadMemStats`.** If the SUT changes behaviour
    based on memory (cache eviction under pressure, `GOMEMLIMIT`-driven GC, reading `MemStats`), then
    GC *timing* becomes app-observable and must be deterministic *and* production-plausible — the
    heap-ratio trigger's numbers matter, not just their determinism. A program sized by explicit config
    (not GC pressure) is fine here; a memory-pressure-adaptive one may not be.

### Tiers (choose deliberately)

- **Tier 0 — superseded (was the stopgap).** GC off during a run + `sync.Pool` reap on `simulation.Run` return. *Unsound*: no
  in-run finalizers, unbounded intra-run memory, relies on a Pool-victim-cache implementation detail.
  A working stopgap, not a design.
- **Tier 1 — quiescence GC.** Force GC at synctest quiescence points; leave `fing` async. Deterministic
  and memory-bounded **for blocking-heavy SUTs that neither observe finalizer timing nor run
  channel-touching finalizers**. Covers dimensions 1–2 (at quiescence), 7–10 (largely
  free). **Leaves** dimensions 3–6 (finalizer determinism/bubble-awareness/cleanups), 11 (alloc-bound),
  12 (memory-pressure). Constraints must be documented, not hidden. **Does *not* delete the Tier-0
  `sync.Pool` reap** — that reap is a *cross-Run pool-lifetime* fix (a channel `Put` in Run 1 is `Get`
  in Run 2 with the wrong bubble stamp), orthogonal to *in-run* memory bounding: in-run quiescence GC
  does not fire after `f`'s final `Put`, so the pooled object survives to the next Run regardless. The
  reap (or an equivalent forced end-of-Run GC) stays until something else evicts cross-Run pool state.
  See "Interaction with the Tier-0 pool reap" below.
- **Tier 2 — current (landed).** Deterministic **heap-ratio-triggered STW GC** that can fire
  mid-execution (dimensions 1, 2, 11) + **synchronous, bubble-aware, deterministic finalizer & cleanup
  drain** (3–6) + synchronous sweep (8) + scavenger off (9) + deterministic assists (10). Works for any
  SUT. This is a substantial, GC-internals-deep change and the riskiest in the whole DST effort; it can
  be built incrementally (e.g. finalizer drain before the heap-trigger) but the *design* should be
  whole so increments don't foreclose each other.

### Tier 2 design (concrete)

The DST-faithful GC is the **collapse of production GC under the DST preconditions** (`GOMAXPROCS=1`,
async/sysmon preemption off, single bubble per `Run`). It is *not finer* than production (it adds no
collection capability the real GC lacks), *not coarser* (finalizers run, weak refs clear, memory is
bounded, `MemStats`/`GOMEMLIMIT` stay live), and *not foreclosing* (every piece hooks to **"a GC
cycle completed,"** never to *what triggered it*, so the two trigger sources compose — see Increments).

Canonical contracts this collapses: the GC pacer/trigger (`mgcpacer.go`, `mgc.go`), the sweep/finalizer
machinery (`mgcsweep.go`, `mfinal.go`, `mcleanup.go`), and the synctest bubble (`synctest.go`,
`chan.go`). All exist and are settled upstream, so there is no undesigned top-tier to foreclose against.

Validation legend: **[V]** empirically validated (prior derisking / probe); **[C]** argued from
construction, to be confirmed empirically when that piece is built; **[R]** confirmed by reading the
cited code this round.

#### D1 — Deterministic trigger (dimensions 1, 2, 11)

The heap trigger fires when `gcController.heapLive.Load() >= trigger` (`mgc.go:712`), where
`trigger, _ = gcController.trigger()` returns `goal - runway` (`mgcpacer.go:1245`). Of the inputs to
`trigger()`:

- `goal = gcPercentHeapGoal = heapMarked + (heapMarked + lastStackScan + globalsScan)*gcPercent/100`
  (`mgcpacer.go:1295`) — **deterministic** at `GOMAXPROCS=1` given a deterministic allocation
  sequence (every term is a byte counter advanced in fixed amounts). **[R]**
- `runway = consMark * (1-gcGoalUtilization)/gcGoalUtilization * (lastHeapScan+lastStackScan+globalsScan)`
  (`mgcpacer.go:1327`) — the **only** wall-clock-derived term: `consMark` comes from
  `assistDuration = now - markStartTime` in `endCycle` (`mgcpacer.go:604`). **[R]**

**The contract is OBSERVABLE determinism, not a byte-exact trigger.** D1 originally proposed zeroing
`runway` (the only wall-clock-derived trigger input: `consMark` carries `idleMarkTime`, and under
`gcForceBlockMode` idle mark workers still run because DST forces the *mode* but not
`debug.gcstoptheworld`, so the pacer never zeroes them — `idleMarkTime` is nonzero and varies
run-to-run). **Both the override and the no-override path were instrumented (the STW-forcing increment).** Findings:

> - **The runway override does not make the trigger deterministic.** With `runway:=0`, the trigger value
>   (`gcController.trigger()`, recorded at each `gcStart`) *still* varied run-to-run, because the trigger
>   also depends on `heapLive`/`heapMarked` (via `goal` and `sweepDistMinTrigger`, `mgcpacer.go:1287`),
>   which carry their own run-to-run accounting noise — process heap history at bubble entry plus
>   internal allocations — that cascades through the trigger feedback loop. **[V]**
> - **`numGC` (the GC *count*) is deterministic under STW** regardless of the override (churn workload:
>   17 every run with STW; grow workload: 15 every run). Without STW it can flip by ±1 at some GOGC
>   (e.g. GOGC=100 churn: 20 vs 21) because concurrent mark "allocate-black" floating garbage varies with
>   wall-clock mark timing — see D2. **[V]**
> - So the wall-clock `runway` term is **sub-observable**: it perturbs the trigger by less than the
>   accounting noise already present, and below `numGC` granularity. Pinning it buys no determinism the
>   accounting noise doesn't already deny, and removes none the system needs.

**Decision: no runway code.** Byte-exact trigger determinism is neither achievable (heapLive accounting
noise) nor the contract. What DST guarantees and what the STW-forcing increment's tests pin is **observable** determinism:
allocation results, scheduling, and `numGC` (bounded + stable). The earlier "[V] STW makes consMark
deterministic (assistTime=idleMarkTime=0)" claim was **wrong** — `idleMarkTime` is nonzero; that line
is corrected here. (Revised invariant **DST-GC-1**: *under `dstActive`, GC introduces no nondeterminism
into observable program behaviour — allocation results, scheduling, and the GC count; the exact trigger
byte carries sub-observable accounting noise and is explicitly out of scope.*)

Both trigger *sources* feed the same downstream cycle:
- **Heap-ratio** (above) — fires mid-execution from `mallocgc` (`malloc.go:1341/1453/1593/1687/1760`),
  the only thing that bounds an **alloc-heavy non-blocking span** (dimension 11; probe: 195 MB and
  climbing under quiescence-only GC). **[V]**
- **Quiescence** — fires from the `synctestRun` driver loop right after the bubble is detected durably
  blocked (`synctest.go:223`, after `gopark(synctestidle_c, …)`); the existing Tier-1 prototype hook.
  Handles the blocking-heavy common case and runs the cycle while nothing else is runnable. **[V]**

Use **both**: heap-ratio for the alloc-bound axis, quiescence so memory is reclaimed promptly at
natural blocking points (and so a blocking-heavy SUT's heap trajectory matches Tier 1).

#### D2 — STW mark + synchronous sweep (dimensions 2, 8, 10; precondition for D1/D3/D4)

Run every DST GC stop-the-world (the existing `gcstoptheworld=2` / `gcForceBlockMode` path,
`mgc.go:789-797`), gated on `dstActive()` rather than the GODEBUG. `gcForceBlockMode` does mark **and**
sweep with the world stopped (`mgc.go:673`; the synchronous-sweep special case is `mgc.go:2092`), so
this single mode-forcing delivers, in one move: **`numGC` determinism** (no concurrent mark → no
"allocate-black" floating garbage whose volume varies with wall-clock mark timing → the GC count is
stable, where concurrent GC flips it ±1 — D2 below), D3 (synchronous sweep → deterministic finalizer/
weak *discovery*, the load-bearing reason, tested with the quiescence drain), and dimension 10 (assists):
`gcAssistAlloc1` is gated on `gcBlackenEnabled` (`mgcmark.go:716`), set only during concurrent mark, so
no mutator reaches the assist path under STW. **[R]** (It does **not** make the trigger *byte*-exact —
see D1; that is not the contract.)

**What STW is and is not load-bearing for — empirical (mutation-tested at the STW-forcing increment).** The earlier claim
that STW is needed to stop *concurrent mark from reordering mutators* did **not** survive testing: at
`GOMAXPROCS=1` + `asyncpreemptoff` + the per-g RNG, GC is **transparent to mutator scheduling** for
deterministically-scheduled workloads — CPU-bound and channel-based probes are bit-identical across
runs with or without STW (a removed-STW mutation survived every such probe). The one mutator
nondeterminism reproducible at single-P is **`runtime.Gosched`-contention** (several simultaneously
runnable goroutines racing for the run queue), and it is **GC-independent**: it diverges with GC fully
*off* too. That is a **Seq 5** concern (ordering simultaneously-runnable goroutines), out of scope for
the GC work; GC *amplifies* it but STW does not fix it. So STW's load-bearing roles here are the
**demonstrable** ones: deterministic finalizer/weak **discovery** (D3/D4 — tested with the quiescence drain), assist
elimination (above), and a **safe in-bubble GC** (no concurrent GC system goroutine competing for the
bubble's single P). Its mutator-scheduling effect is not relied upon. This is why the STW-forcing increment's test
(`TestDSTGCAllocBoundDeterministic`) asserts the **demonstrable** invariant — `NumGC>0`, i.e.
the heap trigger fires and bounds memory (dimension 11) — and STW's own teeth-test lives with the quiescence drain.

**Is the heap-trigger crossing point deterministic?** **No — and this is fundamental** (corrected by
the trigger instrumentation pass). The schedule up to the crossing is deterministic, but `heapLive`
accounting is **not** byte-deterministic at `GOMAXPROCS=1`: it advances in span-granular jumps, and span
boundaries relative to the allocation stream depend on the **heap layout** (span addresses from the
process's `mmap` history), which varies run-to-run. So `heapLive` crosses `trigger` at a slightly
different allocation each run (measured: first-GC `heapLive` 4000312–4005784, a ~5 KB spread), and
`heapMarked` (the live set captured at the mark instant) wobbles with it (206728 vs 212048). This noise
is **not** wall-clock-derived and **cannot** be removed by any pacer change (it is the dropped runway
override's true reason for failing — D1). It is below the granularity of the **observable** contract
(`numGC`, allocation results, scheduling all stay deterministic — D1), but it has one real consequence
for finalizers, handled in **D4 (discovery-cycle scoping)**. The STW cycle itself is correct and does
not corrupt bubble accounting: `gcStart` dissociates the caller's bubble (`mgc.go:746-755`) and
GC-subsystem goroutines carry `bubble==nil` (`proc.go:5390`). **[V]**

#### D3 — Synchronous sweep before the drain (dimensions 7, 8)

Finalizers are **queued during sweep pass-2** (`mgcsweep.go:574-590`), the same pass that clears weak
handles — so `finq` is complete and weak clearing is done only **after** sweep finishes. **[R]** Sweep
order is span-class-linear with no RNG and no wall-clock (`mheap_.nextSpanForSweep`,
`mgcsweep.go:96-117`), hence deterministic given a fixed heap. **[R]** Production sweep is lazy/
concurrent (`bgsweep` + proportional mutator sweep via `deductSweepCredit → sweepone`,
`mcentral.go:85`, `mcache.go:254`), which would let finalizer/weak *discovery* order float.
**Mechanism:** under DST, drive sweep to completion **synchronously inside the GC step** —
`for sweepone() != ^uintptr(0) {}` (the `finishsweep_m` idiom, `mgcsweep.go:231-239`) — before waking
the drain, so `finq` is whole and weak clearing (dimension 7) is finished and deterministic. **[C]**

#### D4 — Bubble-scoped deterministic finalizer & cleanup drain (dimensions 3, 4, 5, 6)

The unifying invariant — the load-bearing piece of Tier 2:

> **Invariant (DST finalizer drain).** Finalizer and cleanup callbacks run on a goroutine whose
> `g.bubble == ` the Run's bubble, scheduled by the deterministic scheduler, woken at the deterministic
> **quiescence** point (not at GC-completion — see "Where it runs / when it drains" below, an
> as-built correction). Never on the async system `fing` (`runFinalizers`) or the async cleanup pool
> (`runCleanups`). Scope — **ownership by registration**: the drain executes exactly the callbacks
> registered by THIS run's bubble goroutines among those a simulation GC discovers in-run (a
> run-owned object discovered only after the run finalizes on the ordinary async workers, as always;
> each special carries a run-epoch stamp,
> `dstCallbackEpoch`, recorded at `SetFinalizer`/`AddCleanup`). A callback registered before the run,
> by a goroutine outside the bubble, by a foreign synctest bubble, or by a previous run is
> process-level work: even when a simulation GC discovers it mid-run, queue-time routing
> (`queuefinalizer`/`cleanupQueue.enqueue`) defers it past `dstDeactivate` with the pre-bubble queues
> and it executes on the ordinary async workers afterward. The drained set is therefore a pure
> function of the run's own activity — foreign registrations can neither appear in it nor advance the
> drain's per-g RNG stream (`TestDSTForeignCallbackDeferred`).

Why this is the faithful collapse, dimension by dimension:

- **Determinism (3).** Async `fing` completes a nondeterministic *count* by any given point (probe: 7
  vs 8 of 8). **[V]** Re-hosting both queues onto one deterministically-scheduled bubble goroutine
  makes *when each runs* a function of the schedule, not wall-clock — and the drain orders each
  batch by the per-bubble **registration sequence** before executing (see the discovery invariant
  below): block order as discovered is sweep order, which is heap-layout-dependent, so it must not
  be the execution order. (Production detail for contrast: within a finq block the order is
  reverse-LIFO (`mfinal.go:218-220`) **[R]**; cleanups are block-LIFO and, at `GOMAXPROCS=1`,
  `maxCleanupGs = max(GOMAXPROCS/4, 1) = 1` so the "concurrent" pool collapses to a single
  goroutine. **[R]**)
- **Bubble-awareness (4).** A finalizer doing a channel op fatals under async `fing` because
  `fing.bubble == nil` and the check is `c.bubble != nil && getg().bubble != c.bubble → fatal`
  (`chan.go:193/319/418/540/703`); a channel adopts its maker's bubble at creation (`chan.go:116-117`).
  **[R]** **There is exactly one bubble per `Run`** (`Run` is non-nestable), so the drain goroutine
  simply carries that bubble and *every* bubble channel op inside a finalizer succeeds — **no
  per-object bubble tracking** (the dimension-4 "adopt the owning object's bubble" collapses to "adopt
  the one bubble"). This is the *structural* form: the illegal state (finalizer in the wrong bubble) is
  unrepresentable because there is only one bubble to be in. **[R]**
- **Arbitrary behaviour (5).** On a bubble goroutine: a finalizer that **allocates** may re-cross the
  heap trigger → nested STW GC → more finq; the drain loops `for finq != nil` so it absorbs its own
  garbage and terminates when allocation stops producing it. A finalizer that **blocks** on a bubble
  channel parks the drain goroutine; another bubble goroutine wakes it — deterministic, and faithful
  (production's `fing` blocks too, just stalling all later finalizers). The driver's quiescence wake is
  **wait-reason-checked** (`dstDrainParked`): it goreadys the drain only when the drain is parked at its
  own drain park, never while it is blocked inside a callback (waking a g parked in a channel wait
  corrupts that wait's sudog queue); a pending-work quiescence that finds the drain callback-blocked
  skips the wake — sound, because the drain loops until the queues are empty when the callback's wait
  completes. A drain still callback-blocked at Run end is reported by the `total != 1` deadlock check. A
  callback that **panics** (recorded by Explore) or calls **runtime.Goexit** kills the drain: the death
  clears the driver's reference (a dead g must never be woken) and every callback the run queued or
  later discovers is **deterministically discarded** at quiescence and at Run-end teardown
  (`dstDiscardQueuedFinq`/`dstDiscardQueuedCleanups`, accounted so the queue ledger stays exact — as
  *discarded*, never as *executed*: `finexecuted` and the cleanup executed counter feed
  `runtime/metrics`, and counting never-run callbacks there falsifies a public observable post-run;
  the ledger-exactness check gets its own discard leg instead — `TestDSTFinalizerGoexitLedger`) —
  never leaked to the bubble-less async workers (DST-FIN-1/DST-CLEANUP-1). A finalizer that **spawns** a
  goroutine: the child inherits `g.bubble` via `newproc1` (normal goroutines inherit; only system
  goroutines skip it at `proc.go:5390`), so it is bubble-accounted and deterministically scheduled.
  **[R]/[C]**
- **Cleanups (6).** Identical treatment: drain the cleanup queue (`gcCleanups`, enqueued from
  `freeSpecial` at `mheap.go:2810` during sweep) on the same bubble goroutine, in block order. **[R]**

The invariant's named legs, cited across the runtime and this doc:

- **DST-FIN-1 / DST-CLEANUP-1 (bubble-only execution).** A run's finalizers/cleanups execute only on
  the run's drain goroutine — never leaked to the bubble-less async workers (`fing`, the cleanup
  pool), including at discard: a dead drain's queued callbacks are deterministically discarded, never
  executed elsewhere. *Enforced:* `TestDSTPooledFinalizerRunEndInBubble`,
  `TestDSTPooledCleanupRunEndInBubble`, `TestDSTFinalizerGoexitDrain`.
- **DST-FIN-2 / DST-CLEANUP-2 (quiescence dead set).** The callback set run at each quiescence point
  is the deterministic dead set discovered by then (the drain runs before virtual time advances; a
  callback landing mid-drain defers at most one quiescence). *Enforced:*
  `TestDSTGCFinalizerDiscoveryDeterministic`, `TestDSTCleanupRunSetDeterministic`,
  `TestDSTFinalizerBlockedDrainQuiescence`.
- **DST-FIN-3 (drain lifecycle).** A drain not stuck in a user callback exits before the run's
  end-of-bubble deadlock accounting (the `dstStopGCDrain` handshake) — it never outlives the bubble
  nor inflates its goroutine total; a drain STUCK in a blocking user finalizer at Run end surfaces
  as the deterministic bubble-deadlock diagnostic, never silently goreadied out of its wait.
  *Enforced:* `TestDSTFinalizerStuckDrainRunEnd` (the stuck arm; the clean-exit arm holds in every
  other drain test's teardown).

**Where it runs / when it drains — at quiescence, not at every GC (an as-built correction).** A
single **persistent per-`Run` drain goroutine** (`synctestGCDrain` — finalizers+cleanups), started inside the bubble by
`synctestRun` (like `bubble.main`, via `newproc1`, so `g.bubble` is the Run's bubble and it dies with the
bubble — no cross-Run leak), parked when idle. It is woken to drain **at synctest quiescence points** —
*not* after every heap-triggered GC. Heap-triggered GCs (which fire mid-burst to bound memory, dimension
11) sweep and **queue** finalizers/cleanups but do **not** wake the drain; the accumulated `finq`/cleanup
queue is drained when the bubble next reaches quiescence (the `synctestRun` driver calls
`(*synctestBubble).dstDrainAtQuiescence` right after the quiescence `gopark(synctestidle_c)` returns,
which runs the quiescence GC and `goready`s the drain). `goready` of the drain from the driver is a plain
enqueue. The drain runs the accumulated callbacks, re-parks. Hooking the drain to **quiescence** (not to
GC-completion) is what makes finalizer *timing* deterministic — see the discovery-cycle invariant next.

**The one classification the build must get right.** The drain's idle park must count as a **durable**
block for synctest quiescence accounting (`changegstatus`) — otherwise a bubble with an idle drain
goroutine never reaches `running == 0`, and the *quiescence* trigger never fires. It is legitimately
durable: the drain is woken only by the driver at quiescence. **As built:** a dedicated wait reason
`waitReasonSynctestGCDrain`, added to `isIdleInSynctest`. Additionally, the drain's start function
is `runtime`-prefixed, so it must be identified (by start-PC) in `isSystemGoroutine` as a *user*
goroutine, or `newproc1` would withhold `g.bubble` from it. **[C]**

> **Driver-stall hazard (why not run finalizers on the root).** The `synctestRun` driver goroutine
> (`bubble.root`) also has `g.bubble` set and *could* run the drain inline at quiescence — but a
> finalizer that blocks would stall the driver and wedge time advancement. Hence a **separate** drain
> goroutine, never the root. **[R]**

**Discovery-cycle scoping — what "deterministic finalizer discovery" can and cannot mean (a trigger-instrumentation finding).** D2 established that the heap-trigger **crossing point wobbles** run-to-run (heap
layout noise), so the live set captured at a *mid-burst* GC's mark instant (`heapMarked`) varies — a
finalizable temporary that is live at one run's mark instant but already dead at another's is
**discovered in a different GC cycle**. Measured: mid-run finalizer-run count 54661 / 54787 / 55417
across runs of the same seed. This is **not** curable by the drain (it is about *which cycle queues* an
object, upstream of the drain) nor by the dropped runway override (it is heap-layout noise, not
wall-clock). So the achievable invariant is **set-at-quiescence, not per-cycle**:

> **Invariant (DST finalizer discovery).** At any **quiescence point**, the set of finalizers/cleanups
> that have run equals the set of objects unreachable from the **quiescent live set** — which is
> deterministic (all goroutines durably blocked at deterministic points). The *cycle* in which a given
> object was discovered during a preceding non-quiescent burst is **not** guaranteed. The *order* in
> which one drain executes its accumulated callbacks **is deterministic**: **registration order** — a
> per-bubble sequence number stamped at `SetFinalizer`/`AddCleanup`, by which the drain orders its
> queued work before running it. Discovery hands the drain a *set* in heap-address-dependent sweep
> order (span pop order + special offsets — pre-run span-fill history, not a function of the seed);
> executing in that order would make two same-cycle callbacks with interacting side effects (shared
> state, channel sends waking different goroutines first) an unseeded schedule fork — a violation of
> DST-GC-1 and the top-tier determinism contract, which "production also leaves order unspecified"
> does not excuse: unspecified-in-production makes **any** fixed order *sound* (⊆ real), while replay
> requires the *chosen* order be a pure function of the run's own activity, which registration order
> is. The order is deterministic-for-replay, **not** a documented ordering contract for SUTs:
> production gives none, and a SUT depending on finalizer order is out of spec regardless. A SUT that
> observes finalizer effects only at quiescence boundaries (the natural `synctest` observation
> points) sees a deterministic set; one that depends on per-cycle discovery timing is relying on
> behaviour production also leaves unspecified. Registration order is the deterministic *default*,
> not a foreclosure: production can exhibit other orders, so a seeded/strategy-driven permutation of
> the drain batch is a legitimate future exploration axis — it slots in at the same point (order the
> batch before running it) with no representation change. **Landed for finalizers:** a per-run
> registration sequence (`dstCallbackSeq`, stamped at `SetFinalizer` via `dstNextCallbackSeq`, carried on
> `finalizer.dstSeq`) by which `dstDrainFinq` re-lays the detached `finq` chain (`dstSortFinqBySeq`, a
> bottom-up merge sort — package `runtime` cannot import `sort`) before `runFinqBlocks` runs it, so
> execution is ascending registration order; the block structure is preserved, so the discard/ledger
> machinery is untouched. Pinned by `TestDSTFinalizerGoexitLedger` (the Goexit finalizer registered LAST
> runs LAST → `ran==batch-1`; sweep order would run it FIRST → `ran==0`). **Landed for cleanups too:**
> `dstDrainCleanups` calls `dstSortCleanupsBySeq`, which pops every `full` cleanup block, sorts all their
> `cleanupFn`s ACROSS blocks by `cleanupFn.dstSeq` (`dstSortCleanupFnsBySeq`, the same merge sort), re-lays
> them into the same blocks, and re-pushes so the `full` LIFO pop yields the lowest-seq block first —
> leaving the blocks on `full` so the existing drain loop's discard/ledger machinery is untouched. Pinned
> by `TestDSTGCCleanupOrderRegistration` (id-0 cleanup runs first under the sort; block-LIFO sweep order
> would run a last-registered block first). Both drain legs now execute in registration order.

Draining **at quiescence** (above) is what realizes this: by a quiescence point every mid-burst GC's
queue plus a fresh quiescence GC have been folded in, so the drained set is the deterministic dead set.
The eventual finalized set (by `Run` end) is fully deterministic — confirmed: all objects' finalizers
run. **[V]** Weak-pointer clearing (dimension 7) inherits the same scoping: cleared sets are
quiescence-deterministic; the exact clearing cycle for a boundary object is not. This is the honest
contract; it is **faithful to production**, which specifies neither finalizer timing nor order.

**As built — exactly one GC per quiescence, not a fixpoint.** `dstDrainAtQuiescence` runs a
*single* fresh STW GC, then drains what it queued — it does **not** loop GC+drain to a fixpoint. This is
the faithful realization of the invariant above: an object reachable only through another finalizable
object's *still-pending* finalizer is marked alive by that GC (kept for the pending finalizer), so it is
**in** the quiescent live set and must not run at this quiescence; its finalizer runs at a later
quiescence once the earlier one has run — exactly how production resolves finalizer **chains** across
successive GC cycles. The full set is finalized by `Run` end. (A per-quiescence GC+drain fixpoint would
over-run relative to the invariant *and* loop forever on a finalizer that re-registers itself with
`SetFinalizer`.) The single `gopark` after the drain still waits for any bubble goroutine a finalizer
*unblocked* — e.g. a finalizer that sends on a channel another goroutine receives on — to run and
re-block before virtual time advances.

**Run-end fixpoint (resolves the chain-tail hazard).** The single-GC-per-quiescence rule is right
*during* the run (it matches production chain resolution), but at `Run` **end** there is no later
quiescence, so a chain whose **tail** touches a bubble channel and is dropped near the end (object B
reachable only through object A's still-pending finalizer) would otherwise be left for post-teardown
async `fing`/cleanup processing (`g.bubble == nil`), and the tail's channel op would fatal. So
`dstStopGCDrain` loops GC+drain until a GC discovers nothing new
(`dstDrainAtQuiescence` returns whether it made progress), resolving the whole chain **in-bubble**
before teardown. The loop has no finite round cap: every finite chain completes in the bubble; a callback
that continually re-registers itself is a non-terminating callback workload (like a user goroutine that
never durably blocks) and the run does not complete rather than leaking the residual to a bubble-less
async goroutine. It is sound because the SUT has exited (everything is dead, so running the full chain is
correct) and changes no in-run quiescence behavior; the cleanup drain is covered identically
(the loop checks both `finPending` and `cleanupPending`). Regressions: `DSTFinChain` (a 3-level chain with
a channel-touching tail) fatals after teardown without the fixpoint; the long-chain tests
(`DSTFinLongChain`, `DSTCleanupLongChain`) require a >256-level tail to run while `dstActive` is still true.

#### D5 — Scavenger off (dimension 9)

`bgscavenge` is timer/`nanotime`-driven (`mgcscavenge.go:507` `slept = nanotime() - start`, plus the
`s.timer` sleep) and so nondeterministic, but
logically transparent (it returns free pages to the OS; it changes RSS, not program semantics). **[R]**
**Mechanism:** under DST, park it permanently. As built: gate `scavenger.wake()` alone — outcome-
equivalent to gating both `wake` and `ready`, because `ready()` only sets `sysmonWake` and every
path that would run the scavenger funnels through `wake()`, where the gate sits. Observable only by
a SUT that reads RSS — see D6. **[C]**

#### D6 — Memory-pressure faithfulness & `GOMEMLIMIT` (dimension 12); upstreamability

Because the per-bubble relative trigger reuses production's GOGC ratio, a memory-pressure-adaptive
SUT sees a **production-plausible** GOGC heap trajectory *between quiescence points*: the heap grows to
the GOGC ratio of the bubble's live set, then STW GC. The plausibility claim is bounded honestly: the
drain machinery also runs **one full GC at every quiescence point** (D4), so a timer-driven SUT that
blocks often sees `NumGC` far above what GOGC alone would produce and a heap that rarely approaches the
GOGC ratio between blocks — deterministic and sound (GC timing is unspecified; an extra collection is
always a real execution), but not the production *cadence*, and a performance cliff for long virtual
horizons. `NumGC` under GOGC is deterministic (the GC-set-level guarantee), and
`ReadMemStats` is deterministic at observable granularity for `NumGC`; `HeapAlloc`/`NextGC` carry the
sub-observable byte-noise of the heap trigger (D2), so a SUT that branches coarsely on `MemStats`
replays, one that compares them byte-exactly may see noise. Weak-pointer clearing (dimension 7) is
deterministic at the set level — confirmed by `TestDSTWeakClearingDeterministic`.

**`GOMEMLIMIT` and RSS stats under DST — resolved by the per-run memory-limit knob.** The *env* `GOMEMLIMIT` still cannot be
honored deterministically: the per-bubble relative trigger replaced the production heap goal (`min(gcPercentHeapGoal,
memoryLimitHeapGoal)`) with a *GOGC-only* bubble-relative trigger, and `memoryLimitHeapGoal` derives
from `mappedReady` (total mapped memory), which is **not bubble-local** and **nondeterministic** under
DST (~115 KB run-to-run: mmap-arena history + ASLR + scavenger-off accumulation); honoring it makes
`NumGC` wobble (8/9 at a tight limit). GOMEMLIMIT's semantics are inherently *total-RSS*, which DST
does not model (the scavenger is parked, D5). Two fixes give the user a deterministic equivalent:
- **`Options.MemoryLimit`** — a per-run knob that bounds the bubble's *own* net heap, expressed as the
  per-object deterministic measure `bubbleMarked + dstHeapAlloc` (live-at-last-mark + per-object bytes
  allocated since), the deterministic analogue of physical `heapLive - dstHeapBase`. This is bubble-local
  and per-object deterministic, so both `NumGC` *and which cycle discovers each object* under the limit
  are reproducible in normal and `-race` builds (`TestDSTMemoryLimit` for the set level;
  `TestDSTGCPerCycleDiscoveryDeterministic`'s memlimit regime for per-cycle; the trigger in `mgc.go`
  `gcTrigger.test`). Redefined semantics under DST: *bound bubble net heap growth*, not *bound total
  RSS*. It is an upper bound on top of the GOGC trigger; when `GOGC=off` it is the sole bound (the
  `defaultHeapMinimum` floor is skipped so a limit set above it is honored).
- **RSS-derived `MemStats` are out-of-contract** — `HeapReleased`, `HeapIdle`, and `HeapSys`'s idle
  component carry `mappedReady`/sweep-`madvise` process noise that is not bubble-local, so they are
  **not** deterministic under DST and a SUT must not assert on them (same status as the init-time
  boundary: nondeterminism the SUT must not read, not nondeterminism the fork hides). An earlier version
  zeroed these fields to make `ReadMemStats` reproducible; that was removed as a SUT-accommodation — it
  *falsified* a runtime value (there are idle spans; it reported 0) to serve a SUT that read fields the
  contract already steers away from, and it didn't even cover the `Sys`/`*Sys` fields that jitter the
  same way. The honest contract is the one below: only the *set-level* observables are deterministic. A
  SUT that sizes by memory should branch on `NumGC` and use `Options.MemoryLimit`, never `HeapReleased`/
  `HeapIdle` (and only coarsely on `HeapAlloc`/`HeapInuse`, which carry the sub-observable byte noise of
  the heap trigger — D2).

**So that memory is always bounded** regardless of config, the DST trigger falls back to a fixed
`defaultHeapMinimum` floor when `GOGC=off` (`gcPercent < 0`), instead of never firing — a GOGC=off
bubble that allocates is then still *deterministically* bounded (`TestDSTGCOffMemoryBounded`) rather
than growing without limit (production would have relied on GOMEMLIMIT here, which DST cannot honor
deterministically). `defaultHeapMinimum`, the constant, not `gcController.heapMinimum`, which is
`defaultHeapMinimum*GOGC/100` and overflows to garbage when `GOGC=off`.

> **Invariant DST-MEM-1 (observable memory determinism).** Under DST, the GC-set-level memory
> observables a SUT can read — `NumGC` (under GOGC or `Options.MemoryLimit`) and the set of weak pointers
> cleared — are a deterministic function of the seed. *Violation:* a SUT branches on `NumGC` or on which
> weak refs cleared and sees different values across runs of one seed. *Enforced:*
> `TestDSTGCAllocBoundDeterministic`, `TestDSTWeakClearingDeterministic`, `TestDSTMemoryLimit`.
> (Out-of-contract — *not* deterministic, the SUT must not assert on them: the RSS-derived fields
> `HeapReleased`/`HeapIdle`/`HeapSys`-idle, the byte-level live-heap fields `HeapAlloc`/`HeapInuse`, the
> process-total `Sys`/`*Sys` fields, and `NumGC` driven by the *env* `GOMEMLIMIT` — all carry process or
> sub-observable byte noise; a SUT sizes by `NumGC` and `Options.MemoryLimit` instead.)
>
> **Invariant DST-MEM-2 (always memory-bounded).** Under DST, a bubble that allocates is
> deterministically memory-bounded for *any* GOGC/GOMEMLIMIT config: the heap trigger always fires for
> sufficient bubble growth — the GOGC-relative target when `GOGC>=0`, the `defaultHeapMinimum` floor when
> `GOGC=off`. *Violation:* a `GOGC=off` bubble allocates without bound (no in-run GC) and the simulation
> OOMs. *Enforced:* `TestDSTGCOffMemoryBounded`.

**Upstreamability.** The whole collapse is expressible as a `gcstoptheworld`-style mode —
`GODEBUG=gcdeterministic=1` ≈ {STW (already `gcstoptheworld`) + fixed `runway` + synchronous sweep +
in-schedule finalizer/cleanup drain + scavenger off}. The STW and synchronous-sweep pieces already
exist upstream; the new, narrowly-scoped knobs (fixed runway, drain hook) are the upstream-friendly
framing if this is ever proposed beyond the fork. Under DST it is gated by `dstActive()`, not a
separate GODEBUG.

#### Interaction with the Tier-0 pool reap

In-run GC (Tier 1/2) bounds **intra-Run** memory but does **not** subsume the Tier-0 inter-Run
`sync.Pool` reap. `sync.Pool` entries are evicted only by `poolCleanup` at GC start (2-generation
victim cache → two GCs to fully clear). The cross-Run hazard is: `f` does its final `Put(ch)` (ch
stamped with Run 1's bubble) and returns; **no GC fires between that `Put` and Run 2's `Get`**, so the
stale-bubble channel survives and Run 2's `Get` fatals. In-run GC, by definition, stops at `f`'s end.
So Tier 2 **retains** an end-of-`Run` reap (two quiet pool generations, sized for the 2-generation
cache) as a *pool-lifetime* mechanism distinct from in-run memory bounding. The reap is folded into
the in-bubble run-end fixpoint before `dstDeactivate`, so objects made unreachable only by Pool
eviction have their finalizers/cleanups drained with the bubble still active. Only a change to pool
*lifetime* across bubbles (out of scope here) would remove it. **[R]**

#### Increment sequence (each useful; none forecloses another)

The ordering key: **the drain hooks to "the bubble reached quiescence," never to a GC trigger** — so the
mid-burst heap trigger can be added independently of the drain, and the drain's determinism rests on the
deterministic quiescent live set, not on a deterministic trigger byte (which does not exist — D2).

1. **STW forcing + GC enabled in-run** (D2; **landed** — the STW-forcing increment). Force `gcForceBlockMode` under
   `dstActive`; stop disabling GC in `simulation.Run`; park the scavenger (D5). No runway code (D1: the override
   was tried and dropped as ineffective). Delivers memory bounding (dimension 11) and observable
   determinism (`numGC`, alloc, sched). Tests: `TestDSTGCAllocBoundDeterministic` (numGC>0 + cross-run
   identity). Foreclosure check: none — STW is the safe in-bubble default and the precondition for 2/4.
   (Synchronous sweep, D3, comes free with `gcForceBlockMode`, `mgc.go:2092`.)
2. **Quiescence GC hook** (D1 quiescence source; **landed** — the quiescence-drain increment). At the `synctestRun` driver
   quiescence point (`synctest.go`, after the `gopark(synctestidle_c)` at the loop top), run **one** fresh
   STW GC so the live set drained next is the deterministic quiescent set. Depends on 1. Foreclosure
   check: none — an added trigger site into the same STW path. **As built:** the GC is run by
   `(*synctestBubble).dstDrainAtQuiescence`, called from the driver right after the quiescence `gopark`
   returns, before virtual time advances; merged with step 3 (the GC and the drain wake are one call).
3. **Bubble-scoped drain — finalizers** (D4 for `fing`; **landed** with the quiescence drain). Persistent per-`Run`
   bubble goroutine (`synctestGCDrain` — named for finalizers+cleanups once the cleanup drain joined; created once in
   `synctestRun` so it does not perturb the root's DST RNG stream); new idle-classified wait reason
   (`waitReasonSynctestGCDrain` in `isIdleInSynctest`); identified as a user goroutine by start-PC in
   `isSystemGoroutine` so `newproc1`
   gives it the bubble. Realizes the **discovery-cycle invariant** (set-at-quiescence). Depends on 1+2.
   Foreclosure check: the drain hooks to *quiescence*, so the mid-burst heap trigger (5) needs no change
   to it. **As built, two refinements:**
   - *`fing` gated at the wake/dequeue sites, not in `queuefinalizer`.* `queuefinalizer` still sets
     `fingWake` (harmless); the scheduler's wake of `fing` (`proc.go` `findRunnable`) and `fing`'s own
     dequeue loop are gated by `dstCallbackWorkersBlocked()` (`dstActive` or the pre-active
     `dstPreparing` pass), so during a Run `finq` accumulates and only the drain drains it. The predicate
     is inert for non-DST because `dstBuild` folds false.
   - *Drain exit handshake.* The drain is a bubble goroutine and counts toward `bubble.total`, so it must
     exit before the `total != 1` deadlock check; `dstStopGCDrain` runs a final drain, sets `gcDrainExit`,
     and waits for the drain to die (invariant DST-FIN-3).
4. **Bubble-scoped drain — cleanups** (D4 for `mcleanup`; **landed** — the cleanup-drain increment). The *same* drain
   goroutine (`synctestGCDrain`), same quiescence wake, drains `gcCleanups` after `finq` via a factored
   `runCleanupBlock`/`dstDrainCleanups`; `cleanupPending` joins `finPending` in the wake decision; the
   quiescence GC's sweep already flushes per-P cleanup blocks (`mgcsweep.go`). Depends on 3. Foreclosure
   check: none. **As built — the async pools are fully dormant during activation and Run:** the finalizer
   goroutine `fing` and the cleanup-pool goroutines must neither *run* nor be *created* during a Run,
   because either would run a callback with `g.bubble == nil` (fatal on a
   bubble channel op) or, on creation via `go`, draw from the creating goroutine's DST RNG stream and
   persist across Runs (breaking reproducible-in-isolation). So: gate the `fing` wake (`proc.go`) and
   worker dequeue/callback loop (`mfinal.go`) with `dstCallbackWorkersBlocked()`; gate `createfing`
   (`mfinal.go`) under `!dstActive()`; gate the cleanup wake (`proc.go`) and worker dequeue/callback loop
   (`mcleanup.go`) with `dstCallbackWorkersBlocked()`; and gate `createGs` (`mcleanup.go`) under
   `!dstActive()`. The wake/dequeue gates matter when an async goroutine pre-exists the Run; the create
   gates when the first callback of its kind is inside the Run. Pre-bubble finalizers/
   cleanups are excluded before `dstActive`: activation sets a short `dstPreparing` gate so ordinary async
   callback workers cannot dequeue new work, performs two ordinary GC queue-detach passes before storing the seed,
   detaches any queued process-level blocks from the queues the bubble drain observes, snapshots run-local
   queued/executed baselines so detached queued work and prior-run
   callbacks already executing on async workers cannot poison the run-end fixpoint, and releases detached
   work back to the ordinary async pools after `dstDeactivate`. If an async worker is already inside a
   pre-bubble callback when the run starts and that callback later returns, the worker parks before running
   the next callback or dequeuing another block, then resumes after deactivation. If callbacks run before the
   run, they observe `dstActive=false`; if they remain deferred, they cannot enter the run's first
   quiescence drain.
   (`createfing` is the one gate not independently testable in the harness — fing pre-exists from a stdlib
   import; same mechanism as the tested `createGs`.)
5. **Mid-burst heap trigger semantics** (dimension 11 finalizer interaction; **landed** with the quiescence drain).
   Heap-triggered GCs already fire (step 1); they **queue** finalizers without waking the drain — which
   falls out of the step-3 design directly: nothing wakes the drain except the quiescence hook, so a
   mid-burst GC's queued finalizers simply wait in `finq` for the next quiescence drain. Depends on 3.
   Foreclosure check: none — it only gates *who wakes the drain*.
6. **Scavenger off** (D5). Folded into step 1 (one-liner). Listed for dimension completeness.
7. **Memory-pressure validation** (D6; **landed, with a correction** — the memory-pressure increment). Validated: `NumGC`
   under GOGC and weak-pointer clearing are deterministic (`TestDSTGCAllocBoundDeterministic`,
   `TestDSTWeakClearingDeterministic`). The verification **disproved** D6's original `GOMEMLIMIT` claim:
   the relative trigger dropped the `memoryLimitHeapGoal` term, and that goal (and `HeapReleased`)
   derive from non-bubble-local `mappedReady`, which is nondeterministic under DST — so `GOMEMLIMIT` is
   ignored and RSS stats are nondeterministic (open question, filed). Added: a `defaultHeapMinimum`
   floor so a `GOGC=off` bubble is still deterministically memory-bounded (`TestDSTGCOffMemoryBounded`).
   See D6 above.

Tier 1 ≈ steps 1–2 + 6 (memory-bounded, observably deterministic, finalizers still async — the
documented Tier-1 limitation). Tier 2 = all. Because the drain (3–4) hooks to quiescence and the
mid-burst trigger (5) only changes who wakes it, the two compose without a throwaway retrofit — the
non-foreclosure property the design is organized around. **Step 1 landed first; the drain, cleanup, and memory-pressure increments build
2–7.**

#### Investigation RESULT — per-bubble relative trigger makes finalizer discovery deterministic ✅

**Outcome: the heap-layout noise IS reducible, cheaply, and per-cycle finalizer discovery determinism
is achievable.** A throwaway prototype (instrumented overlay; reverted) established it empirically. This
**supersedes the set-at-quiescence scoping** of D2/D4 above for the trigger mechanism — those sections'
"byte-exact unachievable / discovery only set-at-quiescence" framing is the *fallback* if the mechanism
below is not adopted; with it adopted, the drain may run **per-GC** and discovery is per-cycle
deterministic.

**As built, discovery is per-cycle but the drain still runs at quiescence — these are
separate axes.** The per-bubble relative trigger (adopted) makes *discovery* per-cycle deterministic: which GC
cycle queues a given object is a function of the seed, **in the contract** since Phase 2a (the
per-object trigger made it robust to -race and binary composition) and enforced by
`TestDSTGCPerCycleDiscoveryDeterministic` — see the layered-contract section below. That is independent
of *when the queued finalizers run*. The landed drain runs them
**at quiescence**, not per-GC, deliberately: a per-GC (mid-burst) drain would execute user finalizers
while SUT goroutines are mid-execution (between cooperative yields), interleaving finalizer side effects
with the SUT; at quiescence every SUT goroutine is durably blocked, so finalizers run in isolation and
the run *set* is the deterministic quiescent dead set. So per-cycle discovery determinism (the relative trigger) and the
quiescence drain (D4, the as-built correction) compose: the cycles that queue are deterministic,
and the drain runs the accumulated set deterministically at the next quiescence.

**Root of the crossing wobble (measured).** The trigger is roughly *absolute* (≈`heapMinimum`), and
`heapLive` at **bubble entry** (`base`) varies run-to-run (349 KB–416 KB; process heap history before
the bubble). So the bubble must allocate `trigger − base` to cross, and that varies with `base`
(hypothesis (a) — baseline — dominant; span-overshoot (b) is a minor ±span residual). It is **not**
address/ASLR (`heapLive` is a byte count).

**The fix (prototype, proven).** A **per-bubble relative trigger with per-cycle re-baseline**:
- snapshot `dstHeapBase = heapLive` at bubble entry;
- fire the heap trigger on `heapLive − dstHeapBase ≥ target` instead of the absolute pacer trigger
  (`gcTrigger.test`, `gcTriggerHeap`);
- after each GC's sweep completes, **re-baseline** `dstHeapBase = heapLive` (in `gcMarkTermination`
  after `gcSweep`), so every cycle measures the bubble's own *net* allocation from the last GC's end.

The re-baseline is essential: relative-to-*entry* alone fixes only the first GC (the first GC collects
entry-time garbage, shifting the baseline); re-baselining per cycle makes every cycle's crossing the
bubble's deterministic `target`.

**Measured determinism (same seed, throwaway hashes of the per-cycle finalizer-queue sequence):**
| workload | absolute trigger | relative + rebase |
|---|---|---|
| churn (single g) | 3 distinct / 8 | **1 / 10** |
| ring buffer, varied lifetimes (single g) | 4 distinct / 6 | **1 / 10** |
| ring buffer, **5 goroutines + Gosched** | (n/a) | **1 / 12** |

Finalizer **discovery is set-based**, so it is even robust to the `Gosched`-ordering nondeterminism
(Seq 5 gap): the *set* of objects dead at each deterministic mark does not depend on interleaving order.
A small ±span `heapLive` residual remains (the GC-crossing hash had 2 distinct values in one churn run)
but it is **sub-discovery** — it did not move the finalizer-queue sequence in any tested workload.

**Open sub-decision — GOGC faithfulness vs determinism (Spec-first collapse-check).** The prototype used
a **fixed** `target` (4 MB net growth per cycle). That is deterministic and sound but **not GOGC-faithful**
for a large/growing live set (it collects every 4 MB regardless of live size, where GOGC would scale
with it). Scaling `target` with the bubble's live set the obvious way — `bubbleLive·GOGC/100` with
`bubbleLive = heapMarked − base` — **reintroduces the wobble**, because `heapMarked` includes the
process baseline live, which varies. A GOGC-faithful *and* deterministic target needs a **clean
per-bubble live measure**: force a GC at bubble entry so `base` is process-*live* (not entry garbage),
then `bubbleLive = heapMarked − base` is deterministic if the process baseline is stable during the
bubble. For a program sized by explicit config (not GC pressure — D6) the **fixed target suffices**; a
memory-pressure-adaptive one wants the GOGC-scaled-with-entry-GC version. Either stays a faithful
GOGC collapse (fixed = a floor; scaled = the real ratio); neither is *finer* than production.

**Decision for the build (supersedes the prior plan):** adopt the per-bubble relative trigger as the DST
heap trigger, so finalizer/weak **discovery is per-cycle deterministic**. Discovery and callback
*execution* are separate axes (see "As built" above): discovery tightens to per-cycle, while
the drain continues to run at quiescence, where callbacks execute in isolation against a quiescent
bubble. DST-GC-1 and D4 tighten from set-at-quiescence to **per-cycle discovery determinism**.

**The per-bubble relative trigger — IMPLEMENTED (GOGC-scaled, full-faithful).** Landed as the GOGC-scaled-with-entry-GC version
(the production-faithful option, user-chosen):
- `dstActivate` forces a full GC at bubble entry (STW under DST) and snapshots `dstHeapBase =
  gcController.heapMarked` — the process *live* set, with pre-bubble garbage collected so the baseline
  is not polluted by entry garbage the first in-bubble GC would otherwise free (which would drive
  `heapMarked` below `base`). (`runtime/dst.go`.)
- `gcTrigger.test` (`gcTriggerHeap`, `mgc.go`) under `dstActive` fires when
  `dstHeapAlloc ≥ max((heapMarked − dstHeapBase)·GOGC/100, heapMinimum)` — the production GOGC rule with
  the scaling term on the bubble's *own* live (`heapMarked − dstHeapBase`), excluding the
  run-to-run-varying process baseline, but driven off **`dstHeapAlloc`** (per-object allocated bytes since
  the last GC) rather than physical `heapLive` (Phase 2a — see the layered-contract section for why). No
  per-cycle rebase is needed: `base` is fixed at entry and `heapMarked` updates each cycle, so the target
  tracks the bubble's live set faithfully.
- Regression tests over the permanent `dstBubbleFinqFP` hook (the bubble-local total finalizers
  discovered, `finqueued − dstFinqBase`): `TestDSTGCFinalizerDiscoveryDeterministic` asserts the
  set-level finalizer discovery (`numGC` + total) is reproducible. The `dstHeapBase` baseline subtraction
  itself — the part that cancels the run-to-run-varying pre-bubble heap — is mutation-guarded by
  `TestDSTMemoryLimit`'s **baseline-independence** check: `numGC` under a fixed limit must not change when
  a large heap is retained *before* the run (16 MiB). Dropping the baseline (absolute trigger) lets that
  pre-bubble heap inflate the live total, so `numGC` explodes (9 → >16000) and the check fails. (The
  discovery workload's sparse `numGC` is robust to the baseline offset, so it is not the test that pins
  it — a gap the byte-exact-per-cycle hash papered over before it was removed; the new check closes it.)
  The per-cycle `dstFinqSeq` probe used during bring-up was removed when the discovery test went
  set-level.

**Validation + the Seq-5 boundary (measured).** The entry-GC baseline holds: `bubbleMarked =
heapMarked − base` is identical across runs (12/12) — the GC trajectory is deterministic. Finalizer
discovery is then **fully deterministic for non-contended workloads** (single goroutine, ring buffer:
20/20). A **multi-goroutine + `Gosched` contention** residual once measured here (~15 %; with GC
entirely off, a race-free interleaving observable was 4 distinct/10) was proven to be **the
scheduling-order axis, not the GC**: removing the `Gosched` made the *same* workload 20/20
deterministic. That residual is now **closed** by the system-goroutine-isolation fix (above): the
remaining nondeterminism was infrastructure goroutines consuming the bubble's scheduling RNG a
timing-varying number of times; isolating them makes the runnable order — and thus *which* objects sit
in each per-goroutine structure at the (deterministic) GC instant — a pure function of the seed even
under contention. **Scope of the relative-trigger tighten:** per-cycle discovery is deterministic *given* a deterministic
runnable order; under contention it is an interleaving-sensitive observable, dependent on that order
(the scheduling-order axis, Seq 5) exactly as every such observable is — which is why the
discovery test is single-goroutine, to isolate the GC trigger from that axis. (`finqueued` is
process-cumulative, so the test hook
subtracts a bubble-entry baseline — without that subtraction the entry GC's varying pre-bubble finalizer
count masquerades as nondeterminism; a probe-level trap worth recording.)

**The layered determinism contract (the `-race` boundary).** The determinism guarantee is **layered**,
and the layering is the trust contract (mirrored in the `testing/simulation` package doc). Originally the
GC per-cycle layer fired on physical `heapLive` and so was *not* `-race`-robust; Phase 2a moved it to
per-object `dstHeapAlloc` and pulled it into the contract (last row, and the "How per-cycle discovery is
made deterministic" subsection below the table):

| layer | guarantee | basis | under `-race` |
|---|---|---|---|
| **Logical** | scheduling, select, map, `math/rand`, values, **replay** | per-g RNG + single-P | **holds** (verified: 8/8 DST logical tests pass under `-race`, incl. GOMAXPROCS=4 churn; no race reports) |
| **Finalizer set @ quiescence** | the finalizer/cleanup *set* run by a quiescence point = objects logically unreachable there | reachability (logical) | **holds** (the quiescence drain enforces it) |
| **GC set-level** (`numGC`, total finalizer/weak set) | the GC count and the *set* of objects discovered | heap bytes, but target floors at `heapMinimum` | **holds** (the 2 GC tests pass under `-race`) |
| **GC per-cycle** — *which cycle* discovers an object | **per-object allocated bytes** (`dstHeapAlloc`) | **holds** (Phase 2a; `TestDSTGCPerCycleDiscoveryDeterministic`) |

All four layers are unconditional. Every DST heap-trigger crossing fires on `dstHeapAlloc` (per-object
allocated bytes): the floored case (`target == heapMinimum`), the GOGC-scaled case
(`target == (heapMarked − base)·GOGC/100`), and the `Options.MemoryLimit` case (the bubble's net heap
`bubbleMarked + dstHeapAlloc` vs the limit). Two closure conditions make the per-cycle row hold beyond
the channel-light workloads that first validated it (both **landed**):

- **Internal-pooled allocations are excluded from the trigger (M4).** `clearpools` leaves per-P sudog/
  defer caches alone and per-P `gFree` lists survive across runs — so whether a bubble channel op or
  `go` statement *refills* from a cache or *allocates* a fresh `sudog`/`_defer`/`g` (a heap allocation
  through the trigger counter) depended on pre-run process history, moving the crossing point for
  channel/goroutine-heavy SUTs. The fix is **not** a cache flush — flushing `gFree` would LEAK (`allgs`
  pins every `g` forever and only reuses them via `gFree`, so emptying it strands them unboundedly
  across a test's many runs). Instead the DST heap trigger **excludes** the three runtime-internal
  pooled structs `g`, `sudog`, `_defer` from `dstHeapAlloc` (`dstIsInternalPooledType`, keyed off the
  cached type descriptors): whether one is allocated or reused is a pooling artifact, not SUT heap
  growth, and stacks are already excluded (`stackalloc`, not `mallocgc`), so this makes the trigger
  consistently reflect the SUT's own objects with no leak. Pinned by `TestDSTGCPoolCarryoverDeterministic`
  (two in-process runs at one seed whose inherited `g`/`sudog` pools would otherwise shift the crossing).
- **The crossing fires on the bubble's own allocation.** The trigger test is evaluated **and the GC it
  arms is started inside the bubble-allocation gate** — a crossing latched by a bubble allocation that
  cannot start GC at that point (e.g. `m.locks > 1`) must not leak the *start* to whichever process-wide
  allocation happens next (a foreign/infra goroutine at an unseeded point). Under DST the `mallocgc`
  dispatcher is the only HEAP-trigger evaluation site: the span-grab and large/arena-allocation
  trigger checks are disabled while a run is active, so a foreign allocation can never start a
  DST-armed cycle (`TestDSTGCForeignStart`: with the trigger condition held persistently true, NumGC
  is identical with and without a foreign allocator churning tiny, small, pointerful, and large
  objects; the user-arena site is gated identically but not exercised — arenas need their own
  GOEXPERIMENT). Two consequences: a user-forced `runtime.GC()` (`gcTriggerCycle`) is bubble-controlled too —
  from a bubble goroutine it is sanctioned (the cycle runs at that call's deterministic point in the
  schedule); from any other goroutine it panics, the fault APIs' caller-position rule (a foreign
  forced cycle would mark the bubble's heap, discovering its finalizers/weaks, and zero
  `dstHeapAlloc` at a wall-clock instant). The refusal is keyed on the published simulated-process
  env (white-box `dstActivate`, which never publishes one, stays exempt), so it covers the whole run
  INCLUDING the entry stretch between the activation seed store and the bubble's creation, where a
  foreign cycle would move `heapMarked` after the `dstHeapBase` baseline snapshot and silently stale
  the relative trigger for the entire run; a call already in flight across activation is caught by a
  second refusal after the cycle-number capture (the interleaving that could hurt — a post-baseline
  cycle number — is exactly the one whose capture synchronizes with the baseline cycle and so sees
  the seed store); the runtime's own
  activation/quiescence cycles use an internal entry, `debug.FreeOSMemory` and pprof's goroutine-leak
  GC funnel through the guarded one (the leak entry refuses before arming its process-global mode
  flag, which a recovered panic would otherwise leak into the run's own next cycle). Pinned by
  `TestDSTForeignRuntimeGCPanics` (mid-run, both foreign positions) and
  `TestDSTForeignGCActivationStretch` (the entry stretch). And during an active run foreign/process heap growth never
  TRIGGERS a collection (the physical `heapLive` trigger is unreachable under `dstActive` and forcegc
  is neutralized) — foreign garbage is reclaimed only by bubble-armed (or user-forced) cycles, so a
  run whose bubble never crosses leaves foreign growth unbounded, a modeled consequence of
  bubble-keyed pacing. `GOEXPERIMENT=sizespecializedmalloc` would bypass the dispatcher with
  compiler-emitted direct calls, so `enterSimulation` refuses it, like FIPS mode. The gate also requires the allocation to be on the bubble
  goroutine's own stack: runtime bookkeeping allocated on systemstack on its behalf (e.g. `allgs`
  append-growth, whose size and timing are process history) neither counts toward `dstHeapAlloc` nor
  evaluates the trigger (`TestDSTGCSysstackAlloc`: a run that grows `allgs` and a warmed rerun that
  reuses `gFree` produce identical mid-run per-cycle discovery). The set-level test (`numGC` + total finalizers,
`TestDSTGCFinalizerDiscoveryDeterministic`) and the per-cycle test (mid-run partial discovery for the
floored, GOGC-scaled, and `MemoryLimit` regimes, `TestDSTGCPerCycleDiscoveryDeterministic`) both run in
all builds.

**How per-cycle discovery is made deterministic under `-race` (Phase 2a).** The earlier framing scoped
per-cycle determinism out of the contract because the trigger fired on **physical `heapLive`**, which
advances **span-granularly** — `gcController.update` accounts a whole span (`npages*pageSize`) when it is
grabbed (`mcache.go`), so `heapLive − heapMarked` jumps in span chunks and the allocation at which it
crosses the GC trigger depends on the bubble's **entry span-fill phase**, which varies run to run (worse
under `-race`/composition, which shift it). That moved *which cycle* discovered a given object. A
trace-hash localization (instrumenting the raw per-cycle trigger inputs) pinned it
precisely: the **logical crossing point is deterministic, only the span-granular `heapLive` accounting is
not**, and `heapMarked` is deterministic given a deterministic crossing (so no "logical live set" is
needed — that was an over-estimate of the fix).

The fix is to drive the trigger off **`dstHeapAlloc`** — bytes summed **per-object at allocation**
(`mallocgc`), using each object's size-class size (`elemsize`) — instead of `heapLive`, and to check the
trigger on **every** allocation (the `mallocgc` dispatcher, gated `dstActive()`), not only at span grabs.
`elemsize` is a deterministic function of the requested size (size classes are fixed), is
`-race`-invariant (the race detector uses shadow memory, not object redzones — object *sizes* are
identical under `-race`), and is in `heapMarked`'s units (the GC counts the same slot size), so the
GOGC-scaled comparison `dstHeapAlloc ≥ (heapMarked − base)·GOGC/100` is **exact**, not merely
proportional. The cycle boundary then lands at a deterministic allocation, so per-cycle finalizer/weak
discovery is a deterministic function of the seed in normal **and** `-race` builds, and across binary
compositions — for both the floored and the GOGC-scaled regime (measured: 300/300 identical, normal and
`-race`, both regimes).

To give the per-object accumulation a single choke point, `-tags dst` routes every heap allocation
through the `mallocgc` dispatcher (the compiler-emitted per-size-class fast paths are gated off,
`sizeSpecializedMallocEnabled && !dstBuild`; they are off by default and already off under `-race`).
Production behavior is unchanged — every piece is under `dstActive()`/`dstBuild`, dead-code-eliminated or
inactive when off.

**Residual (sub-observable, accepted).** The GOGC-scaled target's basis `heapMarked − base` carries a
rare sub-object wobble from the process baseline captured in `dstHeapBase` at entry (a pre-bubble
transient that survives the entry GC); it does not flip discovery in practice and is the same class as
the `HeapAlloc`/`HeapInuse` byte noise the contract already steers away from (DST-MEM-1). Independently,
the raw `finqueued`-based observable also counts **pre-bubble stdlib finalizers** that survive the entry
GC and die in-bubble, whose count differs between *builds* (process startup) though it is constant within
one binary; a SUT observes its *own* finalizers, which are build-invariant, so this does not affect the
contract — the per-cycle test asserts within-build replay (the `-race` contract) on the mid-run partial.

This closes the **convergence point for "full determinism under `-race`"**: the Phase-1 map confirmed the
byte-based GC trigger was the sole remaining within-build `-race`/composition nondeterminism in the
contract layer (after the system-goroutine-isolation fix and the build-invariant hash key); driving it
off per-object `dstHeapAlloc` removes it. The original investigation plan is retained below.

**Faithful-collapse note (nit).** The DST trigger goal is `heapMarked + (heapMarked − base)·GOGC/100`,
vs production's `heapMarked + (heapMarked + lastStackScan + globalsScan)·GOGC/100`. DST both subtracts
`base` and drops the stack/globals scan runway — both make DST trigger GC *more* often (smaller goal).
This is finer in *frequency* but not in *capability*: an earlier GC is always a state the real runtime
can reach (lower GOGC / memory pressure forces exactly that), and it never skips a GC production would
force. Faithful collapse; no unreachable execution.

----
*Original investigation plan (retained for provenance):*

The design above scopes finalizer discovery to **set-at-quiescence** because the heap-trigger crossing
point wobbles run-to-run (D2: `heapLive` crosses `trigger` at a different allocation each run, so
`heapMarked` and the mid-burst discovery *cycle* vary). That scoping is a *consequence* of accepting the
heap-layout noise as irreducible. **The next investigation tests whether it is actually reducible** — if
the per-bubble heap trajectory can be made byte-exact, the contract tightens to **per-cycle / byte-exact
discovery determinism** and the drain need not be quiescence-only.

Investigation plan (run *before* committing to the quiescence-only drain — the outcome
decides the drain's shape):

1. **Find the root of the crossing wobble.** `heapLive` is a byte counter advanced in span-granular
   jumps, not per object — and it is *not* an address, so ASLR/span-address variation should not by
   itself move it. Instrument (throwaway) at bubble entry and at each `gcStart` under `dstActive`:
   record `heapLive` at bubble entry (`dstActivate`/`synctestRun`), the per-size-class mcache fill
   state, and `heapLive` at the first in-bubble GC. Hypotheses to discriminate: (a) **baseline** —
   `heapLive` at bubble entry varies (process heap history before the bubble); (b) **span-fill state** —
   partially-filled spans inherited from pre-bubble allocation make the first bubble allocations grow
   `heapLive` by a varying amount before a fresh span is grabbed; (c) something else (stack scan bytes,
   runtime-internal allocs during the bubble).
2. **Prototype the fix matching the root.** Likely candidates: **(i)** measure the trigger against a
   **per-bubble baseline** — snapshot `heapLive` at bubble entry as `dstHeapBase` and fire when
   `heapLive - dstHeapBase ≥ target`, so only the bubble's *own* (deterministic) allocations decide the
   crossing; **(ii)** **flush all mcaches at bubble entry** (`prepareForSweep`/a forced
   `stopTheWorld`+flush) so allocations start from a deterministic span-fill state; **(iii)** a forced
   GC at bubble entry to pin `heapMarked`. Combine as the root demands. A full per-bubble arena/allocator
   reset is the heavy end — only if (i)/(ii) are insufficient.
3. **Measure.** Re-run the trigger-value instrumentation (the `dstMixTrigger` overlay from the STW-forcing increment) and
   a finalizer-discovery-count probe across many runs/seeds. Success = trigger value AND per-cycle
   finalizer count byte-identical across runs.
4. **Decide the contract.** If byte-exact is achieved cheaply and soundly (no production-faithfulness
   violation — the heap trajectory must stay GOGC-plausible), tighten DST-GC-1 and the D4 discovery
   invariant to per-cycle determinism, and the drain may run per-GC (simpler, more faithful to
   production timing). If not achievable cost-proportionately, the set-at-quiescence design stands and
   the drain is quiescence-only. *(Outcome: per-cycle discovery adopted; the quiescence drain was
   retained — see "Decision for the build" and "As built" above.)* **This is a Spec-first / collapse-check decision: the chosen trigger
   must remain a faithful GOGC collapse, neither finer nor coarser.**
