# DST gmdb end-to-end compatibility coverage

Lands: 14

## Gap

The individual syscall and fault features need an integration check that gmdb's actual open, locking, liveness, mmap, and recovery paths run under DST without host passthrough or unsupported fences.

## Required outcome

The test surface includes an end-to-end gmdb compatibility run, or a representative harness that exercises the same raw surface if gmdb cannot be vendored. Coverage includes database open/create, read-only data mmap, directory and file durability barriers, single-writer flocking, shared lock-file mmap coordination, pid liveness recovery, virtual clock reads, and crash/restart recovery over the durable image.
