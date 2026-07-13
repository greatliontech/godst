# Pre-existing enforcing-leg failures (environment-sensitive)

`task test:untagged` and `task test:dst` are red on this development machine
with six failures that reproduce byte-identically on a pristine `9520a9ef49`
worktree built from scratch (`git worktree add` + `make.bash`) — none is
caused by any working-tree change; the rest of both legs is green with them
skipped.

Untagged leg (`go test -count=1 -short runtime`):

- `TestDSTGCSysstackAlloc` — the `-tags dst` testprog is extremely slow, not
  hung: at `DSTSEED=12345` it completes ("done", exit 0) after 23.5 minutes of
  single-P CPU on this machine, far past the suite's per-test budget (~530 s
  in the untagged -short leg), so the harness kills it first. Affects the
  tagged leg too (same testprog under `test:dst`'s 60 m package budget).
- `TestDSTRunRejectsOverlappingTopLevelRuns` — the overlap guard probe reports
  `overlap=false active=true` (the second top-level `Run` was never observed
  during the first's active window).
- `TestDSTCryptoUnseededVectors` — "unseeded readers did not run during the
  active window".
- `TestDSTCryptoPriorRunCaller` — "the prior run's caller did not read during
  the active window".

Tagged leg (`go test -tags dst -count=1 -timeout 60m runtime testing/simulation
crypto/rand net os syscall os/exec os/signal`), beyond the four above (which
recur in the tagged runtime package):

- `net`: `fatal error: total < 0` (the bubble-accounting fatal in
  `runtime/dst.go`) during `TestDSTNetSYNACKObservesHostCrash`
  (`net/dst_latency_test.go:143`), killing the whole net test binary.
- `os`: `TestDSTPipeBasic` — "host fds changed across a pipe run: before
  [0..6], after [0..7]" in isolation: the run's `os.Create("/f")` page-cache
  memfd is still open at the after-census (page caches release lazily at the
  NEXT run's first filesystem op), so the census gains one fd; in a full-suite
  run the before/after delta shifts with whatever the predecessor run left
  pending. Reproduces identically on the pristine worktree, isolated and
  full-suite.

The overlap/crypto three share a shape: a helper goroutine that must
interleave with an active run's wall-clock window never gets scheduled inside
it — suggesting a machine/kernel-scheduling sensitivity (observed on Linux
7.1.3-arch1-2), possibly the same root as the GC item's slowness. The net and
pipe items look machine-independent (an accounting invariant and a
lazy-release census asymmetry) but predate the audit either way. Whether any
of these are red on the machines the legs were previously gated green on is
unestablished.

Diagnosis anchor: reproduced on pristine HEAD `9520a9ef49` (fresh worktree +
`make.bash`), 2026-07-13, with identical failure text — so any fidelity-audit
working-tree diff is excluded as the cause.

Lands: when both enforcing legs are green on a reference machine, or with the
determinism-escape sweep (which owns wall-clock-window sensitivity) if that
lands first.
