# DST GC: the funneled forced-GC entries are pinned only structurally

Lands: when debug.FreeOSMemory and the goroutine-leak GC entry each have a
mid-run foreign test arm with a live bubble, or the funnel reliance is
recorded at the guard

## Gap

Severity L (audit-found 2026-07-11). The foreign-forced-GC guard's tests
exercise runtime.GC (bare + foreign-bubble) and the activation stretch; no
test calls debug.FreeOSMemory or the leak entry mid-run with a live bubble.
Their protection rests on the GC() funnel and the hoisted leak-flag guard —
a refactor reordering freeOSMemory to scavenge before GC() would do foreign
wall-clock scavenging unrefused, undetected.

## Required outcome

Mid-run foreign arms for both funneled entries (panic observed, leak flag
not armed, scavenge not run), or the guard comment records the funnel
dependency explicitly as the enforcement point.
