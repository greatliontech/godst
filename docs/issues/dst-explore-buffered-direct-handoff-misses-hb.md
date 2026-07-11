# Buffered-channel direct handoff omits slot-reuse happens-before events

Lands: when the empty-buffer waiting-receiver send path records the same slot
release/acquire relation as the race detector

## Gap

Severity L. The buffered-channel direct handoff path in `runtime.send` performs
the race detector's two slot notifications but emits no corresponding DST
sync events. A receiver's prior slot release can therefore be absent from the
offline clocks of the next direct sender. DPOR treats ordered accesses as
concurrent, adds redundant reversals, and can consume a caller's schedule
budget on classes the channel happens-before relation already fixes.

## Required outcome

Every buffered slot reuse path records one consistent HB relation. A targeted
capacity-one test forces the waiting-receiver handoff and proves the protected
accesses are ordered in the DST clocks.
