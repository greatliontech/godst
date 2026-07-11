# Process restart inherits environment mutations

Lands: when last-invocation teardown discards the process environment view and
a restart copies the run-entry host baseline

## Gap

Severity M. The simulated environment map is keyed by stable logical process
id and reset only between runs. `Setenv`, `Unsetenv`, and `Clearenv` mutations
therefore survive normal exit or crash into a same-name process restart,
although environment is process-owned state and only host state survives.

## Required outcome

Every fresh process invocation starts from the documented host environment
baseline. Tests cover all three mutation forms across normal exit and crash.
