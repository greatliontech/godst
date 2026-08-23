# sysmonUpdateGOMAXPROCS's run-active guards have no enforcing test

Two guards in `sysmonUpdateGOMAXPROCS` are unenforced:

- **The pre-push skip** (`maxProcsUpdateBlocked(custom)`, the `dstActive()`
  half): sysmon must not wake the auto-update helper while a simulation is
  active. The helper's own post-STW re-check (enforced by
  `TestDSTRunAutoMaxProcsUpdateDropped`) drops the *resize*, but only after
  `stopTheWorldGC` — so without the sysmon skip a cgroup/affinity change
  mid-run admits a stop-the-world pause under the simulation. Dropping the
  skip survives every suite: nothing observes a mid-run STW.
- **The under-lock recheck**, below.

In a dst build, `sysmonUpdateGOMAXPROCS` rechecks `updateMaxProcsG.idle`
under the helper's lock before pushing, because a second pusher (the DST
GOMAXPROCS hooks use the same protocol) may have readied the helper between
sysmon's first check and the lock; injecting an already-runnable g throws.
Stock has one waker, so untagged the recheck folds away (`!dstBuild || …`).
Tagged, dropping the recheck survives every current suite: the race window
is the instruction gap between the check and the lock, and no test steers
an interleaving into it.

Closing both needs the same shape as the gc-assist carry issue: for the
skip, an observable for "an STW happened inside the run" (a test hook
counting STWs by reason, sampled across a pushed update); for the recheck,
a deterministic interleaving probe at that lock (or a controllable pause
hook between the check and the lock) with reachability asserted, not a
timing-dependent stress loop.

Lands: user decision.
