# DST gmdb OpenRoot support

Lands: 4

## Gap

gmdb uses Go's rooted filesystem API on its open/create path. DST currently fences `os.OpenRoot` under a simulation run.

## Required outcome

`os.OpenRoot` and rooted file operations run over the in-memory per-host filesystem. Rooted path resolution cannot escape the opened root and never falls through to the host filesystem. Root operations preserve the existing DST path, metadata, durability, and host/process isolation contracts.
