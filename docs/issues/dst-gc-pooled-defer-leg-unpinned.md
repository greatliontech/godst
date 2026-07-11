# DST GC: the pooled-struct cancellation's _defer leg has no killing test

Lands: when a heap-defer shape exercises the _defer arm of the pooled
cancellation with an asserted cold/warm equality, or the untested leg is
recorded at the counter

## Gap

Severity L (audit-found 2026-07-11). The pooled cold/warm cancellation counts
g, sudog, and _defer, but the pinned test programs exercise only g (1500
goroutines) and sudog (channel blocks); their defers are stack/open-coded.
Dropping _defer from dstInternalPooledTypes or neutering its pooled branch in
mallocgc passes every test; a defer-in-a-loop SUT (heap defers via newdefer)
then diverges cold-vs-warm through uncounted/unsubtracted _defer structs.

## Required outcome

A test shape that heap-allocates defers pins the _defer arm (its neutering
splits an asserted equality), or the leg's untested status is recorded at
dstInternalPooledTypes.
