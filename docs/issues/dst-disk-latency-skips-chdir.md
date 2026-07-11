# SlowDisk does not delay named Chdir

Lands: when Chdir pays one disk-touching path traversal delay

## Gap

Severity L. `dstChdir` resolves a named directory under the filesystem lock but
does not call `dstDiskDelayHere`. A slow disk therefore reports zero virtual
time for Chdir while equivalent named path walks pay the configured per-op
latency.

## Required outcome

Chdir sleeps once outside the tree lock before resolving the path. The disk
latency operation table includes Chdir while retaining Getwd as an undelayed
in-memory control.
