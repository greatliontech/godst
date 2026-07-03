# DST scheduling & exploration

> Lower-tier mechanism design for the **scheduling/exploration axis**, governed by the top-tier
> contract in [design.md](./design.md) (the Soundness / Completeness / Non-foreclosure invariants and
> the control-surface table). Covers the Seq 5 seam (`dstFindRunnable` / `dstSchedSelect`) and the
> Level 2 access-granularity DPOR explorer. Code conforms to the contract.

## Seq 5 design: seeded interleaving diversity (validated seam + framing)

**The framing correction (measured, not assumed).** Seq 5 is *not* a determinism fix. Seq 1a/1b
already made multi-runnable scheduling under `simulation.Run` both **deterministic** and **sound**: a
GOMAXPROCS=1 bubble with N goroutines contending through `Gosched` replays *one* interleaving across
runs (probe `DSTSchedScenario`: 1 distinct over same-seed runs). The residual is the opposite of
nondeterminism — the schedule is **seed-invariant**: every seed (1, 2, 3, 12345, 999, 777, 424242)
produces the *identical* `4,0,1,2,3` round-robin. So the harness explores exactly **one** sound
interleaving regardless of seed; an ordering bug reachable only under a *different* sound interleaving
is invisible — running N seeds re-runs the same interleaving N times. Seq 5 closes this completeness
gap by making "which runnable goroutine proceeds next" a **seeded** function: different seeds explore
different *sound* interleavings. (An earlier exploratory note that the `Gosched`/global-runq path was
*nondeterministic* was measured on a raw non-DST program; under the actual `simulation.Run` harness — sysmon
gated, asyncpreemptoff, single P, per-g RNG — it is deterministic. The fault is diversity, not noise.)

**The seam: a single unified choke point in `findRunnable` (validated).** At GOMAXPROCS=1 the runnable
set spans three places — `runnext`, the local ring, and the global runq — all drained today in
FIFO/runnext-priority order, hence seed-invariant. The seam is **one** function, `dstFindRunnable`,
spliced into `findRunnable` where it would call `runqget` then `globrunqgetbatch`: under DST@P=1 it
gathers the whole runnable set {`runnext` ∪ local ring ∪ global runq} into a stack-allocated
`dstCandidates` view (no slice — enumerating/removing a candidate allocates nothing, so it cannot
perturb GC determinism) and asks the active **strategy** which one proceeds. A chosen local-ring
element is swap-removed (head into the vacated slot, advance head); a global element is unlinked by
index; runnext is cleared. The `schedtick%61` global-fairness peek is gated off under DST (the unified
pick already considers the global runq).

Why *unified*, not per-queue: a per-queue hook (seed `runqget`, separately seed `globrunqget`) cannot
express a **global**-priority policy — and PCT (below) must run the globally-highest-priority runnable
goroutine, across local and global together. Hooking the single dequeue point in `findRunnable`, which
sits downstream of every readying source (`goready`/`injectglist`/`netpollready` — the non-foreclosure
choke points), is the literal first row of the choice-point table (*order among runnable Gs:
`runqget`/`findRunnable`*) and is strictly more diverse even for uniform-random (a `Gosched`'d
goroutine on the global runq can now interleave ahead of a locally-woken one, which the per-queue
local-before-global structure forbade). No put-side *seeding* is needed: choosing from the union
subsumes the `runnext` decision.

The **only put-side change** is to *defer* the `-race` `randomizeScheduler` shuffle under DST (gate
its three hooks with `!dstActive()`): under `-race`, `randomizeScheduler` is a `const true` and would
reorder the enqueue (and, via `randn → per-g rand`, advance the enqueuing goroutine's stream)
nondeterministically, perturbing the get-side seam's input. This is *not* put-side seeding — it hands
ordering to the seeded get-side seam, exactly as the Spec's "`randomizeScheduler` becomes seeded
policy under DST" intends. It is free in normal builds (`randomizeScheduler` is `const false`, so the
whole branch — and the `dstActive()` call — is elided).

**Gate: `dstActive() && gomaxprocs == 1`.** The non-FIFO ring removal is safe only with no concurrent
stealer — guaranteed at one P. `simulation.Run` pins GOMAXPROCS=1, so the public API always qualifies; the
white-box `dstActivate` path at GOMAXPROCS>1 (the per-g churn tests) leaves scheduling FIFO (those
tests assert per-g RNG robustness, not scheduling order). At P=1 there is no stealer, so the unified
seam holds `sched.lock` (to touch the global runq) and uses plain ring loads/stores rather than the
steal-synchronizing CAS.

**Strategies (the `simulation.RunWith` control surface).** The strategy is a per-run choice consulted at the
unified seam, `dstSchedSelect(candidates) → index`:
- **Random** (default; `simulation.Run` and `simulation.RunWith{Strategy:Random}`): uniform pick over the runnable
  set from the scheduling RNG. Different seeds → different sound interleavings.
- **PCT** (`simulation.RunWith{Strategy:PCT, Depth:d, Steps:K}`): Probabilistic Concurrency Testing. Each
  **simulation-bubble** goroutine gets a random base priority at creation (`g.dstPrio`, drawn from the
  scheduling RNG in `newproc1`, well above the change-point low band) — the creation-side draw is gated
  on the creator's bubble exactly as the selection side is (system-goroutine isolation, design.md): a
  foreign or non-bubble goroutine created mid-run consumes **no** scheduling-RNG draw, else every later
  PCT priority would shift with process-composition noise (the gate lands with the scheduler-isolation
  chunk). The seam runs the highest-priority runnable
  goroutine (ties by goid, for determinism). `d−1` **priority-change points** are placed at random
  steps in `[1,K]` (re-rooted per bubble); when the step counter reaches one, the goroutine scheduled
  at that step is dropped to a low priority — the priority inversion that exposes a depth-`d` bug. PCT
  gives a probabilistic guarantee (≈ `1/(n·K^{d−1})`) of hitting a depth-`d` interleaving per seed,
  higher yield than uniform-random for deep bugs. `Steps` should approximate the run's
  scheduling-decision count: if `K` overshoots the run length the change points fall past the end and
  never fire (PCT degenerates to a fixed seeded priority order — still sound, diverse, deterministic,
  but without the depth mechanism).

Both strategies share the seam's soundness (only runnable goroutines are ever chosen) and determinism
(every draw is from the seeded scheduling RNG, advanced in a deterministic order at P=1). `g.dstPrio`
and the PCT state are unused under Random and when DST is off.

