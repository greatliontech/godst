# gcAssistAlloc's DST membership carry has no enforcing test

`gcAssistAlloc`'s bubble-disassociation block carries simulation membership
across the disassociation (`dstGCInternal` stands in for `dstSimG` while
`gp.bubble` is nil), exactly like `gcStart`'s. The `gcStart` twin is
enforced (`TestVerboseOnOffSameSeedTranscript` fails when it is dropped);
the `gcAssistAlloc` twin survives the full tagged short runtime suite, the
simulation short suite, and the determinism sweep when dropped. The path is
real — it was added for a demonstrated fault (intermittent exploration
prefix divergence under GC/finalizer churn; see the canonicalize-exploration
-scheduling fix in history via `git log -S "gp.dstGCInternal = gp.dstSimG"`)
— but reaching it requires a bubble goroutine to take an allocation assist
inside the concurrent-mark window, a timing the current suites never
deterministically produce.

Closing this needs a deterministic way to route an in-sim bubble goroutine
through the assist disassociation — steering the mark window and assist
debt, with reachability made observable (an export-test counter on the
branch), so the enforcing test cannot pass vacuously.

Lands: user decision — the enforcing test is the deferred work itself, so no
passive trigger exists; scheduling the assist-steering design is the user's
call. When built, its reachability must be asserted via an export-test
observable, not inferred.
