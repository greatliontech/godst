# Plan: primary-go — vanilla Go + DST patches

Spec: docs/dst/design.md ("Untagged footprint (contract)") and
docs/dst/releases.md. Derived from the 2026-08-23 untagged-inertness and
CI-coverage audits; chunk 1 amends the spec to the contract the rest of the
plan conforms code and CI to.

- [x] 1. Spec amendment: design.md untagged contract becomes
  instruction-identity-modulo-recorded-allowlist (INV-VANILLA, spec-tier with
  `Lands:` until chunk 7); releases.md gains the positioning statement and the
  upstream patch-cadence policy.
- [x] 2. `os.Root` create ops (`OpenFile`/`Mkdir`/`MkdirAll`) return to stock
  `0o777` validation in both build modes; mode-bit preservation through
  creation/Chmod/durability stays; the design.md filesystem-metadata clause is
  rescoped to stock's accepted domains.
- [x] 3. Runtime residue gating: `gdestroy`'s ungated stores and bubble-drain
  guard, `gcStart`/`gcAssistAlloc` save-restores, and `isSystemGoroutine`'s
  package-var guard become constant-guarded and fold away untagged.
- [x] 4. Finalizer/cleanup callback residue (`addfinalizer`, `addCleanup`,
  `runCleanups`, `(*cleanupQueue).enqueue`) folds away untagged;
  `TestDSTUntaggedCodeFootprint` moves its anchors to the carrier frames and
  design.md's anchor sentence is updated in the same chunk.
- [x] 5. Inline-neutrality: `goready`, `syscall.Read`/`Write` regain stock
  inlining; sweep for other guard-cost inlining losses; the hook shape that
  survives the inliner's pre-fold cost model is designed here.
- [x] 6. Small-residue sweep: `crypto/internal/sysrand` untagged read path
  returns to stock shape; `os.root` dst nil-check residue;
  `testing.chattyPrinter` untagged initialization; the syscall fd-wrapper
  split (`Close`/`Fstat`/`Seek` → `closeFD`/`fstatFD`/`seekFD`) folds back
  to stock untagged text; the `os.dstFileBackend` interface type's
  `time.Time`-carrying method retains the whole `time` package in untagged
  binaries (~35 KB, `io/fs.FileMode.String` is one symptom) — break the
  retention; `syscall.gettimeofday`'s fenced wrapper gained inlinability
  (callers' text); stock-only `internal/abi.TypeOf` and the `type:.eq.*`
  pair identified and dispositioned.
- [ ] 7. Differential untagged-inertness gate: symbol-level text comparison
  of untagged-built probe-corpus programs (corpus closure covers every
  dst-modified std package) against the same programs built by the upstream
  base release toolchain, machine-checked
  allowlist of recorded deviations (each cited to design.md); Taskfile leg
  wired into CI; INV-VANILLA promoted to enforced; dispositions whether
  `TestDSTUntaggedCodeFootprint` is subsumed or kept as the fast leg.
- [ ] 8. Exported-API baseline: `api/godst.txt` for `testing/simulation`,
  `cmd/api` green, an api leg in per-PR CI.
- [ ] 9. cmd and breadth legs: `cmd/internal/testdir` plus short
  `cmd/go`/`cmd/compile`/`cmd/link` legs (own TMPDIR outside GOROOT),
  `test:inert-std` into the per-PR gate, arm64 axis in matrix, untagged
  `go build std` sweep over the supported platform list, and the
  tagged-context dependency-policy evaluation (dispositions the
  tagged-import-policy-gate issue); arm64 enablement of the vanilla text
  gate (its drift heuristics are amd64-tuned).
- [ ] 10. Nightly `go tool dist test` leg in matrix on amd64 and arm64,
  untagged; environment-red tests skipped by explicit name with citation.
- [ ] 11. Release workflow validates the extracted distpack binary tarball:
  untagged version/build/test use plus the tagged smoke, run from the
  extraction rather than the checkout.
- [ ] 12. Port cadence tooling: upstream tag-watch workflow, post-commit port
  verification (recoverable from the merge commit, no worktree state),
  scripted half of the port audit as a task, port rehearsal job, matrix
  trigger for port branches.
- [ ] 13. Local install: distpack-based `task install` to a versioned prefix
  with a `current` symlink; `go` (vanilla) and `godst` (`GOTOOLCHAIN=local`)
  launcher pair off one install; releases.md install documentation.
