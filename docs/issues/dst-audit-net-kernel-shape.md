# DST audit: net divergences from kernel shape (bind, backlog, close-RST)

Lands: chunks 21–23 of docs/plans/dst-audit-fixes.md (item 3 → 21, 4 → 22, 5 → 23; items 1–2 landed in chunks 20 and 19)

## Gap

Severity M (full-surface audit, 2026-07-10; each reproduced). Kernel-shape
divergences in the virtual wire, all in the false-negative direction (a SUT
that keys on the real behavior is misled or a real failure is masked):

3. **`Dialer.LocalAddr` / ephemeral allocation ignores listener bindings**
   (`dstLocalBindInUse`, `src/net/dst_reset.go:104-121`; `dstAllocateListenPort`,
   `dst.go:429`). Binding a dial's local end to a live listener's 2-tuple
   succeeds in-sim; production `bind(2)` fails EADDRINUSE. Same blindness both
   ways.

4. **A full accept backlog blocks a deadline-less dial forever**
   (`make(chan Conn, 128)`, `dst.go:765`; blocking send `dst.go:1002-1007`, no
   retransmit horizon on that loop). Production drops the SYN → connect
   ETIMEDOUT (~130s). Sim-only permanent hang, the class Soundness forbids.

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
