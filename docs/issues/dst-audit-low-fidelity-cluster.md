# DST audit: low-severity fidelity and hygiene divergences

Lands: chunks 4, 9, 11–17, 24, 25, 28, 29 of docs/plans/dst-audit-fixes.md (per item; chunk 29 is the last)

## Gap

Severity L/nit (full-surface audit, 2026-07-10). A cluster of small,
deterministic divergences from host behavior and minor hygiene defects, each
verified, none blocking:

- **Untagged zero-footprint claim overstated.** `finalizer` (mfinal.go) grew
  5→6 words and `cleanupFn` (mcleanup.go) 3→4 words unconditionally, so
  untagged builds carry a dead `dstSeq` word and fit fewer entries per block;
  `NumCPU` (debug.go:267) branches on a runtime var, not a build const, so it
  is not dead-code-eliminated untagged. No behavior change; the spec's "zero
  footprint" note is inaccurate.

- **fsync-EIO model kinder than post-fsyncgate Linux.** A faulted fsync does
  not advance the durable image and a heal resumes I/O (disk_fault.go:53-56,
  per faults.md:546/879); on Linux ≥4.13 an fsync EIO marks pages clean and a
  retried fsync succeeds without the data reaching disk. A DB whose recovery is
  "retry fsync after EIO" passes here but loses data in production. Spec
  explicitly chose the kinder model — spec-amend candidate for the user's call.

- **`Listen(":0")` port allocator never wraps or reclaims**
  (`dstAllocateListenPort`, dst.go:429-445): ~55k listens in one run exhaust
  "no free ports" even with every listener closed; real kernels wrap and reuse.

- **Same-host connections get an unbounded send buffer and no horizon**
  (dst.go:986): two co-located peers each writing ≫1 MiB before reading
  deadlock in production but succeed in sim (masks a real deadlock, unbounded
  sim memory). Recorded in a code comment, not the spec — spec-amend candidate.

- **Proc-overlay `Fd()` panics** (dst_fd.go:90-92), making the spec's proc-fd
  identity contract ("zero (st_dev, st_ino)") unreachable; the dead
  `dstFDFstat` branch would report `Dev = host+1`, not zero
  (dst_fd_stat_linux.go:42-44). Spec-vs-code contradiction.

- **Directory `Seek` at nonzero returns EISDIR** (dst_fs.go:1369-1372); Linux
  permits lseek on a directory fd.

- **16K-page / VA-39 hosts refused at first file creation, not first mapping.**
  `dstPageCacheCheckHost` runs in `dstPageCacheNew` at every regular-file
  creation (dst_pagecache_linux.go:162-169; os/dst_pagecache_linux.go:93), so
  on Asahi/Apple-VM (16K pages) or Raspberry Pi OS (VA_BITS=39, reservation at
  0x5a00_0000_0000 > 2^39) the first `os.Create` throws. Loud and
  deterministic, but the spec words the refusal inside the mapping paragraph;
  the effective scope is every dst file op — surface it so the capability claim
  matches reality.

- **nits:** a periodic fake timer whose `when == bubble.now` exactly at the
  `DriftClock` instant keeps its old rate (dst.go dstRemapHostTimers);
  `dstMMapEntry.seq` is written, never read, with a comment describing
  nonexistent tie-breaking (dst_mmap_linux.go:57,176,183); Explore misattributes
  fan-out overflow as BudgetHit under `MaxSteps` (explore.go:452-455); a blocked
  flock waiter whose fd is closed elsewhere wakes EBADF where Linux grants
  (dst_flock_linux.go:27); raw `syscall.Pwrite` on an O_APPEND file honors the
  offset (Linux appends); failed/zero writes bump mtime (subsumed by the
  refused-write-grows issue).

## Required outcome

Each item is either corrected to match host behavior or recorded in the spec as
a deliberate modeled limit with rationale. The proc-fd `Fd()` contradiction and
the zero-footprint claim are the two that touch stated contracts; the rest are
fidelity notes.
