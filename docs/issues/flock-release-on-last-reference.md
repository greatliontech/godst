# A simulated flock releases at fd close, not at the open file description's last reference

The flock model keys a lock's owner by `(host, process, fd)`
(`src/os/dst_flock_linux.go`, `dstFlockOwner`) and releases it on every
virtual-fd close (`src/os/dst_fd.go`, `dstFlockReleaseFD`). Linux keys a
BSD flock by the open file description and releases it when the
description's last reference drops — and a shared mapping of the file
holds a reference, as does a `dup` of the descriptor. So under the
simulator:

- a process that closes a locked file's descriptor while its
  `MAP_SHARED` mapping of the file is still live loses the lock at the
  close; on Linux the lock survives until the mapping goes too;
- a lock taken through one descriptor is released by closing that
  descriptor even when a `dup` of it is still open; on Linux it is not.

Demonstrated from pando (the consumer that surfaced it): pando's store
lock is an exclusive flock on the store file, and its close unmaps
before closing the descriptor. A probe that drops the unmap from the
close path — the descriptor closes, the mapping leaks — fails pando's
real-OS test (a reopen right after close refuses: the leaked mapping
still holds the lock) but would pass every DST schedule under this
model. No current pando schedule depends on the difference, because
pando always unmaps first and never dups the descriptor; the real-OS
suite carries the Linux semantics meanwhile.

What would close it: key flock ownership by the simulated open file
description, count its references (descriptors from `dup`/`dup2`/fork
inheritance, and each live shared mapping of the file), and release at
the last one — `munmap` and process-exit mapping teardown included —
with the DPOR happens-before release moved to that point.

Lands: when a godst change set next touches the flock model or the
fd-close and mapping-teardown release paths, or when a consumer
schedule depends on a lock outliving its descriptor through a mapping
or a duplicate (pando's store-lock schedules would then need it).
