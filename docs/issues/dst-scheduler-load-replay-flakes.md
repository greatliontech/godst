# Same-seed scheduler probes can diverge under host contention

Lands: the foreign-bubble, PCT non-bubble-creation, exploration-sweep, and network-reset-order probes replay identically under repeated focused and contended full-suite runs, with every load-dependent input diagnosed and eliminated

## Gap

Severity H. A tagged runtime gate running alongside architecture and standard-
library builds observed four same-seed schedule divergences:

- `TestDSTForeignBubbleIsolation` produced identical loaded runs but a different
  isolated run.
- `TestDSTPCTNonBubbleCreation` produced one different loaded PCT schedule.
- `TestDSTExploreSweep` reported that a recorded schedule prefix was not enabled
  during replay.
- `TestDSTNetResetOrderDeterministic` produced different reader wake orders for
  two seed-4 runs during a contended tagged gate; an immediate focused count-20
  rerun passed.

Immediate focused sequential reruns of the affected tests passed. The failures
are intermittent, but each directly violates the same-seed replay contract and
shows that host contention can still reach scheduler or exploration inputs.

## Required outcome

Identify and remove each load-dependent input. All four probes must replay
identically in repeated focused runs and while the tagged gate runs under host
contention. Regression coverage must reach the diagnosed sources rather than
only increasing repetition counts.
