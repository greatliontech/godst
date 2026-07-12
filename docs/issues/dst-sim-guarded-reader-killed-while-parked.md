# DST sim: a caller-gate reader killed while parked strands the gate

Lands: when a goroutine killed inside a guarded extent cannot strand its
callerGate release (release before any kill-exposed park, or the kill path
drains stranded readers), with the interleaving pinned

## Gap

Severity M (review-found 2026-07-11, adjacent — the park and the kill paths
predate the gate; the gate adds the hang consequence). Reachable shape:

- Goroutine A of process p1 calls Crash("p2"): it holds callerGate.RLock and
  parks at procTeardownMu because B holds that mutex inside crashHost("h") —
  the host owning p1. B's dstMarkHostGoroutinesCrashed(h) marks A while A is
  sema-parked; the sema dequeue skips crashed waiters, so A never resumes
  and its deferred RUnlock is stranded. leaveSimulation's callerGate.Lock
  then blocks forever — the run hangs at deactivation.
Concurrent fault APIs are exactly what procTeardownMu exists to serialize,
so the interleaving is in-spec reachable.

## Required outcome

A killed-while-parked guarded reader cannot strand callerGate: the guarded
extent releases before any park a kill can reach, or the crash-marking path
accounts for gate readers it kills. The caller-gate comment in simulation.go
then replaces its tracked-open note with the enforced property.
