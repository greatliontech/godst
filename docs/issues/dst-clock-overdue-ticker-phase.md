# DST: multi-period-overdue ticker crossing DriftClock fires with a one-shot phase error

Lands: when the mid-run re-map converts an overdue timer's when backwards
(when' = now − remap(now−when), clamped against the when <= 0 sentinel) so the
catch-up division counts host periods exactly

## Gap

Severity M (review-found 2026-07-10; pre-existing — on HEAD the same shape
additionally kept the old-rate period forever). A ticker overdue by more than
one period at a `DriftClock` instant (its channel timer is never heaped while
no reader blocks, so it sits due-unfired) re-arms via the catch-up
`next = when + period·(1 + delay/period)` — which divides `delay = now − when`,
a base span straddling both rate regimes, by the NEW-rate period anchored at
the OLD-rate `when`. Reproducer: `NewTicker(100ms)` on host h,
`Sleep(350ms)`, `DriftClock("h", 1e9)` (rate 2), `<-C`: next fires at 400ms
base where the host-correct boundary (next multiple of 100ms host after
350ms host elapsed) is 375ms base — a one-shot phase error bounded by one
period. The spec formula `when' = T + (when−T)·r_old/r_new` covers the
negative remainder; the code's `t.when > now` guard (and `dstDriftRemap`'s
`x <= 0` pass-through) omits it. faults.md's boundary clause is scoped to the
exactly-due case for this reason.

Fix caveat: a naive backwards remap can drive `when <= 0` on a rate decrease,
colliding with the `t.when == 0` "not running" sentinel `maybeRunChan` keys
on — the conversion needs a clamp.

## Required outcome

A timer overdue at the change instant re-arms on host-period boundaries
computed consistently in one rate regime (the spec formula's negative
remainder honored, sentinel-safe), pinned by an overdue-multi-period ticker
test; faults.md's boundary clause then widens from "due exactly at the
change instant" to overdue timers.
