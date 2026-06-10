# DST audit hardening

**Lands:** before `dst-disk`

## Source

Deep audit of the full DST delta, `go1.26.4..HEAD`, after Level-2 timer-HB validation.

## Gate

Do not start new DST feature work until these findings are either fixed, filed into narrower issue docs with an explicit `Lands:` trigger, or disputed by the user.

## Sequence

1. **Build and production hygiene.** Restore the production-untouched contract and make the repository dependency policy pass.
2. **Network semantic hardening.** Make the in-memory network preserve the public `net` contract for the surfaces it intercepts.
3. **Finalizer and cleanup isolation.** Ensure pre-bubble and run-end callback handling cannot perturb or escape a run.
4. **Level-2 exploration correctness and reporting.** Fix missed non-race boundaries and make failure reporting match the replay contract.
5. **HB pruning optimization and docs.** Reduce conservative over-exploration where safe and bring public docs back in sync with the landed contract.

## Findings

### 1. Build and production hygiene

| Severity | Finding | Failure mode | Files |
|---|---|---|---|
| H | Non-`dst` builds are not byte-identical to upstream because public-package seams call linknamed DST gates unconditionally. | Ordinary builds execute extra call edges before `crypto/rand`, `os.Getpid`, `os.Getuid`, `os.Hostname`, `net.Dial`, and `net.Listen`, violating the production-untouched contract. | `src/crypto/internal/sysrand/rand.go`, `src/os/exec.go`, `src/os/proc.go`, `src/os/sys.go`, `src/net/dial.go` |
| M | `go/build` dependency policy fails. | `testing/simulation` imports `sort`, but deps only allow `RUNTIME, internal/synctest < testing/simulation`; `./bin/go test go/build -run '^TestDependencies$' -count=1` fails. | `src/go/build/deps_test.go`, `src/testing/simulation/explore.go` |

### 2. Network semantic hardening

| Severity | Finding | Failure mode | Files |
|---|---|---|---|
| H | DST `DialContext` bypasses normal context validation and deadlines. | Nil contexts do not panic, canceled contexts can still connect, and a full accept backlog can block forever because `dstDial` has no context path. | `src/net/dial.go`, `src/net/dst.go` |
| H | Unsupported networks are silently modeled as TCP-like streams. | `net.Listen("udp", ...)` succeeds as a stream listener backed by `net.Pipe`, creating API behavior the real `net` surface cannot produce. | `src/net/dst.go` |
| M | Simulated address handling is too raw. | `:0` does not allocate unique ports, invalid ports such as `999999` are accepted, and `localhost:port` does not match a listener registered as `127.0.0.1:port`. | `src/net/dst.go` |

### 3. Finalizer and cleanup isolation

| Severity | Finding | Failure mode | Files |
|---|---|---|---|
| H | Pre-bubble finalizer/cleanup draining is not a fixpoint and runs callbacks synchronously after DST is active. | A pre-bubble callback chain can leak its tail into the first in-bubble drain; a pre-bubble callback can also block `simulation.Run` entry or consume DST RNG/scheduler state before the bubble is rooted. | `src/runtime/dst.go`, `src/runtime/synctest.go` |
| M | Run-end finalizer/cleanup fixpoint cap can leak valid finite chains longer than 256 levels. | A long but finite chain can escape to post-run async finalizer/cleanup execution and run with `g.bubble == nil`. | `src/runtime/synctest.go` |
| M | `GoroutineProfile` can overcount while DST finalizers run on `synctestGCDrain`. | The drain is treated as a user goroutine, but `fingRunningFinalizer` still adds one extra finalizer goroutine to the profile count. | `src/runtime/mfinal.go`, `src/runtime/mprof.go`, `src/runtime/traceback.go` |

### 4. Level-2 exploration correctness and reporting

| Severity | Finding | Failure mode | Files |
|---|---|---|---|
| H | Non-race `Explore` misses the post-`go` transition boundary. | An assertion-only SUT with `go child; parent writes; wait` cannot explore child-before-parent-write, yet may report `Exhausted=true`. | `src/runtime/proc.go` |
| M | `Explore` does not convert SUT panics or synctest deadlocks into replayable failures. | `simulation.Explore(..., func() bool { panic("boom") })` unwinds instead of returning failure metadata with the schedule. | `src/testing/simulation/explore.go` |
| M | Multiple new TSan reports in one explored schedule collapse into one `Failure{Race:true}`. | A schedule that increments `RaceErrors` by more than one yields one failure even though the spec says each distinct race yields one race failure. | `src/testing/simulation/explore.go` |
| L | Public `Explore` has no schedule/step budget options. | Users cannot request bounded exploration with explicit budget-hit reporting, though trace overflow is reported. | `src/testing/simulation/explore.go` |

Status: sequence item 4 fixes the non-race post-`go` boundary, top-level SUT callback panic metadata, multi-race reporting, and public schedule/step budgets. SUT-created child-goroutine panic conversion is split to `docs/issues/dst-explore-child-panic-failure-reporting.md`, and synctest-deadlock conversion is split to `docs/issues/dst-explore-deadlock-failure-reporting.md`, because both require runtime/synctest-layer handling beyond callback-local recovery.

### 5. HB pruning optimization and docs

| Severity | Finding | Failure mode | Files |
|---|---|---|---|
| L | DPOR HB only models ready/create edges, not uncontended mutex unlock→lock or buffered channel send→receive HB. | Correctness is conservative, but protected accesses can be over-explored and raise overflow risk. | `src/testing/simulation/explore.go` |
| L | Public package docs are stale. | `testing/simulation` still says real network I/O is not deterministic and says GC per-cycle discovery is not in contract, while the design says network and GC per-cycle discovery are landed. | `src/testing/simulation/simulation.go`, `docs/dst/design.md` |

## Close-Out

When a sequence item is fixed, promote the durable rationale into the relevant spec section, code comment, or regression test. When all findings are fixed, filed narrower, or user-disputed, remove this issue from `docs/issues/README.md` and delete this file.
