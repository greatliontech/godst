# Per-cycle GC-discovery timing is out-of-contract; elevating it needs a race-invariant trigger

**Lands:** when a SUT demonstrably needs deterministic *per-cycle* GC/finalizer-
discovery timing — i.e. it asserts *which* GC cycle discovers, finalizes, or clears
a given object, not merely the total set. Rare; no such need is demonstrated today.

**This is not a defect against the current contract.** The DST contract is
deliberately **set-level only**: `numGC` and the total set of finalizers/weak refs
discovered are deterministic (including under `-race`), but *which cycle* discovers
a given object is **not** claimed and **not** tested (design.md D1; the
`testing/simulation` package doc; the layered-contract section "Why per-cycle
byte-exact is not in the contract"). This issue tracks the *optional* work to raise
per-cycle discovery to an in-contract deterministic property, should a SUT ever
require it — and records the root cause and the (hard) fix so the analysis is not
relost.

## Behaviour (the thing that would have to be fixed)

*Which* GC cycle discovers a given object is byte-exact only in a fixed normal
build. It is perturbed — the per-cycle split moves by ±1 span — by:

- **`-race`/`-msan`**: redzone-inflated object sizes and ASLR-dependent entry state
  shift the byte↔object mapping by a span. Measured (when a per-cycle probe still
  existed): a **bimodal** split — two values run-to-run; `numGC` and the total set
  stayed stable.
- **Binary composition**: linking a heavier import set shifts the bubble's entry
  span-fill phase, moving the crossing near a span boundary (this is what forced an
  earlier crypto/os-user testprog into its own binary before the test went
  set-level).

`numGC` and the total discovered set are unaffected in all cases (the number of
trigger crossings is the same; only which side of a boundary an object lands on
flips).

Note: the per-cycle measurement probe (`dstFinqSeq`/`dstFinqSeqFP`) was removed when
the discovery test went set-level, so reproducing the bimodality now requires
re-instrumenting `mgc.go` to hash the per-cycle `finqueued − dstFinqBase` sequence.

## Root cause

The DST heap trigger (A.5, `mgc.go` `gcTrigger.test`, `gcTriggerHeap` branch) fires
when bubble-local growth `heapLive − dstHeapBase` reaches the GOGC-relative target.
It is **byte-based** and effectively **span-granular**: `heapLive` advances at
span-refill boundaries (`mcache.refill` → `gcController.update`), tested in
`mallocgc`. A.5 makes it byte-exact in normal builds by being bubble-local — the
bubble's own allocation packs into spans deterministically. But the `dstHeapBase`
subtraction only cancels the *process* baseline; a perturbation *inside* the
bubble's own byte accounting (redzone-inflated sizes, or an entry span-fill phase
that shifts the first crossing) is one the subtraction cannot cancel, and it moves
the crossing by a span.

## Options

1. **Race-invariant logical trigger (the only clean fix).** Drive the DST trigger
   from the SUT's *logical* allocation (requested object sizes / object count),
   invariant to redzone inflation and span-packing. **Hard:** the GOGC-relative
   target needs a *logical* live set (live bytes in requested sizes), which the
   runtime does not track — `heapMarked` is the *physical* live set (after rounding
   + redzones). A pure count-based trigger sidesteps the live set but is no longer
   GOGC-proportional and changes the GC behaviour the SUT observes. This is a real
   GC-trigger redesign.
2. **Quantize/pin the crossing** to a coarser race-invariant granularity so a ±1-span
   shift cannot move an object across a boundary. Hacky; only works if the target ≫ a
   span and the variation is a single quantum, and it perturbs the normal-build split
   too.
3. **Current state — out-of-contract.** Set-level is the contract and is `-race`-
   robust; per-cycle discovery timing is documented as sub-observable noise a SUT
   must not assert on. A SUT that needs per-cycle determinism asserts set-level
   instead. (`TestDSTGCFinalizerDiscoveryDeterministic` asserts set-level in all
   builds; the relative trigger is mutation-guarded by `TestDSTMemoryLimit`'s
   baseline-independence check.)

Assessment: option 1 is the only sound elevation and it is a GC-trigger redesign
(needs a logical live set the runtime does not maintain) for a benefit no SUT has
yet demonstrated. Option 3 is the shipped behaviour.
