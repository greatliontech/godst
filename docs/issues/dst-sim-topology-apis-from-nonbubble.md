# DST sim: Host/Process from a non-bubble goroutine mid-run mutate topology nondeterministically

Lands: when Host and Process gain the fault APIs' caller-position guard, or the
declaration-API caller contract is recorded as the user's responsibility

## Gap

Adjacent finding, chunk-27 review (2026-07-11). The fault APIs now panic when
called during an active run from outside the run's bubble, but `Host` and
`Process` — declaration APIs that mutate run state — do not: a mid-run `Host`
re-declaration relays the host-up op (functionally a reboot, HealHost-plus) and
re-establishes the host clock; `Process` starts SUT goroutines outside the
bubble. A pre-run goroutine calling `Host("h", ...)` mid-run reboots the
machine at a wall-clock instant the seed does not control — the same
silent-nondeterminism class the fault guard kills, through a declaration API.

## Required outcome

Host/Process invoked during an active run from outside the run's bubble fail
loudly like the fault APIs, or the spec records the declaration-API caller
contract explicitly.
