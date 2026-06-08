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

### Phase 2 — TRIGGER — DONE

Both sources the Phase-1 map found are fixed. The authoritative write-up is now
design.md ("How per-cycle discovery is made deterministic under `-race`" and "Map
hash key requires `-tags dst`"); this records the outcome and the corrections to
the original plan.

**2b — map hash key (cross-build).** Fixed: the `-tags dst` AES hash key is derived
from a fixed constant (`alg.go` `dstFixedHashKey`), position-independent, so
multi-group map order is build/composition-invariant. Enforced by
`TestDSTMapHashKeyBuildInvariant`. Committed (`fix(dst): make the -tags dst map hash
key build/composition-invariant`).

**2a — GC per-cycle trigger.** Fixed, with two corrections to the plan above, both
established by trace-hash localization (a throwaway `DSTMapProbeVerbose` capturing
the raw per-cycle trigger inputs):

- *The counter is per-object `elemsize`, not "requested size".* The original premise
  (requested size, pre-rounding) is deterministic but in the wrong units; `elemsize`
  (size-class size) is equally deterministic and `-race`-invariant **and** in
  `heapMarked`'s units, so the GOGC-scaled comparison is **exact**, not merely
  proportional. The counter (`dstHeapAlloc`) sums `elemsize` per allocation at the
  `mallocgc` dispatcher, and the trigger is checked **per allocation** (not at span
  grabs). Localization: the *logical* crossing point is deterministic; only
  span-granular `heapLive` accounting was not.
- *The GOGC-scaled case is NOT a hard remainder; no "logical live set" is needed.*
  The instrumentation showed `heapMarked` is deterministic given a deterministic
  crossing — the feared physical-live-set redesign (C) was an over-estimate. Driving
  the GOGC-scaled crossing off `dstHeapAlloc` makes per-cycle discovery deterministic
  for the large-live-set regime too (measured 300/300, normal and `-race`). Only a
  rare **sub-object** residual remains in the GOGC-scaled target via the `dstHeapBase`
  process baseline — sub-observable, the `HeapAlloc`/`HeapInuse` byte-noise class
  (DST-MEM-1), does not flip discovery.

Enforced by `TestDSTGCPerCycleDiscoveryDeterministic` (mid-run partial discovery
reproduces across same-seed runs, floored and GOGC-scaled, in normal and `-race`
builds; mutation-guarded: reverting to the `heapLive` crossing or dropping the
per-allocation check makes it wobble).

Rejected during the investigation: **requested-size counter** (unit mismatch for the
GOGC-scaled target); **logical live-set redesign** (C — unnecessary, `heapMarked` is
deterministic); **quantize/pin the crossing** to a coarse granularity (hacky).

## Status — RESOLVED

Per-cycle GC discovery is deterministic under `-race` (within-build replay) and
across builds/compositions, for both the floored and GOGC-scaled regimes — the
"full determinism under `-race`" goal. The contract is now stated in design.md (the
layered-contract table's per-cycle row is `holds`); `TestDSTGCFinalizerDiscovery
Deterministic` (set-level) and `TestDSTGCPerCycleDiscoveryDeterministic` (per-cycle)
guard it. The temporary trace-hash probe used to map and localize the sources is
reverted before commit. This doc is retained for provenance; design.md is
authoritative.
