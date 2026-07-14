# faults.md documents HostConfig.IP; the landed HostConfig has no such field

faults.md's canonical API example (`simulation.HostConfig{IP: "10.0.0.1",
NumCPU: 4, Clock: simulation.Skew(50*ms)}`) and its "Per-host network
address space" section ("Each host has a routable IP (`HostConfig.IP`, or
deterministically assigned...)") name a configuration field the landed
`HostConfig` (testing/simulation/node.go) does not carry — it has only
Clock, Hostname, NumCPU. The example does not compile; a consumer following
the landed-marked section cannot configure a host IP at all (only the
deterministic `10.0.0.<id>` assignment plus the `HostIP(name)` query exist).

Spec-amend candidate (user ruling, spec-wins-by-default direction): land the
`IP` field (validated against the deterministic address scheme), or amend
faults.md to the deterministic-assignment-only contract and fix the example.
Either way, a compile-checked example (a test constructing the documented
config literally) prevents recurrence.

Lands: when HostConfig either gains the documented IP field or faults.md's
example and address-space section are amended to the assignment-only
contract, with a compile-checked example test.
