# Scheduler determinism-net strengthening: three latent gaps, no demonstrated fault

Test-surface extensions identified by audit; none has a reachable failing
path today, so each is hardening, not a fix:

- **Disabled-visibility rule lacks a direct white-box pin.** The GC
  `sched.disable.user` window's candidate filter (user goroutines invisible
  to selection; all-invisible nil-return + `gcBlackenEnabled` throw) is
  enforced but pinned only by composed regressions (overlap-guard/crypto
  runs that happen to cross a GC window) — a PARTIAL weakening (filter
  dropped from one scan, `anyVisible` intact; or the alternation-slot
  `dstSchedPrevSys` burn) is caught only probabilistically. A white-box test
  driving the window deterministically closes it.
- **Trace-site ident asymmetry weakens the diversity fold at one site.** The
  timer site records the raw draw as its ident; the select site records the
  chosen index, so the xorIdent freeze-detection fold can alias across seeds
  when candidate counts are tiny. Recording the raw draw at the select site
  (or folding both) restores uniform detection strength. Observation-only —
  no determinism impact.
- **Root teardown order rides pointer-map iteration.** `dstCloseRoots`
  closes victims in map order while `dstCloseOpenFiles` sorts by
  registration seq precisely because close order is observable;
  `dstOpenFileEntry.seq` stays zero for roots. `Root.Close` has no
  observable side effect today, so no escape exists — but any future
  observable effect on Root close forks the schedule silently. Stamping seq
  at root registration and sorting the teardown removes the trap.

Lands: when the scheduler/fs determinism test surface next extends — each
item lands as a test-only (first two) or mechanical-ordering (third) change
with its own pin; any in-run-observable Root.Close side effect added before
then MUST land the third item with it.
