# DST explore: the -race membership-gate doors lack reaching test arms

Lands: when an edge-free foreign shape opens the inline-commit door (or its
impossibility is explained), pinning the access-gate and sync-event-degrade
membership terms under -race

## Gap

Severity L. The membership gates on the -race recording surfaces are
unpinnable in the existing shapes: TestExploreForeignBubbleSyncChurnRace's
log-cleanliness assertions cannot fire under a gate regression because the
rendezvous shape shields its own doors — foreign wakes set the conservative
filter flag (the edge degrade) before any foreign access reaches
dstAccessShouldYield, routing every auto access to the pending path, which
never commits for an infra-picked goroutine. An EDGE-FREE foreign shape
(instrumented-write churn, no rendezvous, so the flag stays clear) should
open the inline-commit door (dstAccessMaybeShared false → early false →
dstCommitAccess with seq 0), and a rendezvous shape with the sync-event
degrade reverted should buffer seq-0 events; neither arm was landed. The
two-advance-causality harness in that test (foreign progress counter proving
mid-episode foreign instrumented execution) is reusable.

## Required outcome

The two -race membership mutants (dstYieldAccess gate reverted; the
dstRecordSyncEventForGID seq==0 degrade reverted) each have a killing test,
or the doors' unreachability is explained and recorded at the test's
coverage-cap comment.
