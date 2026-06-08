# Full determinism under `-race` ≡ a race-invariant GC trigger (per-cycle discovery)

**Lands:** when we pursue full determinism under `-race` (a completeness/confidence
goal), or when a SUT demonstrably needs deterministic *per-cycle* GC/finalizer-
discovery timing (asserts *which* GC cycle discovers/finalizes/clears a given
object, not just the total set). No SUT need is demonstrated today — this is
elective.

## Why this is the convergence point

"Full determinism under `-race`" and "rework GC determinism" are the **same root
cause**. `-race` does not change the *logical* execution (per-g RNG values,
scheduling decisions, control flow); it changes only the *physical* layer (redzone-
inflated object sizes, shifted addresses, perturbed timing). So the determinism map
splits cleanly:

| Layer | Basis | Under `-race` |
|---|---|---|
| scheduling, select, map, rand, computed values, replay | per-g RNG + single-thread (logical) | holds |
| GC set-level (`numGC`, total finalizer/weak set) | logical reachability + floored trigger | holds |
| **GC per-cycle timing** (*which* cycle discovers an object) | **physical heap bytes** | **breaks** |
| pointer addresses, `%p`, `uintptr` | ASLR + redzones (physical) | breaks — fundamentally out of scope |

So the *only* thing `-race` breaks that a SUT could legitimately want deterministic
is per-cycle GC timing, and it breaks it for exactly one reason: the GC trigger
fires on **physical heap bytes** (`heapLive`), which redzones shift by ±1 span. Fix
the trigger and you get full `-race` determinism. There is no separate "race
problem" and "GC problem."

**The scheduler-determinism fix (commit `d8f46779a6`) already removed *one*
physical/timing leak.** That bug — system (`g.bubble==nil`) goroutines consuming the
bubble's seeded scheduling RNG a timing-varying number of times — was the same shape
(a physical/timing quantity leaking into a seeded stream) and was *worse* under
`-race`. It is fixed (system goroutines scheduled RNG-free; invariant `rngDraws ==
decisions - sysScheds`, `TestDSTSchedSystemIsolation`).

The mapping phase (Phase 1, below) then **confirmed the GC byte-trigger is the sole
remaining *within-build* `-race` source — but refuted "plausibly the last leak"**: it
also found a *second*, independent **cross-build** leak of the same class (the map
hash key — see "Phase 1 — MAP RESULT"), now fixed. So "the GC byte-trigger is the last
one" was not a safe assumption; the map had to be drawn.

## Behaviour (the thing to fix)

*Which* GC cycle discovers a given object is byte-exact only in a fixed normal
build. The per-cycle split moves by ±1 span under:
- **`-race`/`-msan`**: redzone-inflated sizes + ASLR-dependent entry state shift the
  byte↔object mapping by a span. Measured (when a per-cycle probe existed): a
  **bimodal** split — two values run-to-run; `numGC` and the total set stayed stable.
- **Binary composition**: a heavier import set shifts the bubble's entry span-fill
  phase, moving the crossing near a span boundary (the same class as the scheduler
  bug; a bare `import "net"` exposes composition-fragile DST bugs that lean binaries
  hide — the testprog now imports `net`, which is good for surfacing them).

`numGC` and the total discovered set are unaffected (same number of crossings; only
which side of a boundary an object lands on flips).

## Root cause

The DST heap trigger (A.5, `mgc.go` `gcTrigger.test`, `gcTriggerHeap` branch under
`dstActive()`) fires when bubble-local growth `heapLive − dstHeapBase` reaches the
GOGC-relative target. It is **byte-based** and span-granular: `heapLive` advances at
span-refill boundaries, tested in `mallocgc`. A.5 makes it byte-exact in normal
builds by being bubble-local. But `dstHeapBase` subtraction only cancels the
*process* baseline; a perturbation *inside* the bubble's own byte accounting
(redzone-inflated sizes, or an entry span-fill phase that shifts the first crossing)
the subtraction cannot cancel, and it moves the crossing by a span.

## Plan — map, then trigger (go deep, no shortcuts)

### Phase 1 — MAP (rigorous derisk; do this first)

Confirm the GC byte-trigger is the *sole* remaining `-race`/composition
nondeterminism source — do not assume it. Use the **trace-hash localization** that
nailed the scheduler bug: add temporary diagnostic counters/rolling-hashes that
*separate* candidate observables, run the suite under `-race` **many** times (the
bug class is rare, ~1% per run — need hundreds of runs, in a *heavy* binary), and
find exactly which observable diverges. Candidate observables to hash independently:
the goroutine schedule (selection sequence), the per-g rand stream, the per-cycle
finalizer-discovery sequence (`finqueued − dstFinqBase` folded per in-bubble GC —
the `dstFinqSeq` probe was removed when the discovery test went set-level; re-add it
temporarily), the GC-trigger crossing points, weak-clear order. Outcome: a confirmed
map (GC trigger is the lone source) or a list of other sources to address first.

