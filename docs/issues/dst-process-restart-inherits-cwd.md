# Process restart inherits the predecessor's working directory

Lands: when last-invocation teardown removes process-owned cwd state while
preserving the host filesystem

## Gap

Severity M. Working directories are keyed by stable `(host, proc)` identity,
but `dstApplyProcessTeardown` never clears that entry. A same-name process
restart begins in the predecessor's directory instead of `/`, so relative
recovery I/O can target stale process-private state.

## Required outcome

Normal exit, explicit crash, and mapping-fault death reset cwd before a
same-name restart. Tests cover each death path and relative path resolution
from the fresh root cwd.
