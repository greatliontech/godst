# DST gmdb host crash and restart

Lands: 12

## Gap

gmdb needs host crash and restart to validate recovery from power loss and kernel failure. DST has per-host filesystem durability images, but `CrashHost` and restart are pending.

## Required outcome

`CrashHost(host)` kills every process on the host, applies process resource teardown for each victim, resets host-owned volatile resources, and restores the host filesystem from the durable image. Restart re-establishes the host and lets processes reopen the recovered on-disk image with fresh process resources.
