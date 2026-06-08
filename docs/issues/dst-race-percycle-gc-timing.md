# Per-cycle GC-discovery timing is bimodal under -race (set-level preserved)

**Lands:** when a SUT needs byte-exact *per-cycle* GC/finalizer-discovery timing
under `-race` (rare), or when a race-invariant DST GC trigger is designed. Narrow:
the logical determinism layer and GC set-level are fully deterministic under
`-race`; only *which* GC cycle discovers a given object is affected.

## Fault (measured)

Under `-tags dst -race`, `DSTGCFinDiscovery` (seed 12345, GOGC=100) prints a stable
`numGC=3 total=40000` but a **bimodal** per-cycle hash — exactly two values across
runs (`e267a13d41df5b71` / `a194f8292615403b`, ~2:4 over 6 runs). So under `-race`:

- The GC **count** (`numGC`) and the **total** finalizer set are deterministic.
- The **assignment of objects to cycles** flips between two states run-to-run.

In normal builds the per-cycle hash is a single value (byte-exact).

Reproduce: `go build -tags dst -race` the runtime testprog, then
`GOGC=100 DSTSEED=12345 ./testprog DSTGCFinDiscovery` several times.

## Root cause

The DST heap trigger (A.5, `mgc.go` `gcTrigger.test`, `gcTriggerHeap` branch) fires
when bubble-local growth `heapLive - dstHeapBase` reaches the GOGC-relative target.
It is **byte-based** and effectively **span-granular**: `heapLive` advances at
span-refill boundaries (`mcache.refill` → `gcController.update`), and the trigger is
tested in `mallocgc`. A.5 makes this byte-exact in *normal* builds by being
bubble-local — the bubble's own allocation packs into spans deterministically, and
ASLR moves addresses, not packing.

`-race` perturbs the byte↔object mapping by **one span**, run-to-run: redzone-
inflated object sizes and/or the entry baseline `dstHeapBase` (the process-live set
after the entry GC, which under `-race` includes ASLR-dependent state) shift the
span-boundary crossing by a span relative to the logical allocation sequence. The
crossing lands on one of two sides depending on a run-to-run-varying (ASLR-
dependent) factor, reassigning the objects straddling that boundary between cycle N
and N+1 — hence exactly two modes. `numGC` is unchanged because the total number of
crossings is the same; only the boundary position flips. (Locus — baseline vs.
packing — not pinned further; both are the same ±1-span byte-trigger effect.)

## Why A.5's technique does not extend to -race

A.5 cancels the *process* baseline by subtracting `dstHeapBase`, which is why normal
builds are byte-exact. But `-race`'s perturbation is *inside* the bubble's own byte
accounting (object sizes and/or the live baseline shift by a span), which the
subtraction cannot cancel.

## Options

1. **Race-invariant logical trigger.** Drive the DST trigger from the SUT's
   *logical* allocation (requested object sizes / object count), not byte-based
   `heapLive` — invariant to `-race`'s redzone inflation. **Hard:** the GOGC-relative
   target needs a *logical live set* (live bytes in requested sizes), which the
   runtime does not track (`heapMarked` is the *physical* live set, after rounding +
   redzones). A pure count-based trigger sidesteps the live set but is no longer
   GOGC-proportional and changes the GC behavior the SUT observes (and would make the
   per-cycle split differ between normal and `-race`, each deterministic — a
   different contract).
2. **Quantize/pin the crossing** to a coarser race-invariant granularity so a
   ±1-span shift cannot move an object across the boundary. Hacky; only works if the
   target ≫ a span and the variation is a single quantum, and it perturbs the
   normal-build split too.
3. **Accept (document) — current state.** The logical layer and GC set-level are
   deterministic under `-race`; per-cycle byte-exact timing is normal-build-only. A
   SUT that asserts *which cycle* a finalizer runs in, under `-race`, is the only
   thing affected (rare) and should assert set-level there. Encoded by
   `TestDSTGCFinalizerDiscoveryDeterministic` (byte-exact in normal builds,
   set-level under `-race`, branching on `race.Enabled`) and the layered contract in
   the `testing/simulation` package doc / design.md D6.

Assessment: option 1 is the only clean fix and it is a real GC-trigger redesign
(needs a logical live-set the runtime doesn't maintain) for a narrow benefit;
option 3 (accept) is the current, documented behavior.
