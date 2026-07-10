# DST: explore recording surfaces admit foreign-bubble goroutines

Lands: when explore recording keys on sim-bubble membership at the seq/access
chokepoints (`dstEnsureSeq`, `dstYieldAccess`, `dstRecordReadyEdge`,
`dstApplyLiveSyncEvent` gates)

## Gap

Severity M (found 2026-07-10 during the foreign-livelock fix's mutation
testing; mechanism cited, not reproduced end-to-end). The Level-2 explore
recording gates check `gp.bubble == nil` / `bubble == nil`
(`src/runtime/dst_explore.go:61`, `:903`, `:948`, `:1031`), not membership in
the ACTIVE simulation's bubble (`bubble == dstSimBubble`). A foreign synctest
bubble live concurrently with an `Explore` therefore passes them: its
goroutines receive stable indices from `dstEnsureSeq` (consuming `dstSeqCtr`
values at foreign-timing-dependent points, so a simulation goroutine's index
can differ across episodes), commit their instrumented accesses into the DPOR
access log, and merge into the happens-before clocks. Foreign-bubble seqs also
persist on the g across episodes while `dstSeqCtr` resets
(`dstClearSchedState` runs only for bubble-created goroutines), so a foreign
goroutine can carry a seq that collides with a simulation goroutine's in a
later episode.

Related: `dstScheduledSelect`'s stable-index loop filters infrastructure
candidates (`dst_explore.go`, the `dstIsInfraCandidate` skip before
`dstEnsureSeq`), and that filter is pinned per-call-site
(`TestExploreForeignSpinner` traces a fresh-spinner episode, where a leaked
assignment deterministically shifts the late goroutine's index). The durable
enforcement is still structural: refuse out-of-sim goroutines at the
`dstEnsureSeq` chokepoint (auditing every caller's zero-seq handling — the
sim bubble's drain legitimately needs a seq on the access/edge paths, foreign
bubbles must degrade to the conservative fallbacks) rather than filtering at
each call site, closing the access/edge-path doors no call-site filter
covers.

## Required outcome

A foreign synctest bubble concurrent with an exploration cannot perturb
recorded schedules, stable-index assignment, the DPOR access log, or the
happens-before clocks: the recording surfaces key on membership in the active
simulation's bubble, enforced at the chokepoints so no per-call-site filter
can silently regress.
