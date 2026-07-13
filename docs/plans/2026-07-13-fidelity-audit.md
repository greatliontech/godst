# DST fidelity audit — no false positives, no false negatives

**Goal.** The DST is valuable only if it never reports false
positives (a fence or model refusing/failing a legal production
shape) or false negatives (simulation semantics diverging from
production Go/Linux so a real bug cannot manifest, or nondeterminism
escaping the seed so failures don't reproduce). This plan audits and
fixes by that standard, ordered by the surfaces protodb — the sole
consumer — exercises. Every fork change gates on the fork's own
Taskfile legs AND a rebuild (`src/make.bash`) + protodb DST
regression slice; sizeable chunks run protodb's full sharded suite.

**Consumer-witnessed seeds** (from protodb's migration to the
audit-fixes toolchain): the `go test -v` / `t.Log` fd-1 panic (the
testing package's chatty printer streams into the bubble with no
capability seam — blocks verbose diagnosis entirely); the
InheritFile capability's node-scoped refusal being swallowed silently
by slog (consumers need a relay they must discover by data loss);
`/dev/null` ENOENT (recorded gap, common legal shape).

- [x] **1 — False-positive burn: the testing-framework stdio seam.**
  The `-v` chatty printer and `t.Log`-adjacent framework writes are
  go-test plumbing, not SUT code — they must not trip the SUT's
  stdio fence. Fix at the testing-package seam (a pre-granted
  framework capability or host-side buffering, per design.md's stdio
  stance — spec-first against §"Deterministic pipes and the stdio
  stance" + §"The interception boundary"); `/dev/null` gets its
  legal-shape disposition (model it or keep the recorded gap with a
  loud typed refusal naming `io.Discard`). Capability node-scope
  refusals become distinguishable (a typed error per the fork's
  settled refusal taxonomy — production-shaped; an error-swallowing
  consumer pipeline stays the consumer's to guard, with the relay
  idiom named in the spec). Gate: fork legs + protodb smoke with `-v`
  usable.
- [x] **2 — Differential conformance harness (the false-negative
  net).** A host-vs-sim differential runner: the same op-grammar
  executed against real Linux primitives and the simulation, outcomes
  diffed (error identity via errors.Is chains, partial-count
  semantics, ordering-observable results). First grammars: pipes
  (the host-probed model's full ladder), TCP conn lifecycle
  (dial/accept/deadline/partial write/EOF/reset ordering — the
  surfaces protodb's mesh leans on), fs durability
  (fsync/O_DSYNC/rename/crash-tear visibility). Lands as a Taskfile
  leg (`test:conformance`) so model drift is caught structurally,
  not by consumer debugging. Divergences found are per-item fidelity
  fixes inside the chunk.
- [ ] **3 — Determinism-escape hunt.** Same-seed cross-RUN and
  cross-process transcript equality swept under `-race` and under
  environmental perturbation (TZ, locale, cwd, GOMAXPROCS, ASLR-ish
  address sensitivity via map iteration); close the deferred
  drifted-lazy-timer-timestamps item (its definition landed — verify
  or fix); any wall-VALUE read reachable inside a run is a finding.
- [ ] **4 — Seeded-schedule diversity (the quiet false negative).**
  protodb witnessed seed-basin resonance (election-livelock class):
  one seed's schedule can systematically hide interleavings. Audit
  the plain seeded path's decision distribution (not Explore — the
  path consumers actually run): quantify schedule diversity across
  seeds for a fixed program; identify degenerate-choice hot spots;
  assess surfacing a cheap Explore tier (PCT depth-1) as the
  consumer-facing default for cluster suites. Deliverable may be
  measurement + a design recorded in design.md §Roadmap rather than
  code — no silent scope call either way.
- [ ] **5 — Deferred arch verification.** The two parked items:
  loong64 Fstat verification and MIPS fence verification — verify on
  real or emulated targets per the Taskfile's cross legs, or
  re-defer with self-contained conditions in issue docs.

Rulings inherited from the consumer session: work lands on `dst`
directly; the adversarial review loop, gate lines, and commit
cadence run exactly as in protodb; protodb's DST suite is this
fork's integration net (rebuild + rerun on every behavior-bearing
change).
