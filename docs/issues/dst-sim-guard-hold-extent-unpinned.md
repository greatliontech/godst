# DST sim: the per-API caller-gate hold extent has no killing tests

Lands: when a site-local release regression (guard-then-release-immediately
at any guarded API, or a release hoisted above the API's last mutation) fails
a test, or the per-helper enforcement bound is recorded at the guards

## Gap

Severity M (audit-found 2026-07-11; missing-test finding, not a code
defect). The activation/deactivation edge tests exercise the gate through
the guard HELPERS (called directly) and a raw RLock; no test drives an
actual guarded API across an activation edge. Rewriting any of the 17 fault
sites from `defer requireBubbleFaultCaller("X")()` to an immediate release —
or hoisting Host's release above the host-up relay, or Process's above
activeProcSet — passes every test while readmitting the contracted-away
tear at that site: a foreign pre-run call passes the guard, releases, Run
activates, and the op mutates the new run at wall-clock timing. The spec's
"holds from its check through its state mutation" is enforced per-helper,
not per-API.

## Required outcome

A harness that pins at least one representative per pattern class (defer
site, closure site, Host, Process) holding across a delayed activation — or
the guards' comments record that the hold extent is convention enforced by
review, with the mutation shapes named.
