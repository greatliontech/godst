# DST gmdb process crash and restart

Lands: 11

## Gap

gmdb needs process crash and restart to validate recovery while the host filesystem and page cache survive. DST currently supports re-invoking `Process` as a logical restart pattern, but the `Crash` fault that kills an existing simulated process is pending.

## Required outcome

`Crash(process)` kills the named simulated process at a cooperative point, applies the process resource teardown contract, and leaves the host filesystem intact. A subsequent restart runs the process entry again with a fresh pid over the surviving host filesystem and clean process-owned resources.
