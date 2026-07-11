# DST GC: foreign debug.SetGCPercent mid-run silently perturbs the trigger

Lands: when a mid-run gcPercent/heapMinimum mutation from outside the run's
bubble is refused loudly or latched out of the DST trigger, with a test arm

## Gap

Severity M (audit-found 2026-07-11; adjacent to the foreign-forced-GC guard,
which covers cycle STARTERS only). setGCPercent writes gcController.gcPercent
and heapMinimum with no DST gate, and the DST trigger reads both LIVE on
every bubble allocation (the GOGC-scaled target and its floor). A foreign
goroutine — a GC-tuner library, test infra — calling debug.SetGCPercent(50)
mid-run halves every subsequent GOGC-scaled crossing's target from a
wall-clock instant: same-seed runs diverge in NumGC and per-cycle discovery
with no panic — the silent-nondeterminism class the forced-GC guard kills.
SetGCPercent(-1) flips the bubble into the GOGC-off trigger rules mid-run.
(Contrast: foreign debug.SetMemoryLimit is inert — the DST branch reads only
the run-scoped dstMemLimit.)

## Required outcome

A foreign mid-run SetGCPercent either fails loudly (the forced-GC guard's
shape) or the DST trigger reads a run-latched gcPercent/heapMinimum snapshot
so the mutation cannot land mid-run; either way a test arm pins it, and
gc.md records the chosen contract.
