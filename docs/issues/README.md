# Issue docs

Tracked follow-ups deferred out of a chunk. Each entry carries a `Lands:` trigger
(a chunk number or a condition). The chunk-start gate (sub-chunk `N.1`) scans this
index for entries resolving to the current chunk; the close-out gate promotes any
load-bearing rationale inline and deletes the resolved entry.

| Issue | Lands | Summary |
|-------|-------|---------|
| [dst-gomemlimit-rss-nondeterminism](dst-gomemlimit-rss-nondeterminism.md) | open — decide what GOMEMLIMIT should mean under DST | GOMEMLIMIT and RSS-derived MemStats (HeapReleased) are nondeterministic under DST: they derive from total mapped memory, which is not bubble-local. GOMEMLIMIT is currently ignored by the trigger; memory is bounded by the GOGC-relative trigger (and a heapMinimum floor for GOGC=off). |
