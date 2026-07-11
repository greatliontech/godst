# Host crash misses a root-process goroutine inside another Host body

Lands: when root-process goroutines retain enough host ancestry for CrashHost
to kill the machine whose Host body they have not exited

## Gap

Severity H. A root-process goroutine can enter `Host("victim")` and then a
nested `Host("other")`. At the crash instant it has no process pid and is
stamped only with `other`, so both the pid-keyed and host-keyed victim scans
miss it. On leaving the nested body it restores the dead victim stamp and
continues executing on a powered-off machine.

## Required outcome

Host death kills every root-process thread dynamically belonging to that
machine, including nested-host extents, and preflight detects the run main in
the same shape. A test releases a nested body after CrashHost and proves no
post-body statement runs.
