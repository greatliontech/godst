# DST GC: pooled/heap counter gates key on the live bubble field

Lands: when the mallocgc DST gates and allgadd key on sticky membership
(dstSimG) like the scheduler, or per-window unreachability is recorded at the
gates

## Gap

Severity L (audit-found 2026-07-11; latent — no demonstrated in-spec path).
The mallocgc pooled/count gate and allgadd test `cur.bubble == dstSimBubble`,
but gcStart and gcAssistAlloc nil the bubble field for their bodies — the
mechanism the sticky dstSimG bit was introduced for in scheduler
classification. An allocation inside those windows (a fresh sudog from a
contended startSema semacquire) would escape both dstHeapAlloc and
dstPooledAlloc. At GOMAXPROCS=1 those semaphores are uncontended, so no
reachable path is demonstrated; recorded as an inconsistency with the
sticky-membership design rather than a fault.

## Required outcome

The counter gates key on the sticky bit, or each disassociation window
carries a recorded no-allocation/unreachability argument at the gate.
