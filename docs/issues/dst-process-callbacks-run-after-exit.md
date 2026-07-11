# Finalizers and cleanups can run after their process exits

Lands: when process exit and crash discard or otherwise contain every callback
owned by the dead invocation

## Gap

Severity H. Finalizer and cleanup metadata records only the run epoch and
registration sequence. It carries no process, pid, or host ownership, so
process teardown cannot identify callbacks queued by, or later discovered for,
the dead process. The root drain can execute such a callback after normal exit
or crash, under root identity, and mutate files or network state after the
process that registered it no longer exists.

## Required outcome

Callback ownership composes with process lifecycle: no callback from a dead
invocation executes after its exit boundary, and any discarded work remains
ledger-exact. Tests cover queued-before-crash and discovered-after-crash
finalizers and cleanups, plus normal process return.
