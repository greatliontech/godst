# Discarded callbacks inflate public executed metrics

Lands: when discarded finalizers and cleanups are accounted separately from
callbacks whose functions actually ran

## Gap

Severity M. `dstDiscardFinChainLocked` increments `finexecuted`, and
`dstDiscardCleanupBlock` increments `gcCleanups.executed`, for callbacks the
dead drain never invoked. The public runtime metrics therefore report discarded
callbacks as executed, contrary to both their metric definitions and the GC
contract's separate discard ledger.

## Required outcome

Queue exactness does not falsify execution metrics. A test kills the drain with
an early `runtime.Goexit` callback, compares callback side effects with both
executed metrics, and proves trailing discarded callbacks are not counted as
run.
