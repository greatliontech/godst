# DST gmdb proc starttime and namespace identity

Lands: 8

## Gap

gmdb reads `/proc/<pid>/stat` field 22 to disambiguate PID reuse and reads `/proc/self/ns/pid` to identify PID namespaces. DST currently has no synthetic `/proc` filesystem for simulated processes.

## Required outcome

The simulated filesystem exposes enough synthetic `/proc` surface for gmdb liveness recovery: `/proc/<pid>/stat` includes a deterministic process starttime in field 22 for live simulated processes, and `/proc/self/ns/pid` readlink returns a deterministic namespace identity for the calling simulated process context. Unsupported `/proc` paths remain deterministic unsupported or not-exist results rather than host passthrough.
