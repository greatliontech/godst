# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number when an active plan exists, a self-contained condition,
"pending feature" for planned roadmap work, or "user decision" when neither a
chunk nor a checkable condition exists and scheduling is the user's call). At close-out, lasting rationale is
promoted into a kept-current artifact and the resolved entry is deleted.

## Open

- **import-policy gate does not cover dst-tagged build contexts** — Lands: 9
  (primary-go plan). `deps_test` evaluates untagged contexts only, so a
  dst-tagged std import bypasses the policy gate. See
  [tagged-import-policy-gate.md](tagged-import-policy-gate.md).
- **hand edits to generated zsyscall files will fight regeneration** — Lands:
  when the linux zsyscall files are next regenerated. The dst fd-wrapper
  split renames symbols inside generated `zsyscall_linux_*.go`. See
  [zsyscall-regeneration-conflict.md](zsyscall-regeneration-conflict.md).
- **gomutant cannot load the fork's std tree** — Lands: when `gomutant
  discover` reports non-zero targets for the fork's std tree. Until then
  probes and campaigns fall back to hand edits, and reviewer hand-probes
  are unavailable. See [gomutant-std-tree.md](gomutant-std-tree.md).
- **gcAssistAlloc's DST membership carry has no enforcing test** — Lands:
  user decision (the enforcing test needs deterministic in-sim assist
  steering — its own design). The gcStart twin is enforced; the assist twin
  survives every current suite when dropped. See
  [gc-assist-membership-carry-unenforced.md](gc-assist-membership-carry-unenforced.md).
- **callback batch loops discriminate their driver two ways** — Lands: user
  decision. `runFinqBlocks` compares against `fing`, `runCleanupBlock`
  consults `findfunc`; one role mechanism would serve both and delete the
  lookup from the tagged path. See
  [callback-loop-driver-discrimination.md](callback-loop-driver-discrimination.md).
- **sysmonUpdateGOMAXPROCS's run-active guards have no enforcing test** —
  Lands: user decision. The pre-push skip (prevents a mid-run STW the
  helper's own gate cannot) and the under-lock recheck (rare second-pusher
  race) both lack a deterministic probe; same class as the gc-assist carry.
  See
  [sysmon-maxprocs-recheck-unenforced.md](sysmon-maxprocs-recheck-unenforced.md).
- **the syscall fences use three guard shapes for one concept** — Lands:
  user decision. Nested zero-cost block, `dstSimFenced && …` conjunction, and
  (pending the wrapper-split fold) unguarded stub calls; one shape, with the
  inline-parity sweep as oracle. See
  [syscall-fence-guard-shapes.md](syscall-fence-guard-shapes.md).
- **enterSimulation's build-mode refusals share one shape** — Lands: user
  decision. The FIPS latch and the arenas-experiment check are one concept
  in two shapes. See
  [enter-simulation-refusal-shapes.md](enter-simulation-refusal-shapes.md).
- **rooted MkdirAll walk shares the Root resolver** — Lands: user decision.
  `dstRootMkdirAll` re-implements `dstRootResolveLocked`'s component walk
  (`.`/`..`/escape/ENOTDIR handling) with its own last-component rule; two
  copies of one walk are where the EEXIST/ENOTDIR divergence lived. See
  [root-mkdirall-shares-resolver.md](root-mkdirall-shares-resolver.md).
- **conformance harness: an `os.Root` domain** — Lands: user decision.
  `os.Root` is modeled separately (`src/os/dst_root.go`) but has no
  differential domain; divergences surface only by hand probe (the 1.26.7
  port audit found one). See
  [conformance-os-root-domain.md](conformance-os-root-domain.md).
- **killed-goroutine kill-trace diagnostics** — Lands: pending
  feature. A process body's return or panic kills its still-running
  goroutines by design (`src/testing/simulation/node.go` process
  model) — but silently, so a workload that spawns long-lived
  machinery from a transient process sees only a downstream stall
  with no error anywhere (field case: an apply pump spawned on a
  driver process cost a two-layer root-cause hunt before the kill
  was even suspected). Record kill events — process id, the killed
  goroutine's spawn site — into the deterministic trace and surface
  them in failure output (and/or an opt-in live log), so "goroutine
  spawned at S was killed with process P" is one line, not a hunt.
- **modeled stdout/stderr in bubbles** — Lands: pending feature.
  Raw write syscalls to fd 1/2 from bubble goroutines panic ("raw
  syscall 1 unsupported") — the refusal of unmodeled I/O is
  correct, but every workload reimplements the same idiom
  (in-memory trace, dumped via t.Log at failure), and the panic
  message costs each new consumer a debugging round. Model fd 1/2
  writes as a deterministic per-process buffer surfaced through the
  test log on failure; at minimum, the panic message should name
  the discipline ("stdout is unmodeled in bubbles; collect
  diagnostics in memory and dump via t.Log").
- **testlog buffer lock schedule coupling** — Lands: when the
  determinism sweep gains a testlog-contention leg (host goroutines
  hammering os.Open/Getenv while bubble goroutines run). The
  -test.testlogfile flush now takes the granted host-I/O path from
  bubble goroutines (no fence crash), but the shared bufio buffer's
  mutex can still park a bubble flush behind a HOST goroutine's
  wall-clock work — the same coupling the -v printer's lock-free
  bubble path was built to avoid; the testlog needs that treatment.
- **page-cache region reclamation (aggregate capacity)** — Lands: when a
  SUT legitimately holds more near-limit files than the mapping region
  carries (~8 at s_maxbytes; fewer under incremental doubling — the
  region is carved, never reclaimed). Growth of an IN-BOUNDS sibling
  then still dies with the mapping-reserve fatal s_maxbytes closed for
  the single-file class. No sound errno exists (a real kernel evicts
  page cache rather than failing), so the honest fix is reclamation or
  an eviction model; until then the loud fatal is the recorded shape.
- **densification-free durable images** — Lands: when a SUT syncs a
  near-limit sparse file. commitDataLocked copies node.data into an
  ordinary slice, so the sync densifies the sparse view and the harness
  dies as an UNTYPED kernel OOM kill — the least loud failure shape,
  same in-spec-input-kills-harness class s_maxbytes fixed for growth.
  Needs a sparse (or COW page-referencing) durable-image
  representation.
