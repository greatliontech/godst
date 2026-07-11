# Same-seed scheduler probes can diverge under host contention

Lands: the foreign-bubble, PCT non-bubble-creation, and exploration-sweep probes replay identically under repeated focused and contended full-suite runs, with every load-dependent input diagnosed and eliminated

## Gap

Severity H. A tagged runtime gate running alongside architecture and standard-
library builds observed three same-seed schedule divergences:

- `TestDSTForeignBubbleIsolation` produced identical loaded runs but a different
  isolated run.
- `TestDSTPCTNonBubbleCreation` produced one different loaded PCT schedule.
- `TestDSTExploreSweep` reported that a recorded schedule prefix was not enabled
  during replay.

One immediate focused sequential rerun of all three tests passed. The failures
are intermittent, but each directly violates the same-seed replay contract and
shows that host contention can still reach scheduler or exploration inputs.

## Required outcome

Identify and remove each load-dependent input. All three probes must replay
identically in repeated focused runs and while the tagged gate runs under host
contention. Regression coverage must reach the diagnosed sources rather than
only increasing repetition counts.
