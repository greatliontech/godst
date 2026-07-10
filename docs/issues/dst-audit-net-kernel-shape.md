# DST audit: net divergences from kernel shape (close-RST)

Lands: chunk 23 of docs/plans/dst-audit-fixes.md (item 5 → 23; items 1–4 landed in chunks 20, 19, 21, 22)

## Gap

Severity M (full-surface audit, 2026-07-10; each reproduced). Kernel-shape
divergences in the virtual wire, all in the false-negative direction (a SUT
that keys on the real behavior is misled or a real failure is masked):

5. **`Close()` with unread inbound data FINs; real kernels RST**
   (`dstConn.Close`, `dst.go:552-559`). The kernel predicate `unreadInbound`
   (`dst_wire.go:317`) exists and process-exit teardown uses it
   (`dst_reset.go:163-175`), but user-level `Close` always closes gracefully;
   the peer reads EOF where production gets ECONNRESET. Unrecorded asymmetry —
   possibly the pending FIN/RST follow-on, but that text records only the
   write-after-peer-close leg.

## Required outcome

Each behavior either matches the production shape (horizon arms on any write
against an unhealable path; crashed-host dial blackholes to ETIMEDOUT; local
bind checks listeners; full backlog fails a deadline-less dial; Close with
unread data RSTs) or is recorded in the spec as a deliberately modeled limit
with its rationale. Items 1–4 are sound-direction bugs; item 5 is a
spec-amend candidate. Each pinned by a test asserting the chosen behavior.
