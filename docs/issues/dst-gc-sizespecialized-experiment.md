# DST: GOEXPERIMENT=sizespecializedmalloc bypasses the deterministic GC trigger

Lands: when the experiment combo is either supported (generated trigger sites
gated and counted) or refused at build/vet level, with a test that builds
under the experiment

## Gap

Severity M (review-found 2026-07-10; pre-existing). With
`GOEXPERIMENT=sizespecializedmalloc` and `-tags dst`, the compiler emits
direct size-specialized malloc calls in USER packages
(`cmd/compile/internal/ssagen/ssa.go` gates emission on the experiment only —
`dstBuild` is invisible to it), so SUT allocations reach
`malloc_generated.go`'s own trigger sites and never pass the `mallocgc`
dispatcher: they neither count toward `dstHeapAlloc` nor gate the DST heap
trigger. The runtime-side `sizeSpecializedMallocEnabled` const
(`malloc.go`, `!dstBuild`) disables only runtime-internal dispatch.

Interim enforcement (landed with the trigger-start confinement):
`enterSimulation` panics when `goexperiment.SizeSpecializedMalloc` is set,
mirroring the FIPS-mode refusal — the combination cannot silently produce a
nondeterministic run. The refusal branch is const-folded and cannot be
exercised by the regular suite (it would need a toolchain and std built under
the experiment), which is this issue's remaining test gap. The refusal also
over-refuses instrumented builds (-race/-msan/-asan), where the compiler
suppresses specialized emission and there is no bypass — loud-refusal in a
safe direction; resolving that precision is part of this issue.

## Required outcome

Either the experiment combination is supported — the generated allocation
paths count into `dstHeapAlloc` and respect the bubble-gate-only trigger
start — or it is refused at a level a test can pin (cmd/go build refusal, or
a CI leg that builds std under the experiment and asserts the
`enterSimulation` panic). The gc.md build-matrix note stays accurate either
way.