### Phase 1 — MAP RESULT (measured, 300–600 runs/build, net-heavy testprog)

Two corrections to the assumptions above; the map was *not* what was assumed.

1. **GC per-cycle discovery is the sole *within-build* `-race` source — confirmed.** Over 300
   runs/build, every logical observable (goroutine interleaving across 5 scenarios, select order,
   per-g `math/rand`) and every GC *set-level* observable (`numGC`, the full finalizer run-set
   count+sum, the weak-cleared set) is `distinct=1` in *both* normal and `-race` builds. Only the
   GC *per-cycle* observables diverge: the per-cycle finalizer-discovery sequence (`finq`), the
   per-cycle `heapMarked` crossing (`mark`), and per-cycle weak-clear timing — the last is
   *bit-identical* to `finq` every run (weak clearing rides the same sweep-pass crossing, not an
   independent source).
2. **Correction A — per-cycle is *not* byte-exact in a fixed normal build.** It is already
   non-deterministic in a *normal* build at scale (`finq` 3–4 distinct/300, `mark` 5–7); `-race`
   merely *amplifies* the same span-crossing wobble (4–8 distinct/300). The "byte-exact per-cycle in
   a fixed normal build" claim (design.md) was measured at 1/10 runs and does not survive 300. ⟹ the
   Phase-2 logical-allocation trigger fixes the normal-build and `-race` per-cycle determinism *at
   once* — one defect, not two. The set-level contract (DST-GC-1) holds in both builds (the `runCount`
   /`runSum`/`numGC` invariance above).
3. **Correction B — a *second*, independent source: the map hash key (now FIXED, Phase 2b).** Multi-
   group (`≥16`-element) map iteration order is **build/composition-dependent**: deterministic
   *within* a build (`distinct=1`/600) but different normal-vs-`-race`. Root-caused: the per-g RNG
   stream is byte-identical across builds (anchors before/after a map match; single-group `≤8` maps
   match), `useAeshash=true` in both — but the `-tags dst` AES hash key `aeskeysched` is the normal
   build's **shifted by exactly one word** under `-race` (`race[i]==normal[i-1]` across 6 words),
   because `alginit` fills it from `bootstrapRand` at a startup *stream position* that `-race` shifts
   by one extra draw. Shifted key → different `hash & mask` placement → different multi-group order;
   single-group maps place in insertion order (hash-independent) → invariant. **Same defect class as
   the scheduler leak.** Fixed by deriving the key from a fixed constant under `-tags dst`
   (`alg.go` `dstFixedHashKey`); enforced by `TestDSTMapHashKeyBuildInvariant`. See design.md
   "Map hash key requires `-tags dst`".

**Net:** for *within-build* `-race` replay (the meaningful "determinism under `-race`": a SUT runs in
one build) the GC per-cycle byte-trigger is the lone remaining source (Phase 2 below). For
*cross-build / composition* identity, there were two sources; the map hash key is now fixed, leaving
the GC trigger. The trace-hash probe used to draw this map is temporary (re-added per the plan above)
and is reverted before commit.

### Phase 2 — TRIGGER (the real work)

A **race-invariant trigger driven by *logical* allocation** — sum of *requested*
sizes (known at `mallocgc` before size-class rounding + redzones), not physical
`heapLive`. Tractability is split:

- **Common DST case — tractable.** For small live sets the target *floors* at
  `defaultHeapMinimum` (a constant; see the `gp < 0` / floor branch in `mgc.go`). So
  the trigger reduces to "logical bytes allocated since bubble entry ≥ constant",
  and logical bytes *are* race-invariant. This likely gets most DST workloads to
  per-cycle `-race` determinism. Implement a per-bubble logical-allocated-bytes
  counter (accumulate the requested `size` at `mallocgc` under `dstActive()`) and
  drive the floored trigger from it.
- **GOGC-scaled case — hard remainder.** When the live set is large enough that the
  target is GOGC × the *live* set, that live set is physical (`heapMarked`,
  post-mark, redzone-inflated), so the target itself is race-variant. A fully
  race-invariant target needs a *logical* live set the runtime does not track. This
  is the real redesign, for memory-pressure-adaptive SUTs (less common). Likely a
  separate increment; may stay out-of-contract if the cost/benefit doesn't justify.

Rejected: **quantize/pin the crossing** to a coarse granularity (hacky; only works
if target ≫ span; perturbs the normal-build split too).

## Current state (option 3 — shipped)

Set-level is the contract and is `-race`-robust; per-cycle discovery timing is
documented as sub-observable noise a SUT must not assert on (design.md D1 / layered
contract; the `testing/simulation` package doc). `TestDSTGCFinalizerDiscoveryDeterministic`
asserts set-level in all builds; the relative trigger is mutation-guarded by
`TestDSTMemoryLimit`'s baseline-independence check.
