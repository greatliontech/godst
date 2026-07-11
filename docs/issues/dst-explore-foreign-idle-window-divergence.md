# DST explore: foreign work in a sim-idle window diverges replay (loud DST-L2-2 panic)

Lands: when the idle-window composition replays deterministically under
foreign churn, or its mechanism is diagnosed and recorded as a bounded,
loudly-reported limit

## Gap

Adjacent finding, chunk-5 test construction (2026-07-11); pre-existing
(reproduces with the pre-chunk recorder). Composition: an Explore whose SUT
sleeps (a window where NO simulation goroutine is runnable) while a foreign
synctest bubble churns runnable goroutines. The recorded schedule prefix then
diverges on replay — "a goroutine in the prefix was not enabled at its
decision" — and the run dies with the loud DST-L2-2 internal-error panic (not
a silent miss). Without the sleep window the same churn is trace-invisible
(TestExploreForeignBubbleSyncChurn); without the churn the sleep is fine, so
the divergence needs foreign work occupying a sim-idle window. Mechanism
undiagnosed: candidates include the bubble-time advance racing the foreign
churn at wall-clock timing (which side of the advance a decision lands on),
distinct from the -race yield-placement sensitivity but plausibly the same
family. Reproducer: TestExploreForeignBubbleSyncChurn's SUT with
`time.Sleep(time.Millisecond)` inserted between the two phases, run with the
rendezvous churn live.

Ruled out (2026-07-11): the sticky simulation-membership classification
(g.dstSimG — which resolved the -race foreign-yield sensitivity, including
making the run root a uniform simulation candidate) does NOT cure this
composition; the divergence reproduces unchanged with it landed. The
remaining suspects are the time-advance machinery itself (the advance step's
position racing foreign churn) and the timer-path transient
(time.go's bubble disassociation window).

## Required outcome

The composition either replays deterministically (foreign work cannot shift
which decision a prefix entry lands on), or the divergence is diagnosed and
recorded as a bounded, loudly-reported limit beside the ForeignSched
contract.
