# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number when an active plan exists, a self-contained condition, or
"pending feature" for planned roadmap work). At close-out, lasting rationale is
promoted into a kept-current artifact and the resolved entry is deleted.

## Open

- [root-rename-host-surface-divergences.md](./root-rename-host-surface-divergences.md) —
  `dstRootRename` diverges from the host `os.Root.Rename` surface on
  existing-directory targets (host `EEXIST`, sim replaces/no-ops) and on
  new-final-assert-vs-old-missing ordering (host-probed matrix in the doc).
  Lands: when dstRootRename is aligned with the host rooted-rename
  surface's probed matrix.
- [mutex-starvation-handoff-livelock-diagnostic.md](./mutex-starvation-handoff-livelock-diagnostic.md) —
  the fake-clock mutex starvation measurement is sound for every finite
  prefix but not for liveness: a SUT whose termination depends on the 1ms
  starvation-handoff flip livelocks in-sim, undetectably (non-durable mutex
  waits never advance fake time). Open question: detect the shape with a
  loud diagnostic, model the flip on virtual decisions, or leave the gap
  recorded. Lands: when the starvation-handoff diagnostic question is
  ruled on.
- [fault-reset-drop-vs-kernel-drain-soundness.md](./fault-reset-drop-vs-kernel-drain-soundness.md) —
  the injected-reset/crash faults destroy a survivor's already-delivered
  bytes, an execution a real kernel cannot produce (tcp_recvmsg drains
  before reporting the socket error); whether the fault layer should model
  drain-then-reset for survivors is an open Soundness call. Lands: when the
  fault-layer drain semantics question is ruled on.
