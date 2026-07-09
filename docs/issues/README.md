# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number, a condition, or "pending feature" for planned roadmap work). The
chunk-start gate (sub-chunk `N.1`) scans this index for entries resolving to the
current chunk; the close-out gate promotes any load-bearing rationale inline and
deletes the resolved entry.

## Open

| Issue | Lands |
|---|---|
| [DST gmdb clock_gettime virtualization](./dst-gmdb-clockgettime.md) | 9 |
| [DST gmdb crash resource teardown](./dst-gmdb-crash-resource-teardown.md) | 10 |
| [DST gmdb process crash and restart](./dst-gmdb-process-crash-restart.md) | 11 |
| [DST gmdb host crash and restart](./dst-gmdb-host-crash-restart.md) | 12 |
| [DST gmdb crash tear fidelity](./dst-gmdb-crash-tear-fidelity.md) | 13 |
| [DST gmdb end-to-end compatibility coverage](./dst-gmdb-end-to-end-coverage.md) | 14 |
