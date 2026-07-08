# DST gmdb flock single-writer discipline

Lands: 5

## Gap

gmdb uses `Flock` for cross-process single-writer coordination. DST currently fences `Flock`, so separate simulated processes cannot exercise writer exclusion or stale-writer release through file locks.

## Required outcome

`Flock` over simulated file descriptors supports `LOCK_EX`, `LOCK_SH`, `LOCK_UN`, and `LOCK_NB`. Locks are scoped to the simulated host and file, are owned by the simulated process and fd, block or return `EWOULDBLOCK` according to `LOCK_NB`, and are released when the owning fd closes or the owning simulated process crashes.
