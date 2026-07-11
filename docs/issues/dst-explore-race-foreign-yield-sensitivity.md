# DST: -race exploration yield placement is foreign-sensitive (coverage shrinks under churn)

Lands: when the dst-race yield placement's foreign sensitivity is diagnosed
and either removed or recorded in the spec as a bounded, reported limit

## Gap

Severity M (found 2026-07-10 while landing the foreign-livelock fix;
reproduced, mechanism undiagnosed). Under `-race` (dst-race
auto-instrumentation), an exhaustive exploration of a race-free finalizer
workload explores 12 schedules foreign-free but only 6 with two foreign
Gosched spinners churning, and single-episode traces diverge alone-vs-spun —
while without `-race` the same shapes are byte-identical
(`TestExploreForeignSpinner`, `TestExploreForeignSpinnerDrainCallback`). The
foreign goroutines never enter recorded schedules or enabled sets (pinned),
so the sensitivity is in where the auto-instrumented yield points fall —
candidate mechanisms: the shared-address promotion filter's cross-goroutine
observations, TSan-side state, or safe-point guard interactions — not in the
selection seam itself.

Before the foreign-livelock fix this composition livelocked outright
(unconditional infrastructure-first starvation), so no coverage claim was
possible at all; the fix made it complete, and `ExploreResult.ForeignSched`
now reports foreign presence at simulation decisions and downgrades
`Exhausted` — the loss is loud, not silent. This issue tracks the root cause:
why instrumented yield placement depends on foreign activity, and whether it
can be made insensitive (restoring full-coverage claims under churn) or must
be recorded as a bounded limit.

Also in scope: the silent-replay-miss path. `Failure` carries no enabled
sets, and the replay-divergence check aborts only when a prefix entry names a
non-enabled goroutine — under `-race` with churn, a shifted auto-yield can
keep every prefix seq enabled while silently executing a different
interleaving, returning `failed=false` with no diagnostic.
`Failure.ForeignSched` marks such replay tokens best-effort; the root-cause
work must either make replay divergence detectable in this regime or record
the limit.

Additional scope (foreign-bubble membership handoff, 2026-07-11): the
membership gates on the -race recording surfaces are currently unpinnable —
TestExploreForeignBubbleSyncChurnRace's log-cleanliness assertions are nets
that cannot fire under a gate regression because the shape shields its own
doors: foreign rendezvous wakes set the conservative filter flag (the edge
degrade) before any foreign access reaches dstAccessShouldYield, routing
every auto access to the pending path, which never commits for an
infra-picked goroutine. An EDGE-FREE foreign shape (instrumented-write churn
with no rendezvous, so the flag stays clear) should open the inline-commit
door (dstAccessMaybeShared false → early false → dstCommitAccess with
seq 0), and a rendezvous shape with the sync-event degrade reverted should
buffer seq-0 events — neither was reproduced within the membership chunk's
budget (its two-advance causality harness proves mid-episode foreign
execution and is reusable). This work owes those door-reaching arms, or an
explanation of why the doors stay shut.

Diagnosis progress (2026-07-11, partial — evidence-backed, mechanism not yet
closed):

- Reproduced 12-vs-6 schedules on the finalizer workload (Exhaustive, -race).
  The first divergence is in the ENABLED SETS, not yield placement per se: at
  decision 3 the main goroutine is enabled alone but still blocked under
  churn — it sits in runtime.GC(), whose completion needs GC mark-worker
  infra slots, and foreign churn competes for the infra alternation. SUT
  blocking points released by INFRASTRUCTURE progress are the sensitive
  class; pure chan/mutex workloads are byte-identical (the spinner pins).
- The alone run ALSO reports ForeignSched=true (and so Exhausted=false) on
  this workload: candidacy probes attribute it to two long-lived goroutines
  (stable goids across episodes) with startpc = the runOnce bubble-body
  closure (runOnceResultLocked.func2), status _Grunnable, waitreason 0,
  bubble nil, seq 0 — candidates at EVERY decision of later episodes. They
  look like prior-episode bubble mains created but never run (or never
  reaped), leaking as permanent foreign-classified runnable zombies — i.e.
  the harness supplies its own churn, which both poisons ForeignSched
  (always true for this workload class under -race) and may itself displace
  infra slots (the leaked zombies are spinner-equivalent). Where they leak
  from is the open question (suspects: aborted/truncated episodes leaving a
  newborn main unscheduled; the -race access-yield path interacting with
  episode teardown).
- Access logs alone-vs-churned are same-length with identical PC streams but
  different heap ADDRESSES from entry 3 on (layout shift) — the
  address-keyed filter surfaces are designed address-insensitive, so this is
  noise, not mechanism.
- Probe recipes (all rerunnable): candidacy prints at proc.go's foreign-PICK
  branch and dst_explore.go's foreign-CAND branch (print goid +
  funcname(findfunc(gp.startpc))); go test needs -v or output is swallowed
  on pass. A scratch white-box test reproducing 12-vs-6 lived at
  testing/simulation/zz_diag_test.go in the working tree during diagnosis
  (git log of this doc's commit has the recipe inline below).

  Reproducer sut: finalizer sends ch1, Gosched, sends ch2; body registers
  finalizer, runtime.GC(), <-ch1, wg.Wait on a goroutine reading ch2; two
  bare Gosched spinners as churn; compare Explore(1, Exhaustive) schedule
  counts and runOnce traces.

Next steps: (1) find the zombie mains' origin (probe episode teardown for
unreaped runnable bubble goroutines; check the abort path); (2) re-measure
the 12-vs-6 with the zombies eliminated — the GC-slot mechanism may shrink
or vanish; (3) then decide fix-vs-record per the Required outcome, and build
the chunk-5 door-reaching arms with the same scaffolding.

## Required outcome

Under `-race`, either exploration coverage is foreign-insensitive (the
spinner tests' trace-equality assertions extend to race builds and the
`ForeignSched` downgrade can be narrowed), or the spec records the
sensitivity as a deliberate bounded limit with its mechanism named, with
`ForeignSched` remaining the reporting surface.
