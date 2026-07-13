# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number when an active plan exists, a self-contained condition, or
"pending feature" for planned roadmap work). At close-out, lasting rationale is
promoted into a kept-current artifact and the resolved entry is deleted.

## Open

- [capability-write-entersyscall-gc-window.md](./capability-write-entersyscall-gc-window.md) —
  inherited-file capability writes carry the entersyscall window that
  wall-timed host events (P reclaim races, pending stops) can turn into a
  same-seed schedule fork (mechanism demonstrated and closed on the
  framework `-v` stream). Lands: with the determinism-escape sweep, or when
  a consumer runs capability I/O alongside host-parallel load, whichever
  first.
- [untagged-leg-preexisting-failures.md](./untagged-leg-preexisting-failures.md) —
  six environment-sensitive `test:untagged`/`test:dst` failures reproduced on
  pristine HEAD `9520a9ef49`. Lands: when both enforcing legs are green on a
  reference machine, or with the determinism-escape sweep if that lands first.
