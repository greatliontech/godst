# DST audit: host crash lets the surviving peer drain in-flight bytes before ECONNRESET

Lands: chunk 18 of docs/plans/dst-audit-fixes.md

## Gap

Severity H (full-surface audit, 2026-07-10; reproduced). `dstResetHost`
(`src/net/dst_reset.go:128-130`) matches only `c.localHost == h`, resetting the
victim's end of each connection; `resetConn` on that end presents as a graceful
write-close to the surviving peer, whose `Read` drains every queued segment —
including bytes in flight at the crash instant — and maps EOF to ECONNRESET
only after the drain (`src/net/dst.go:538-540`). Reproduced: A writes 3 bytes
onto a 100ms link, `CrashHost("A")` mid-flight, B reads `n=3, err=nil`, then
ECONNRESET on the second read. faults.md (Connection reset) requires
"a subsequent read returns ECONNRESET before draining — in-flight bytes are
dropped, as a real RST discards them (DST-FAULT-SOUND)", and the CrashHost doc
promises every conn is "RESET at its peer". The simulation delivers bytes a
powered-off machine's teardown destroyed — false-negative direction for any
SUT whose correctness depends on unacknowledged data being lost on crash.
`Reset(a,b)`/`ResetProcess` are unaffected (they match both ends).

## Required outcome

A host crash resets each of its connections at the surviving peer with RST
semantics: queued and in-flight bytes are discarded, the peer's next read
returns ECONNRESET without draining. Pinned by a test that crashes the writer
host with bytes in flight and asserts the peer's first read fails.