**Scheduling faults (5c) are folded into the fault-orchestration feature, not built here (deliberate,
Spec-first).** A scheduling fault splits into two shapes on opposite sides of the foreclosure line.
*Jitter* (a per-decision seeded probability to defer the chosen goroutine) is self-contained but
marginal — it largely overlaps what Random already explores and dilutes PCT's directed-search
guarantee. The valuable, distinct form for a distributed program is the *straggler* (pin a designated
node's goroutines low — "what if node 2 is slow?"), but that needs a **victim-designation** contract,
which is exactly what the undesigned fault-orchestration feature (compose net+disk+crash+scheduling
faults under one seed) owns. Building a targeting scheme now would risk a throwaway retrofit. PCT's
change points already provide seeded, sound deprioritization of whichever goroutine runs at a change
point, covering much of the "deprioritize a runnable G" ground in the meantime. So scheduling faults
land with the fault-orchestration feature, designed against its orchestration contract. The seam is ready for them: a fault is just another policy at
`dstSchedSelect`, fed through `RunWith` options.

**Empirical validation (probe suite `DSTSchedScenario`, derisked before folding in).** Five scenarios
exercise the distinct runnable-set paths — `gosched` (global runq), `spawn` (runnext/ring), `mutex`
(sema handoff), `broadcast` (`goready` fan-out via `close`), `chanring` (channel rendezvous):
- **Determinism** (same seed → 1 interleaving over 8 runs): all five, in **normal and `-race`** builds
  — upholding the unconditional logical-determinism layer of the DST contract.
- **Diversity** (10 seeds → distinct interleavings): `gosched`/`spawn`/`mutex`/`broadcast` = 10/10;
  `chanring` = 3/10. The *reduced* `chanring` diversity is a soundness signal — channel
  happens-before pins the token path (the value sequence is causally fixed), so the seam can vary only
  the rendezvous points it is *sound* to vary. Diversity scales with real scheduling freedom.
- **Soundness** (`mutexcount`): a non-atomic counter guarded only by a `sync.Mutex` reaches exactly
  G·K for every seed, normal and `-race` — the seam never runs a goroutine blocked on the mutex (it
  selects only among the runnable set), so mutual exclusion is never violated.

**The scheduling RNG is separate, not per-g.** Selection runs on **g0** (the scheduler goroutine), a
system goroutine with no application-meaningful `g.dstrand`. So seeded scheduling draws from a
dedicated **per-bubble DST scheduling RNG** (splitmix64), re-rooted per bubble exactly like the per-g
tree (`dstBubbleRoot`), advanced once per scheduling decision. At GOMAXPROCS=1 the *sequence* of
scheduling decisions is itself deterministic (1a/1b), so a single stream advanced per decision stays
a deterministic function of the seed. (At GOMAXPROCS>1 it would not be — but `simulation.Run` pins P=1.)

**Soundness (the load-bearing argument).** The seam only ever reorders Gs that are *already runnable*
— present in `runnext`/the local ring/the global runq — i.e. each is already past the primitive
(channel FIFO, mutex handoff, happens-before) that made it runnable. Their *relative* order carries
no happens-before constraint: on a real multi-core machine they could run in either order (different
Ps, or work-stealing). So every reordering the seam can inject corresponds to a real degree of
freedom → executions ⊆ real runtime+OS (the top-tier Soundness invariant). The seam *never* pulls
from a wait queue or a not-yet-readied G, so a causally-ordered pair is unrepresentable as a reorder
candidate — soundness is structural, not a runtime check.

**Collapse-check (Spec-first).** Faithful collapse of the top-tier contract: **not finer** (adds no
scheduler choice the runtime does not already make nondeterministically; only reorders genuinely
concurrent Gs — exactly the real degree of freedom), **not coarser** (the single get-side seam sits
downstream of every readying path, so it covers the whole runnable set), **not foreclosing** (5b's
strategy hook
random→biased→PCT→exhaustive and 5c's scheduling faults specialize the *same* selection seam — "pick
from the runnable set" — so seeded-random is one policy among many, no different shape needed).
Single-tier: GOMAXPROCS=1 is the only tier; no distributed/clustered collapse applies.

### Seq-5 project invariants (enforced by 5a's tests)

- **DST-SCHED-1 (entailed: soundness).** The seam selects only among the already-runnable set; it
  never reorders a G ahead of one it has a happens-before edge to. *violation:* if seeded selection
  drew from a wait queue or reordered across a channel handoff that causally orders two goroutines,
  the harness would produce an execution the real runtime cannot — a false-positive bug report while
  every documented channel/mutex ordering guarantee still holds. *Encoding:* structural (selection
  reads only runq contents) + a regression test that channel/mutex-ordered operations keep their
  order across seeds.
- **DST-SCHED-2 (clause-explicit: determinism).** Same seed → identical interleaving. *violation:* a
  seeded selection that draws from a load-dependent source (per-m RNG, or a global the system
  goroutines advance) makes the interleaving vary run-to-run, breaking replay. *Encoding:* the
  determinism probe (1 distinct over N same-seed runs), mutation-tested.

Seed-*variation* (different seeds → different interleavings) is the feature 5a delivers, asserted by a
diversity test; it is a completeness gain, not a safety invariant (a seed-invariant schedule is sound,
just incomplete).

## Level 2 — access-granularity interleaving + DPOR (systematic concurrency testing)

Status: **implemented and validated** (increments D1–D5; see Landed). This is the completeness extension the Completeness caveat names ("Completeness can
be increased additively by injecting cooperative preemption points … still deterministically — see Seq
5"), carried to its full form: make the `-race` detector's memory-access instrumentation points double
as DST scheduling decision points, so the simulation explores interleavings at **memory-access
granularity**, pruned by **Dynamic Partial-Order Reduction (DPOR)**, with the happens-before race
detector confirming races. The result is a deterministic, systematic concurrency+race explorer: a
reproducible replacement for the "run it a thousand times and hope" race lottery.

### The gap it closes (measured)

Seq 5 made *which runnable goroutine proceeds next* a seeded, strategy-driven choice — but the
**interleaving atoms** at `GOMAXPROCS=1` are still the spans *between* cooperative yield points (channel
block, select, `Gosched`, mutex contention, goroutine create). A bug that requires a context switch at a
specific instruction *between two operations that never yield* is unreachable at this granularity, for
**every** seed and **every** Seq-5 strategy. Two faults split out (the Completeness caveat's two halves):

- **Gap A — interleaving granularity.** The switch point the bug needs falls between yield points. This
  is what Level 2 closes.
- **Gap B — physical-race *consequences* under weak memory** (torn/reordered/stale reads a relaxed
  hardware/compiler memory model permits). Reproducing these needs a relaxed-memory model checker, a
  fundamentally different mechanism from interleaving — a single sequential execution cannot produce a
  reordering. This stays where the top-tier contract already puts it: **the job of `-race`**, the
  complementary tool (Completeness caveat). Level 2 does not address Gap B and is designed not to
  foreclose a later memory-model axis.

Gap A is **demonstrated, and demonstrably pure granularity** (derisk corpus, `testdata/testprog/`
under `simulation.Run`, seeds 1..N):

| corpus fault | random | PCT(d3) | oracle |
|---|---|---|---|
| unconditional data race | 40/40 | 40/40 | `-race` (caught regardless of granularity — HB sees the pair however it runs) **[V]** |
| interleaving-conditional data race | 20/40 | 19/40 | `-race` (caught on the seed fraction whose coarse interleaving makes the pair concurrent) **[V]** |
| atomicity violation (mutex-protected), **no yield in the gap** | **0/200** | **0/200** | none — `-race` stays silent (not a data race; 0/40 control), the SUT assertion never trips **[V]** |
| *identical* atomicity violation, **one yield in the gap** | **93/200** | 2–4/200 | SUT assertion **[V]** |

The two atomicity rows differ by **exactly one scheduling point**. Without it the fault is invisible to
every seed and every current strategy; with it, uniform-random finds it ~half the time. The miss is
**purely interleaving granularity** — not logic, not reachability, not search strategy. Auto-inserting
that yield point at every (shared) memory access is Level 2. (PCT finds the depth-1 atomicity violation
*less* than random — PCT's priority discipline targets depth-*d* bugs; the strategies are complementary,
and neither closes the no-yield case. **[V]**)

### Spec-first gate

- canonical: this doc's Seq-5 seam (`dstFindRunnable`/`dstSchedSelect`, `proc.go`) + the top-tier
  **Soundness**, **Non-foreclosure**, **Completeness** invariants + the control-surface table.
- contract (top-tier): *the controllable surface is "which runnable goroutine proceeds next," hooked only
  where the runtime already makes an unspecified/RNG-driven choice; **executions ⊆ real**.*
- mechanism: (D1) access-granularity cooperative yield points auto-inserted at the `-race` access hooks
  under a dst-race compiler mode, **guarded to safe points**, feeding the **same** `dstFindRunnable`
  seam; (D2) a happens-before tracker; (D3) stateless DPOR as a `dstSchedSelect` strategy; (D4) an outer
  `Explore` loop + public API; the `-race` HB detector is the oracle (D5).
- collapse-check: faithful. **Not finer** — a yield at a user memory access is a real degree of freedom
  (the OS can deschedule a goroutine between any two instructions); the guard forbids exactly the yields
  the runtime could *not* take (lock held, on g0, non-bubble), so no choice the real system lacks is
  added. **Not coarser** — adds transition boundaries, removes none of the existing coarse ones. **Not
  foreclosing** — DPOR specializes the same selection seam (random→PCT→DPOR→optimal-DPOR); access yields
  are the design's own additive completeness mechanism; the outer loop is built *on* the seam, not a new
  shape for it. Gap B (weak memory) is a different mechanism and is left to `-race`, unforeclosed.
- Single-tier: `GOMAXPROCS=1` is the only tier; the soundness collapse is "reorder only the already-
  runnable set," unchanged from Seq 5.

### Why the safe-point soundness is structural, not a runtime gamble (derisked)

The `-race` access hooks (`raceread`/`racewrite`/`racereadrange`/`racewriterange`) are compiler-inserted
**only** into functions the compiler instruments, and the compiler **excludes** the runtime and the
synchronization primitives: `runtime`, `internal/runtime/*`, `sync`, `sync/atomic` carry `NoInstrument`/
`NoRaceFunc` (`cmd/internal/objabi/pkgspecial.go`). Disassembly confirms it: **0** race hooks inside
`sync.(*Mutex).Lock`/`Unlock`, **0** inside `runtime.goyield_m`, **19** inside an ordinary user function.
**[V]** So a yield-at-access can fire only in user / non-primitive-stdlib code — **never** inside a mutex
primitive, a channel op, or the scheduler. The dangerous "yield while manipulating lock state / holding a
runtime lock" is unrepresentable by construction. The residual (e.g. a `procPin` reached from
instrumented code) is caught by the **safe-point guard**, and *skipping a yield point is always sound*
(it only reduces completeness) — so no unsound yield can ever occur. Yielding while holding a *user*
mutex is sound and is exactly the interleaving Gap A needs.

### Project invariants (Level 2)

- **DST-L2-1 (entailed: soundness).** Every injected access yield corresponds to a real degree of
  freedom; the seam still selects only among the already-runnable set. *violation:* a failure
  (split-brain, lost commit, false race) reachable only because the harness switched at a point the real
  runtime could not (lock held, on g0, a not-yet-runnable G) — a false positive while every documented
  ordering guarantee holds. *Encoding:* structural (the guard `dstActive && g.bubble != nil && g ==
  m.curg && m.locks == 0`; selection reads only runq contents) **+** a regression test that a
  mutex-protected non-atomic counter under access-granularity yielding still reaches exactly `G·K` (no
  lost update — a blocked G is never run), and channel/mutex-ordered operations keep their order.
- **DST-L2-2 (clause-explicit: determinism).** Same `(seed, schedule)` → identical execution, identical
  recorded access stream, identical verdict (race report / assertion). *violation:* replay of a fixed
  `(seed, schedule)` diverges, so a found bug is not reproducible. *Encoding:* a per-`(seed, schedule)`
  trace-hash stability probe (1 distinct over N runs, normal **and** `-race`), mutation-tested.
- **DST-L2-3 (entailed: DPOR completeness).** The explored set contains at least one execution from every
  Mazurkiewicz trace-equivalence class reachable by the SUT — equivalently, for every pair of *dependent*
  co-enabled transitions, both orderings are explored, and only provably-equivalent (independent)
  reorderings are pruned. Two transitions are *dependent* iff they record overlapping nonzero memory byte
  intervals (`dstAccessYield`/`dstAccessYieldRange`) **or** the same synchronization object's identity
  (`dstSyncAcquire`) — with ≥1 write, by different goroutines, and are not happens-before-ordered.
  (Synchronization object decision order is a dependency: omitting it drops a class — see "Completeness
  boundary".) *violation:* a non-equivalent interleaving — hence a reachable bug — is omitted, so
  "explored to exhaustion" is a false negative. *Encoding:* **`TestDSTExploreSweep`** — for a generated
  family of small closed programs (reads/writes over shared vars, with/without mutexes; plus channel
  rendezvous-order SUTs), the DPOR explored *outcome set* equals brute-force `exhaustiveExplore` for
  every member (802 SUTs — the original 290 plus the atomic, atomic-plain-mixed, multi-way, and two-variable-mixed families — mutation-tested: 23 of the original family fail with `dstSyncAcquire` neutered; 411 fail with `dstAtomicYield` neutered). The committed
  micro-SUTs (`TestDSTExploreComplete` etc.) are the weak per-shape net this generalizes.
- **Hardening clauses (land with the exploration-hardening chunk).** Four consequences of the above,
  made explicit because each was violable in detail while the headline invariant read as satisfied:
  1. **Every capacity that can drop recorded information reports itself.** The sync-HB-event buffer is
     a completeness input, not only a soundness one: a *silently* dropped release/acquire event
     under-orders the trace-HB used for **weak-initial** computation, and a spurious weak-initial can
     early-return `addSourceBacktrack` before the genuine reversal is seeded — a dropped Mazurkiewicz
     class while `Exhausted=true` (a DST-L2-3 violation reached through DST-L2-2's own machinery). The
     "a missing edge only over-explores" argument holds for the reorderability gate alone, not for
     weak-initials. So sync-event overflow folds into the trace's overflow flag exactly as access-log
     overflow does: `Exhausted=false`, reported, test-pinned.
  2. **Filter-capacity accounting is address-independent.** Whether the access filter degrades to
     conservative mode must be a function of counts the schedule determines (entries, syncs, procs),
     never of how an entry's byte range straddles pages at its run-local *address* — an
     alignment-dependent pool exhaustion flips conservative at a different point in a fresh replay
     process, misaligning the prefix (a DST-L2-2 abort, or silent divergence).
  3. **A guard-failing access is recorded, not dropped** (D1's "record the access but do not yield" is
     normative): an access that cannot *yield* can still *conflict*, and dropping it from the
     dependency relation silently prunes its class.
  4. **Replay divergence is detected, not assumed away**: replay cross-checks the recorded enabled
     sets over the schedule prefix (not only "the prefix named a non-enabled seq"), and an abort is
     attributed to SUT-panic truncation only when the abort is actually inside the panicking
     schedule's truncated suffix — a genuine DST-L2-2 violation coinciding with a panicking schedule
     must not be masked. Internal invariant breaches in the backtrack machinery surface as the
     DST-L2-2 diagnostic, never a bare index panic.
- **DST-L2-4 (clause-explicit: production untouched).** Level-2 hooks are build-mode inert outside
  `-tags dst -race`: a non-`dst-race` build emits no compiler-inserted `dstAccessYield`,
  `dstAccessYieldRange`, or `dstAtomicYield` calls, and runtime sync-decision/HB hooks are inactive unless DST is active under
  the scheduled strategy. Runtime structs may carry inert DST fields in this fork; byte-identical layout is
  not the contract. *violation:* production, plain-`-race`, or `-tags dst` without `-race` emits or executes
  Level-2 hooks, changing behavior or scheduling. *Encoding:* a build-mode objdump test proves user code
  calls `runtime.dstAccessYield`/`runtime.dstAccessYieldRange` only when both `-tags dst` and `-race` are
  present; runtime hooks are gated by `dstBuild`/`raceenabled` and `dstActive`/scheduled-strategy guards.

### Mechanism

#### D1 — Access-granularity yield points (the new transition boundary)

A **transition boundary** under Level 2 is, at a safe point on a SUT (bubble) goroutine, either (a) an
instrumented **memory access** (read/write/range) to a *shared* address (`dstAccessYield`), or (b) a
**synchronization object decision**: mutex/RWMutex acquire, try, release, and channel
send/recv/select/close, recorded as a write-conflict on the sync object's identity
(`dstSyncAcquire`); a `sync/atomic` operation, recorded on the operand's address and real byte
width — write-conflict, or read-conflict for pure loads (`dstAtomicYield`, emitted at instrumented
call sites; see "Atomics and len(ch) are recorded transitions" under the Completeness boundary);
or a `len(ch)` observation, recorded as a read-conflict on the channel identity
(`dstSyncObserve`) — *in addition to* the
existing coarse boundaries (block/select/`Gosched`/create), which remain. At a boundary the active strategy
may switch goroutines, so the scheduler can interleave at the grain of a single access.

The sync-object decision boundary (b) is **load-bearing for completeness, not just granularity**: *which*
contending goroutine acquires/releases/closes a sync object first is a real scheduling choice that can
change the outcome, but it is decided at a transition that performs *no memory access* (a goroutine
reaching its `Lock`, `TryLock`, `Unlock`, or `close` records nothing), so without (b) DPOR treats that
decision as independent of everything and silently drops the alternative sync-decision-order Mazurkiewicz
classes (a DST-L2-3 violation — `TestDSTExploreSweep` fails 23/290 with `dstSyncAcquire` neutered).
Announced *before* the state decision/transition and modeled as a write-conflict (same-object sync
decisions do not commute), two decisions on the same object by different goroutines become a co-enabled,
concurrent, conflicting pair whose **both** orderings the existing HB-DPOR explores — with no change to the
dependency/race test. This is the standard DPOR treatment of locks, extended to non-blocking decisions and
their release/close counterparts; it is faithful (`executions ⊆ real`: the real scheduler can switch before
any goroutine changes the sync state) and sound (a pre-decision yield never runs a blocked G).

- **Where.** The `-race` hooks are the access-observation choke point. They are NOSPLIT ABIInternal
  assembly, deliberately wrapper-free to preserve caller-PC capture for reports (`race_*.s`), so they
  cannot host a splittable yield (`goyield` calls `mcall`). The yield is therefore a **Go-level hook**.
- **Auto-instrumentation: a dst-race compiler mode (DECIDED — Option 1, "separate yield call, oracle
  untouched").** Under `-tags dst` **and** `-race`, the compiler's instrument pass
  (`cmd/compile/internal/ssagen` `instrument2`) emits an **additional** call immediately **before** each
  existing race hook: `runtime.dstAccessYield(addr, isWrite)` before scalar `race{read,write}`, and
  `runtime.dstAccessYieldRange(addr, size, isWrite)` before composite `race{read,write}range`. It does
  **not** replace or reroute the race hook. The DST hook records the access byte interval and write bit and
  makes the guarded yield decision; the unchanged race hook then records in TSan exactly as upstream,
  reading its own return address off `(SP)`, so **report PC attribution and detection behavior are
  byte-identical to a stock `-race` build**. The mode is gated by a compiler flag `cmd/go` sets when both
  the `dst` tag and `-race` are present; with the flag off, `instrument2` emits the plain `race*` hooks
  exactly as upstream (DST-L2-4). Two correctness properties this buys over rerouting the race symbol
  (the rejected Option 2): **(i)** the TSan oracle is never modified — perfect attribution, identical
  detection; **(ii)** `raceread` stays `NOSPLIT`, so instrumented `//go:nosplit` functions still link —
  the *new* splittable `dstAccessYield` is simply **not emitted in nosplit functions** (the compiler
  checks `fn.Pragma&Nosplit`; skipping a yield point is always sound). `dstAccessYield` is DST's own
  transition hook — it is *not* TSan's — so the DPOR access stream rides it, not a hijacked detector
  hook. (Rejected Option 2 — replace `raceread` with a Go shim that yields then forwards to `racereadpc`
  — would make `raceread` splittable, breaking nosplit instrumented callers, and reroute every access
  through a different TSan entry with hand-derived PC bookkeeping across six arch asm files: more
  fragile under rebase and modifies the oracle. We do not optimize for upstreamability — this fork
  rebases continuously — but Option 1 is chosen on *correctness*, not upstreamability: untouched oracle +
  nosplit-safe.)
- **The yield primitive.** `goyield`-shaped: requeue the current G on the local runq and `schedule()` →
  `findRunnable` → `dstFindRunnable` (the existing seam). The current G stays runnable, so the seam never
  runs a blocked G — soundness is inherited from Seq 5 unchanged.
- **The safe-point guard** (DST-L2-1): yield only when `dstActive() && dstSchedKind == dstSchedScheduled
  && g.bubble != nil && g == g.m.curg && g.m.locks == 0` — the strategy condition confines access
  yields to Explore's scheduled strategy, so Random/PCT runs are byte-for-byte unaffected. (No separate
  reentrancy guard is needed: the hook calls only uninstrumented runtime code, so it cannot re-enter
  itself.) Any failure → record the access but do not yield (sound; reduces completeness only).
- **Shared-address filtering (tractability + faithfulness).** A yield is meaningful only at a byte interval
  ≥2 goroutines access — a private/stack/single-owner access is independent (Mazurkiewicz), and yielding
  there explores nothing new while multiplying transitions. An access is a transition iff it *conflicts*
  with a prior access by a different goroutine (overlapping byte intervals, ≥1 write) that is **not**
  happens-before-ordered before it (D2). Single-owner and HB-ordered accesses record but do not yield. This is the
  primary control on the access-granularity explosion; its magnitude is measured in increment 1. **[V]**
  Runtime implementation: under `-tags dst -race`, the DST access hooks maintain a preallocated live HB clock,
  live sync-object clocks for the same release/acquire events recorded offline, and a per-interval /
  per-goroutine epoch table. A memory access that has no prior concurrent conflicting access records into
  the per-bubble access log inline and does not call `goyield`; a conflicting access still yields and is
  logged when the goroutine is resumed, preserving commit order. Because a prior-only
  filter cannot know that a later access will need a split inside the same inline interval, the brain
   promotes observed unsafe inline accesses to forced replay yield points keyed by `(dstSeq, hook ordinal,
   hook PC key)`, where the PC key is function-name + offset based rather than an absolute address, and
   restarts the pass until no new promotion is needed. Auto-instrumented accesses to the
  current goroutine's stack log as `addr=0` (private; no conflict identity), while explicit manual/sync
  identities keep their addresses. If the bounded live filter state overflows, the runtime conservatively
  yields every later access (less pruning, never a dropped class). Non-race manual hooks remain explicit
  transition boundaries for the hand-controlled DPOR brain-validation corpus.

#### D2 — Happens-before tracking (the dependency relation)

DPOR's dependency relation needs, for any two accesses, whether they are causally ordered. The runtime
**records** the synchronization events it owns into the per-bubble transition log: ready/create edges
(`goready`/goroutine creation, the non-foreclosure choke points) and, in `-tags dst -race` builds, explicit
sync release/acquire events for real memory-model edges such as mutex `Unlock`→later successful
`Lock`/`TryLock`, channel send→receive / unbuffered receive→send-completion, and
close→closed-receive. The DPOR engine **computes the vector clocks / HB relation offline**, between Runs.
Ready edges and sync events also record the access-log length and a shared HB-event order at the moment
they fired, so same-step events are ordered against inline filtered accesses in the same interval without
conflating sync-object *decision conflicts* with synchronization HB. Sync events are replayed as object
clocks, so an acquire observes the release-time snapshot rather than the releasing goroutine's later
accesses. Buffered channel events key the object by channel plus ring slot, not by element address, so
zero-sized element slots remain distinct HB objects. The full dependency relation is still computed
offline between Runs: two conflicting accesses are **dependent** iff neither clock dominates the other
(concurrent), and HB-ordered pairs are pruned (their order is fixed and sound). The bounded live clocks are
only the conservative access-yield filter; DPOR source sets and replay decisions use the post-Run clocks.
The recorded events are the same ones the scheduler/sync primitives control, so the HB is self-contained
(no dependence on TSan's C-internal clocks); it must agree with `-race`'s own HB to remain the faithful
oracle, which the conflict-set cross-check against `-race` reports validates; the recorders also honor
`raceignore` at their common choke point, mirroring the race detector's ignore state (see the sync-hook
mechanism passage). Timer-fire wakeups in synctest fake time are
   validated by `TestDSTExploreTimerHB`: two goroutines sleep until the same virtual time and then race on a
   shared variable; Exhaustive reaches both read outcomes (`timerhb exh=12`), and DPOR matches them while
   exhausted (`timerhb dpor=3`, two outcomes). So the currently-recorded timer wake edges do not over-order
   that reachable timer-gated conflict shape.

#### D3 — Stateless DPOR as a `dstSchedSelect` strategy

DST already **re-executes** `simulation.Run(seed, f)` from the start per run, so **stateless** DPOR fits
exactly: each schedule is one re-execution guided by a thread-choice prefix + backtrack/sleep sets, no
stored global states.

- **Schedule representation.** A prefix `[]goid` of thread choices, plus per-decision backtrack and sleep
  sets. Goids are deterministic given a fixed prefix (per-g tree + deterministic creation order), so a
  prefix replays exactly; the suffix runs the strategy default (lowest-goid, deterministic).
- **At the seam.** `dstSchedSelect` gains a `dstSchedDPOR` branch: if the decision index is within the
  prefix, return the candidate index whose goid matches the prescribed choice; else pick the default and
  let the post-run analysis add backtracks. Installed per bubble at the synctest re-root point (the
  `dstSchedRootPCT()` slot, `synctest.go`), like every other per-bubble scheduling state.
- **Post-run analysis (the DPOR core — source-DPOR).** After each Run, walk the recorded transition
  trace; for every reversible race — a *concurrent* (sync-HB) dependent pair `(t_i, t_j)`, `i<j` — add a
  backtrack point at the pre-`t_i` decision. The backtrack is a **weak-initial** of the race witness
  (`addSourceBacktrack`), computed over the **trace happens-before** (`dporTraceClocks`), NOT `t_j`'s
  thread directly: `t_j`'s thread may be asleep at `t_i`, and a weak-initial is by construction not
  sleep-blocked for the reversal. **Sleep sets** carry each frame's already-explored / inherited threads
  (filtered by independence with the chosen transition) and skip an asleep backtrack choice, removing the
  equivalence-class re-exploration a plain backtrack set incurs. This is **source-DPOR** (Abdulla et al.
  2014): complete (no Mazurkiewicz class dropped — `TestDSTExploreSweep`) and a strong reduction toward
  one schedule per class (not the fully-optimal wakeup-tree variant, which is left unforeclosed).
- **Soundness + determinism inherited:** every decision still picks an enabled (runnable) thread
  (DST-L2-1); each Run is fully determined by its schedule (DST-L2-2).

#### D4 — The `Explore` outer loop + public API

The driver lives above the seam, orchestrating repeated Runs:

- `simulation.Explore(seed uint64, mode ExploreMode, f func() bool) ExploreResult` — runs the selected
  worklist (Exhaustive or DPOR): pop a schedule, execute `f` under it, analyze the trace, push new
  schedules, until the worklist is empty (the pruned interleaving space is **exhausted**) or a budget is
  hit. `simulation.ExploreWith(seed, ExploreOptions{Mode, MaxSchedules, MaxSteps}, f)` is the budgeted
  form. `ExploreResult` carries schedules/exhausted/overflow/budget-hit + every found failure (a `-race`
  report, a SUT assertion, a SUT panic, or a synctest deadlock) with replay metadata: the observing schedule
  plus any forced access-yield watchpoints active when the failure was observed. Top-level SUT callback
  panics are recovered in the Explore driver; unrecovered panics from SUT-created bubble goroutines or
  finalizer/cleanup callbacks on the DST drain are recorded by the runtime after the panicking goroutine's
  defers run, then that goroutine exits. Scheduled
  synctest deadlocks are recorded as `Failure.Deadlock` inside `synctestRun` before it returns, avoiding the
  unsafe outer-recover path while leaving the blocked goroutines isolated in their deadlocked bubble.
- `simulation.Replay(seed, failure, f)` replays one schedule for reproduction/debug using a `Failure`'s
  recorded schedule plus any forced access-yield watchpoints. `RunWith` remains the Random/PCT control
  surface and rejects unknown strategies instead of silently falling back to Random.
- **No silent cap (No silent downscoping):** if a budget truncates exploration, `Report` says so and how
  much was covered — "exhausted" and "budget-hit" are distinct verdicts; the latter is never reported as
  the former.

#### D5 — The race detector as deterministic oracle (**IMPLEMENTED [V]**)

Each explored interleaving runs under `-race`. The HB detector fires for an unsynchronized access pair
even at `GOMAXPROCS=1` serial execution (it is clock-based, not timing-based), and the report is a
deterministic function of the seed/schedule (same seed → 100/100 identical normalized report; even a
   *conditional* race's detect/no-detect verdict is stable per seed, 0/30 vs 30/30 — no flicker). **[V]** So
   a race found in an explored interleaving is a stable observation; exact public replay uses the schedule
   plus the access-force set recorded on the failure. Atomicity violations (invisible to `-race`) are caught
   by the SUT's own assertions in the same explored interleaving.

Wired into `Explore`: `runOnce` reads `runtime.RaceErrors()` (via the build-tagged
`dstRaceErrors` — real under `-race`, 0 otherwise) before and after each scheduled Run; each NEW race
count increment appends one `Failure` with `Race=true`. The detector dedups by signature, so each distinct
race yields exactly one `Race` failure — the first schedule that exhibits it, including any access-force set
active in that pass so `simulation.Replay` can reproduce it in a fresh process even if later passes do not
re-report due to TSan dedup. Enforced by `TestDSTExploreRaceOracle`: an
unconditional write-write race is reported (the oracle fires under `simulation.Run`), and an
*interleaving-conditional* race — manifesting only when the reader acquires a mutex first (`raceCondSUT`) —
 is found by exploring both acquisition orders and reported with a non-trivial schedule, both deterministic
 across same-seed runs. `TestDSTExploreRaceReplay` covers a race first observed under replay-promoted
 access forces and replays the returned schedule+force token in a fresh process. A non-`-race` build still
 enumerates interleavings and reports SUT-assertion failures; it records no data-race failures.

### Soundness argument (load-bearing collapse-check)

The seam still only ever reorders goroutines that are *already runnable* — an access yield merely
requeues the running G and reschedules, so every candidate is past the primitive that made it runnable,
exactly as in Seq 5. The new freedom is *when* a SUT goroutine yields (now also at shared-memory
accesses), and that freedom is real: on a real multi-core machine the OS can deschedule a goroutine
between any two instructions, so any reordering of access-separated spans is an execution the real
runtime+OS can produce. The guard forecloses precisely the points the real runtime cannot take a switch
at (lock held, on g0, non-bubble), and those are *also* the points the compiler never instruments — so
the structural exclusion and the guard agree. Therefore executions ⊆ real (top-tier Soundness), and the
seam never pulls from a wait queue, so a causally-ordered pair is unrepresentable as a reorder candidate
— soundness is structural, not a runtime check.

### Increment sequence (each useful; none forecloses another)

The ordering key: **every piece hooks to the existing `dstFindRunnable` seam or to "an access happened,"
never to a new control shape** — so the strategy (D3), the outer loop (D4), and the auto-instrumentation
(D1) compose without retrofit. **Build order (DECIDED — sequencing (b)): prove the DPOR brain (2–4) on
the *manual* hook first, then switch the transition source to auto-instrumentation (1's compiler half).**
The DPOR algorithm is the hard, research-grade core; validating it is cleaner when the transition set is
hand-controlled (and checkable against brute-force) rather than fed by every auto-inserted access.
Auto-instrumentation then only changes *where* transitions come from, not the algorithm — non-foreclosing
by the ordering key. (The `cmd/compile`/`cmd/go` work is therefore deferred until the brain is proven.)

1. **Access-yield + transition-record substrate.** Runtime `dstYieldPoint`/`dstAccessYield` + the
   safe-point guard + a per-bubble **transition recorder** (an ordered event log: scheduling decisions
   with the enabled goid set, accesses with goid/addr/size/isWrite/step, and sync events for offline HB —
   D2). **Manual-hook half: VALIDATED [V]** — mutex-counter soundness probe reaches exactly `G·K` at
   access granularity incl. yields while holding a user lock (DST-L2-1; 200/200 over 50 seeds, normal and
   `-race`, 0 spurious races), replay deterministic (DST-L2-2; 30/30 per seed), Gap A closed (110/200),
   per-run yield magnitude measured before filtering. **Compiler half
   (Option 1): IMPLEMENTED [V]** — `cmd/compile` `instrument2` (ssagen) emits an additional
   `runtime.dstAccessYield(addr, isWrite)` immediately before scalar `race{read,write}` hooks and
   `runtime.dstAccessYieldRange(addr, size, isWrite)` before composite `race{read,write}range` hooks, gated
   by the `-d=dstrace=1` debug flag that `cmd/go` sets exactly when `-tags dst` **and** `-race` are both
   present; the race hook itself is untouched (oracle byte-identical), the yield is skipped in
   `//go:nosplit` functions (`goyield` is splittable; skipping is sound), and with the flag off the pass
   emits plain `race*` with no DST access-yield call (DST-L2-4 — verified absent in non-dst and
   dst-without-race builds). An UNMODIFIED SUT (no manual hooks) built `-tags dst -race` is then explored end-to-end
   (`TestDSTExploreAutoInstrument`: the lost update is found, DPOR outcome set == Exhaustive). Foreclosure:
   feeds the same seam. **Runtime sync-primitive hooks for `dstSyncAcquire`: IMPLEMENTED [V]** — channel
   ops (`chan.go` `chansend`/`chanrecv` before `lock(&c.lock)`, `closechan` before the closed-state
   transition, identity = the `*hchan`) and mutex/RWMutex state decisions (`internal/sync.Mutex`
   `Lock`/`TryLock`/`Unlock`, identity = the mutex pointer; `sync.RWMutex` reader/writer admission and
   release, identity = the embedded writer mutex) auto-announce a `dstSyncAcquire` write-conflict. RWMutex
   suppresses the embedded writer mutex's DST happens-before events while executing RWMutex internals and
   records public RWMutex HB separately on the same `readerSem`/`writerSem` identities used by the race
   detector, so failed public `TryLock`/`TryRLock` decisions do not synchronize. The suppression
   mechanism is `g.raceignore`: the HB-record bridges (not the decision-announce bridges) early-return
   when it is non-zero, exactly as `raceacquireg`/`racereleaseg` do, so upstream's existing
   `race.Disable()` brackets around the `rw.w` operations suppress the embedded mutex's HB with no
   DST-specific call variants — and the public RWMutex HB hooks sit before `race.Disable()`/after
   `race.Enable()`, exactly where their race-annotation twins sit. The check lives at the single
   choke point every HB recorder funnels through (`dstRecordSyncEventForGID` — the sync-package
   bridges, the chan.go/select.go records including their g-credited sites, and `dstAtomicYield`'s HB
   contribution), keyed to the EXECUTING goroutine's `raceignore` exactly as every race.go
   acquire/release variant is (the g-credited `raceacquireg`/`racereleaseg` forms also check
   `getg().raceignore`), so the whole HB shadow honors the same ignore state as the race detector
   it must agree with; decision announces and ready/create edges do not flow through that funnel
   and are unaffected. Enforced by `TestDSTSyncHBRaceIgnore` on the recorded event stream itself
   (outcome-based tests cannot see HB records — they only prune): RaceDisable-bracketed mutex,
   channel, and atomic ops record nothing; RWMutex ops record only the public sem events; the
   contended-Lock and TryLock-success record sites are asserted per call site. The mechanism is also load-bearing for DST-L2-4's
   untagged shape: the hook lines fold away in-place inside upstream's method bodies, adding no wrapper
   layer between a caller and `lockSlow`/`unlockSlow` — the trace/pprof semaphore skip constants count
   inline frames, so any indirection (a parameterized `lock(hb)` body, or a `NoDstHB` method variant at
   a call site) shifts every untagged contention/trace stack one frame too deep and renames RWMutex
   profile symbols (`TestTraceStacks`, `TestBlockProfile`, and `TestMutexProfile` enforce the upstream
   shape untagged via the `test:inert-std` leg). An
   UNMODIFIED mutex/channel program's sync-object decision order is therefore explored with no hand
   annotation. The mutex hook is in `Lock` (NOT the `semacquire` slow path): `semacquire` is reached only by
   the *contended loser*, after the uncontended winner already took the fast-path CAS, so it is too late to
   record the winner's acquisition as a conflict — only a pre-CAS announce on the path *every* acquirer
   takes makes both orders a co-enabled conflicting pair. `TryLock`/`TryRLock` announce before failed-state
   rejection; `Unlock`/`RUnlock`/`closechan` announce before state transitions that can flip racing
   non-blocking decisions. Gated by the SAME `dstBuild && raceenabled` / `//go:build dst && race` condition
   as the memory auto-instrumentation (so non-dst, plain-`-race`, and dst-without-race builds are
   hook-inert — DST-L2-4), and confined to the scheduled strategy by the runtime guard. Acceptance:
   `TestDSTExploreSyncAutoInstrument` — unmodified mutex/channel/RWMutex/select/close/Once SUTs each reach
   BOTH sync-decision outcomes under DPOR (vs 1 with the corresponding hook neutered). *Coverage:* this
   covers `sync.Mutex.Lock`/`TryLock`/`Unlock`, `sync.RWMutex.Lock` (decision transitively via `rw.w`),
   `sync.RWMutex.RLock`/`TryRLock`/`RUnlock`/`Unlock`, blocking and non-blocking `chansend`/`chanrecv`,
   `closechan`, blocking and non-blocking `selectgo` channel send/recv paths, and `sync.Once`'s first
   execution path (transitively through its internal `Mutex`). Shared-address filtering for the
   auto-instrumentation explosion is implemented in increment 6 and enforced by
   `TestDSTExploreAutoInstrument`.
2. **Happens-before pruning — recorded events, computed offline** (D2; **VALIDATED [V]**). The runtime
   records ready/create edges (readier happens-before readied) and, in `-tags dst -race` builds, explicit
   sync release/acquire events into pre-sized per-bubble buffers under the scheduled strategy only
   (allocation-free, gated so Random/PCT/non-dst/dst-without-race are unaffected); the DPOR engine builds vector clocks **offline** from those events +
   program order (`dporClocks`/`dporConcurrent` in `explore.go`) and refines the dependency to *concurrent*
   conflicting pairs only. Mutex/channel-serialized conflicts are pruned, including non-waking edges such
   as uncontended mutex `Unlock`→`Lock`, buffered channel slot send→receive, unbuffered rendezvous, and
   close→closed-receive. Measured on `twoPairSUT`
   (two channel-ordered producer/consumer pairs interleaving freely): exhaustive 4032, address-only DPOR
   21, **HB-DPOR 4** — all 0 failures (`TestDSTExploreHBPrunes`, mutation-verified: the `<=10` bound fails
   at 21 when the concurrency test is disabled). Synthetic clock tests validate release/acquire object
   clocks, release-time snapshots, and distinct channel-slot identities. `TestExploreRecordsBufferedChannelHB`,
   `TestExploreRecordsBufferedChannelZeroSizeSlotIDs`, `TestExploreRecordsFullBufferedChannelSenderRelease`,
   `TestExploreRecordsSelectBufferedChannelHB`, `TestExploreRecordsUnbufferedChannelHB`,
   `TestExploreRecordsChannelCloseHB`, and `TestExploreRecordsMutexHB` validate the channel and mutex runtime
   hook paths under `-tags dst -race`. On pure-race SUTs (atomicity/counter, no synchronized
   accesses) HB correctly finds the conflicts concurrent and changes nothing — completeness preserved
   (`TestDSTExploreComplete` still green). Full DPOR HB remains offline; the live clocks only suppress
   access yields using the same release/acquire semantics as the recorded sync events. Foreclosure: additive
   recording.
3. **DPOR strategy** (D3; **VALIDATED [V]**). Iterative stateless DPOR over the schedule recorder
   (`dporExplore` in `testing/simulation/explore.go`): dependency = overlapping nonzero memory byte
   intervals or the same sync-object identity, ≥1 write, different stable index (`g.dstSeq`); backtrack
   points added at ancestor decisions; deterministic backtrack pick. Measured against the Exhaustive
   baseline: **sound** (mutex-counter 0 failures over the
   whole space — `TestDSTExploreFindsAtomicityViolation`), **complete** (counter race: DPOR and
   Exhaustive reach the *identical* outcome set — DST-L2-3, `TestDSTExploreComplete`: 2-goroutine
   `{1,2}` committed; 3-goroutine `{1,2,3}` measured in bring-up), **deterministic + seed-independent**,
   and a real reduction (atomicity 180→10; 2-goroutine counter 180→10; 3-goroutine counter 20160→539).
   Foreclosure: a strategy at the seam.
4. **`Explore` outer loop + API** (D4; **VALIDATED [V]**). `simulation.Explore(seed, mode, sut)` drives
   repeated bubble re-executions; `runOnce` follows a prefix and copies out the trace. Exhaustive and
   DPOR modes share the loop. Reports `Schedules`/`Failures`/`Exhausted`/`Overflow`/`BudgetHit`
   (exhausted vs budget-hit distinct — no silent cap), top-level and child-goroutine SUT panics as
   `Failure.Panic`, synctest deadlocks as `Failure.Deadlock`, and one `Failure.Race` per new `RaceErrors`
   increment. The scheduled post-`go` boundary is active in non-race builds too, so assertion-only
   child-before-parent-continuation failures are not silently skipped. Child-goroutine and drain-callback
   panics are recorded in the runtime after ordinary defers; scheduled deadlocks are converted inside
   `synctestRun` before `Run` returns.
   *(After 4: increment 2's HB pruning + increment 6's filtering
   cut the still-inflated counts; then 1's compiler half so real SUTs need no hand-annotation.)*

**As built so far (this session) — the substrate + brain are proven on the manual hook:** the stable
per-bubble index (`g.dstSeq`, lazily assigned at first candidacy — goid is process-global and drifts
across re-executions, so it cannot key a replayable schedule), the allocation-free recorder
(`dstScheduledSelect` runs on g0 under `sched.lock`), the transition-boundary hooks (`dstAccessYield` for
memory accesses, `dstSyncAcquire` for synchronization object decisions — D1), and the Exhaustive + DPOR
engines. Full landed DST suite stays green (`ok runtime`, normal and the scheduling subset). The remaining
trace entries are real conflict/coarse decisions after increment 6 filtering; soundness + completeness for
SUTs whose accesses and sync-object decisions are recorded
is independent of that inflation, and enforced over a generated family by `TestDSTExploreSweep`.

**Completeness boundary (addr=0 transitions).** DPOR's dependency relation pairs transitions that record
a nonzero conflict identity: a shared **memory access** (`dstAccessYield`) **or** a **synchronization object
decision** (`dstSyncAcquire`, recording the sync object's identity as a write-conflict — D1).
Transitions that record none — pure infrastructure decisions (goroutine creation, `WaitGroup` wakeups,
the isolated `gcDrain` finalizer goroutine) — remain independent of everything, which is correct because
they carry no outcome-determining order choice a recorded access/sync-object decision does not already capture
(the created goroutine's own accesses, the post-`Wait` accesses, … are the recorded transitions). So the
relation is sound and complete *for SUTs whose shared memory accesses **and** synchronization
object decisions are recorded as transitions, and that do not observe finalizer/cleanup timing* — enforced
over a generated family (mutex- and channel-decision-order cases included) by the
`TestDSTExploreSweep` equivalence sweep (DPOR outcome set == exhaustive, 802 SUTs).

**Atomics and len(ch) are recorded transitions (former named exclusions — closed).** `sync/atomic`
operations are decision points: the dst-race compiler mode emits `dstAtomicYield(addr, width, kind)`
immediately before each static `sync/atomic` call in instrumented code — the atomic implementations
are NOSPLIT race assembly that cannot host a yield, so this is the D1 Option-1 shape (an ADDITIONAL
call, TSan untouched) applied at the call-site choke point. Both forms are classified: the free
functions and the typed-API methods — the latter are load-bearing because `sync/atomic` is a
noRaceFunc package whose functions are NOT inlinable under `-race`, so a typed call stays out of
line and only the instrumented call site can announce it (the typed structs' zero-size
noCopy/align64 prefixes put the value at offset 0, so the receiver address IS the atomic's
address). Loads announce as read-conflicts (load pairs commute); stores/RMWs/CAS as write-conflicts
on the operand's real byte width, so atomics also pair with PLAIN accesses of the same memory.
After the yield returns — the op commits next, with no further yield — the hook records the op's
happens-before contribution for offline-DPOR pruning and the live filter clocks: Load acquire,
Store release, RMW acquire+release (acquire first, so RMW chains stay transitively ordered), and
**CAS acquire-only** — it always observes but may write nothing, and the hook runs before the
outcome is known; the missed release of a successful CAS only forgoes pruning. The *effective*
release semantics are cumulative, not the literal per-op observed-by edge: release clocks MERGE
on the object (a load after two stores acquires both stores' history, though it observed only the
last — a deliberate over-approximation on the load side; TSan's own AtomicStore overwrites rather
than merges, so this is strictly more ordered than TSan for W→W→R), while the storer side
deliberately under-approximates (a pure Store records no acquire). Where
this merged model claims an edge the strict observed-by reading would not (the W→W→R pattern, or
a failed CAS if it over-claimed a release), the explorer's architecture masks the difference
rather than losing a class: same-address atomic announces always form a conflicting pair, so the
release/acquire ops are always reorderable, and the per-trace re-analysis of the reordered trace
drops the claimed edge — verified by an outcome-equivalent over-claim mutant across the sweep
(including the two-variable A5 family built to defeat it) and a crafted probe. CAS stays
acquire-only as the faithful floor so no MORE of the model rests on that masking than the merge
semantics already do; the enforced contract is the sweep's DPOR == Exhaustive equivalence.
`len(ch)` announces the channel identity as a READ-conflict in `chanlen` (`dstSyncObserve`),
pairing with the ops' write-conflict announces; `cap(ch)` is NOT hooked — capacity is immutable
after `make`, so a cap read carries no ordering decision (the earlier draft of this boundary named
cap as an exclusion to close; it is a non-decision, not an exclusion). Remaining boundary — atomic call forms that record NO transition: (1) calls from inside a
non-instrumented package (the runtime's own atomics; a norace package's internal call sites when
not inlined into instrumented code) — the sync packages' primitives carry their own decision
hooks; (2) DYNAMIC call forms, where the callee is not a static symbol the emission can classify:
interface dispatch on an atomic value, method values (`f := x.Load; f()` — the generated `-fm`
wrapper lives in the noRaceFunc sync/atomic package), and func-valued free functions
(`f := atomic.LoadInt32`); (3) instrumented `//go:nosplit` callers (the yield is splittable;
skipping is sound but unexplored) and embedded-promotion tail-call wrappers. A SUT whose
outcome-determining atomic is reached ONLY through these forms under-explores exactly as the old
exclusion did; the direct static forms — overwhelmingly the way atomics are written — are covered. Enforced by the atomic/mixed/multi-way
sweep families (DPOR == Exhaustive, including the conservative-HB pruning) and
`TestDSTExploreAtomicAutoInstrument` (unmodified SUTs: CAS winner, store/swap order, add-vs-load,
And/Or, 64-bit and pointer widths, typed-API wrappers, len-vs-send — each Outcomes==2,
Exhausted==true).

An **earlier draft of this note was wrong**: it claimed completeness for "SUTs whose shared *accesses* are
all annotated," overlooking that **synchronization object decision order is itself a dependency**. A
mutex-bracketed program with every access annotated still lost a class (`prog#257`: exhaustive 2
outcomes, DPOR 1) because the lock-order-determining decision is an `addr=0` transition. `dstSyncAcquire`
(D1) closes it with no change to the dependency/race test; the sweep enforces it (23/290 → 0). For the
manual-hook validation phase the SUT annotates sync-object decisions; the dst-race compiler/runtime phase
records them automatically — **now implemented** (increment 1): channel ops, close, and mutex/RWMutex
state decisions auto-announce `dstSyncAcquire` under `-tags dst -race`, so an unmodified mutex/channel SUT's
decision order is explored (`TestDSTExploreSyncAutoInstrument`). The mutex hook is
in `Lock` before the CAS, in `TryLock` before even the locked-state rejection, and in `Unlock` before the
release that can flip a racing `TryLock`, not the `semacquire` slow path (which the uncontended fast-path
winner never reaches — too late to record its acquisition as a conflict). `RWMutex.RLock`/`TryRLock` announce
on the same writer-mutex identity as `RWMutex.Lock`, including failed `TryRLock` admission attempts;
`RWMutex.Unlock`/`RUnlock` announce the same identity before release transitions that can flip racing try
attempts. Blocking and non-blocking select channel cases announce each candidate channel before taking
channel locks, `closechan` announces before the closed-state transition that can flip a non-blocking receive,
and `Once` is covered by its internal mutex.
`TestDSTExploreSyncAutoInstrument` now covers mutex, channel, RWMutex reader-vs-writer, TryLock,
failed TryLock/TryRLock decisions, release-vs-try decisions, blocking and non-blocking select send/recv,
close-vs-receive decisions, and Once winner order under `-tags dst -race`.
Finalizer-timing observation
stays out of scope until *every* access is recorded; the `dporExplore` dependency loop documents the
relation.
5. **Source-DPOR — sleep sets + weak-initial source sets** (D3) — **VALIDATED [V].** The former
   `dporExplore` was a *persistent-set* DPOR: sound + complete, but it re-explored Mazurkiewicz-*equivalent*
   interleavings via different prefixes (the per-frame `done` set precludes exact-duplicate prefixes, so
   the residual redundancy is equivalence-class). Source-DPOR (Abdulla, Aronis, Jonsson & Sagonas, POPL
   2014) removes it, tightening DST-L2-3 from "covers every class" toward "explores no class twice".
   Foreclosure: refines the worklist, not the seam. **This was the riskiest remaining algorithmic piece** —
   a wrong sleep set silently drops a class (DPOR misses a reachable bug while still reporting
   `Exhausted=true`); the micro-SUT completeness tests are a weak net for it. The generated-family sweep
   (`TestDSTExploreSweep`, step 1) is the real net, and it earned its keep twice during this build (see
   step 2). **Build order for this increment (done in this order):**
   1. **Validator first** (so the safety net exists before the algorithm): a brute-force-equivalence
      sweep — a *generated family* of small SUTs (vary goroutine count, accesses, sync) — asserting the
      DPOR explored *outcome set* equals `exhaustiveExplore`'s for every member, not just the committed
      micro-SUTs. This is the real DST-L2-3 guard. **VALIDATED [V] — and it immediately earned its keep:**
      it exposed a *pre-existing* DST-L2-3 completeness defect (the persistent-set DPOR dropped every
      **synchronization-object-decision-order** class in the sweep — 23 SUTs of the then-290, all-and-only
      mutex/channel cases), because the lock/rendezvous-order decision is an `addr=0` transition the dependency relation
      ignored. **Fixed before sleep sets** (a reduction layered on an incomplete search would drop even
      more): `dstSyncAcquire` (D1) records sync-object decisions as conflicting transitions — zero brain change —
      and the sweep (`TestDSTExploreSweep`) is now the enforcing artifact (23/290 → 0). Optimality (sleep
      sets) is therefore built on a foundation the sweep proves complete.
   2. **Source-DPOR (sleep sets + weak-initial backtracks) — VALIDATED [V].** Each `dporFrame` gains a
      `sleep` set: a frame inherits the parent's asleep + already-explored goroutines, FILTERED by
      independence with the transition the parent chose (a commuting goroutine stays asleep, a dependent
      one is woken), threaded through the `for d := len(stack); d < n` extension loop; an asleep backtrack
      choice is skipped. Sleep alone is INCOMPLETE with a crude backtrack rule — **the sweep caught it**
      (a write between two independent reads dropped a class, prog#273). The fix is **source sets**: when a
      reversible race (concurrent dependent pair e_i, e_j) is found, add a **weak-initial** of the witness
      to `backtrack(i)` (`addSourceBacktrack`) rather than e_j's process directly, so the reversal survives
      sleep-pruning. The witness/weak-initials require the **TRACE happens-before** (`dporTraceClocks` —
      transitive closure of the dependency relation, ordering conflicting accesses), NOT the sync-only HB
      (`dporClocks`): **the sweep caught the sync-HB version too** (an independent read became a spurious
      weak-initial; prog#274/276). With trace-HB the sweep is complete. Two relations coexist by design:
      the reorderability GATE uses sync-HB (`dporConcurrent` — a conflicting pair with no sync between them
      is reorderable); the weak-initial/`notdep` uses trace-HB. Sleep independence is deliberately
      coarser than the dependency relation: sleep-set entries are carried across stateless
      re-executions, but raw access addresses are run-local (the same logical object can receive a
      different numeric address after explorer-side allocations). So `addr=0` transitions and read/read
      pairs commute; any nonzero pair with at least one write wakes the sleeper. The race gate still uses
      per-run interval overlap + HB. This can only under-prune, never keep a dependent transition asleep
      and drop a class.
   3. **Adversarial review** (incompleteness failure mode + determinism/soundness) — run per the
      Adversarial loop on the change set.
   Measured (at the then-290-program standing sweep, mismatches=0, including the timer-gated HB SUT;
   the sweep now stands at 802 with the atomic families, still mismatches=0): source-DPOR vs
   the persistent-set baseline cuts the worst-program schedule count `maxDpor` 125→69; current totals are
   `totExh=13414`, `totDpor=1965`; a
   370-program run (incl. the timer-HB SUT, 3 goroutines × 2 ops, and 4-way contention; exhaustive up to
   2520/program), reproducible on demand via `DSTSWEEP=heavy`, is also complete (12.5× reduction,
   161254→12895). Payoff is
   modest on tiny SUTs (persistent-set is already near-optimal there), larger as independent transitions
   multiply. `TestDSTExploreSweep` enforces both completeness (mismatches=0) and the optimality bound
   (`maxDpor` < 80; persistent-set regresses to 125, dropping sleep to 85). When the source-set witness has
   no enabled weak-initial at a decision (every process that could lead to e_j is blocked until e_i runs),
   the reversed order is UNREACHABLE from that state and `addSourceBacktrack` adds no backtrack — skipping,
   not the all-enabled add (which under sleep sets could drop a class), and not a panic. The hand-annotated
   family sweep never reaches this path (`fallbacks=0`); the dst-race compiler's denser auto-instrumentation
   does (a read-modify-write whose read and write both yield), and `TestDSTExploreAutoInstrument` validates
   that skipping there stays complete (DPOR outcome set == Exhaustive). (An earlier draft wrongly asserted
   this path unreachable and panicked; auto-instrumentation showed it is reachable.)
6. **Infrastructure isolation + shared-address filtering** (D1, using D2) — **VALIDATED [V].** *gcDrain isolation* —
   **VALIDATED [V]**: the bubble's finalizer-drain goroutine is scheduled RNG-free as infrastructure
   under the scheduled strategy (`firstSystemG`), so it leaves the recorded schedule/DPOR search; cut the
   exhaustive count ~9× (e.g. counter exhaustive 180→20) with no change to DPOR (it already pruned
   gcDrain as addr=0) and no effect on Random/PCT (`TestDSTSchedSystemIsolation` green). *Shared-address
   filtering proper* (only contended, non-HB-ordered addresses are transitions) is now implemented for the
   dst-race auto-instrumented stream at the runtime hook: all accesses are logged, but single-owner/
   HB-ordered accesses do not yield. The live filter uses bounded preallocated clocks/tables and falls back
   to yield-every-access on overflow, so overflow can only under-prune. The brain promotes any observed
   conflicting inline access that needs a missing boundary to a forced yield on replay; race-enabled DPOR
   uses conservative conflict-anchor backtracking while disabling sleep sets, and the non-race sweep keeps
   full source-DPOR sleep-set pruning. Non-race manual hooks remain explicit transitions, so `TestDSTExploreSweep` continues to
   validate the hand-controlled DPOR brain (`mismatches=0`). Validation:
   `TestDSTExploreAutoInstrument` validates filtered auto-instrumented RMWs by checking DPOR==Exhaustive,
   preserving the known `{1,2}` outcome set, and keeping Exhaustive tractable (plain RMW: 19,448 before
   filtering → 49 after; private-noise RMW: 49 after, outcome set preserved). It also checks filtered
   R/W/R shapes with four outcomes (`rwrExh=1580`, `rwrDpor=51`, `manualRWRExh=159`, `manualRWRDpor=6`),
   range-vs-field identity (`rangeExh=24`, `rangeDpor=2`, two outcomes),
   and post-`go` / post-wake parent-write shapes (`createExh=4`, `createDpor=2`, `wakeExh=25`,
   `wakeDpor=10`) so first accesses after goroutine creation or `goready` wake-up do not hide
   child-before-parent classes.
   Foreclosure: narrows
   transitions; fewer yields is sound because skipped accesses are logged and only independent accesses are
   filtered.

### Open questions (resolved by measurement during the build, not pre-judged)

- **Explosion magnitude (RESOLVED).** Shared-address filtering reduces the auto-instrumented RMW exhaustive
  count from the measured ~19,448 baseline to 49 while preserving outcomes; a private-noise RMW is also
  49. `TestDSTExploreAutoInstrument` enforces both tractability (`<1000`) and outcome preservation.
- **HB source (DECIDED).** DST-side, computed **offline** from recorded sync events (not live hot-path
  clocks, not TSan's C-internal clocks) — it must match `-race`'s own HB, cross-checked against `-race`
  reports.
- **Budget policy when the space exceeds the budget (IMPLEMENTED).** `ExploreWith` reports partial
  coverage with `Schedules` plus `BudgetHit=true`; `Exhausted` is false — never a silent cap (DST-L2-3 /
  No silent downscoping).

