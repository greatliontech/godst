# Crash-recovered rename aliases double-count disk capacity

Lands: when resident-byte accounting counts each reachable file node once,
including crash-tear alias outcomes

## Gap

Severity M. Crash tear can recover both old and new names of one renamed inode.
`dstFSDisk.residentLocked` recursively sums every directory entry under the
assumption that a node has one name, so the aliased file is counted twice and a
write can receive false `ENOSPC` despite sufficient capacity.

## Required outcome

Capacity measures unique resident regular-file content for every filesystem
image the crash model can produce. A seed sweep recovers both aliases, verifies
`SameFile`, and proves a write fitting the unique-byte budget succeeds.
