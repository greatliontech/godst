# Concurrent same-name Process starts can acquire two hosts

Lands: when different-host liveness validation and process registration are one
atomic operation

## Gap

Severity H. `activeProcLivesElsewhere` checks under `activeProcs.mu`, releases
it, and `Process` registers later under `procTeardownMu`. Level-2 scheduling can
let two same-name starts on different hosts both pass before either registers.
The single `activeProcs.host` value then describes only the last writer, so a
host crash can spare the other live invocation.

## Required outcome

One logical process has at most one live host at every reachable schedule. A
DPOR test races two starts and requires one refusal without state publication,
then crashes each candidate host and verifies victim completeness.
