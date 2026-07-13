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
- [root-rename-host-surface-divergences.md](./root-rename-host-surface-divergences.md) —
  `dstRootRename` diverges from the host `os.Root.Rename` surface on
  existing-directory targets (host `EEXIST`, sim replaces/no-ops) and on
  new-final-assert-vs-old-missing ordering (host-probed matrix in the doc).
  Lands: when dstRootRename is aligned with the host rooted-rename
  surface's probed matrix.
- [fault-reset-drop-vs-kernel-drain-soundness.md](./fault-reset-drop-vs-kernel-drain-soundness.md) —
  the injected-reset/crash faults destroy a survivor's already-delivered
  bytes, an execution a real kernel cannot produce (tcp_recvmsg drains
  before reporting the socket error); whether the fault layer should model
  drain-then-reset for survivors is an open Soundness call. Lands: when the
  fault-layer drain semantics question is ruled on.
- [verbose-transcript-divergence-under-host-load.md](./verbose-transcript-divergence-under-host-load.md) —
  `TestVerboseOnOffSameSeedTranscript` diverged once when the three dst
  packages ran concurrently on a loaded host, passes alone — an
  undiagnosed same-seed determinism escape, plausibly the entersyscall
  window reached another way. Lands: with the determinism-escape sweep,
  alongside the capability-write entersyscall window item.
