# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number when an active plan exists, a self-contained condition,
"pending feature" for planned roadmap work, or an explicit user-authorized
trigger when deferred until a downstream consumer begins active development). At
close-out, lasting rationale is promoted into a kept-current artifact and the
resolved entry is deleted.

## Open

- **raw and zoned block devices (`S_IFBLK`, `BLK*` ioctls, `O_DIRECT`, ZNS/SMR, `io_uring`)** —
  Lands: deferred until triggered by user when `dataport`'s storage-node
  extent-store specification settles (user-authorized deferral; zero retrofit to
  existing `testing/simulation` signatures or filesystem consumers). Models raw
  block device nodes (`HostConfig` device declarations), `BLKGETSIZE64` /
  `BLKSSZGET` / `BLKPBSZGET` / `BLKDISCARD` ioctls, strict `O_DIRECT` buffer and
  offset alignment, volatile drive write-cache loss across `CrashHost` (distinct
  from kernel page-cache survival across `CrashProcess`), sector-granular torn
  writes, optional ZNS/SMR sequential write-pointer and open-zone enforcement,
  and a deterministic `io_uring` block-I/O subset. See
  [raw-and-zoned-block-devices.md](raw-and-zoned-block-devices.md).
- **simulated Unix domain sockets (`unix` / `unixpacket` over per-host VFS `S_IFSOCK` inodes)** —
  Lands: deferred until triggered by user when a workload requires local IPC over
  `AF_UNIX` sockets (user-authorized deferral; zero retrofit — additive unfencing
  in `net` and `os` reusing `dstWireEnd` and per-host VFS trees). Models stream and
  seqpacket Unix domain sockets bound to per-host filesystem `ModeSocket` entries,
  enforcing `EADDRINUSE` until `os.Remove`, host-local isolation, and `CrashHost`
  socket durability semantics. See
  [unix-domain-sockets.md](unix-domain-sockets.md).
- **declarative sub-host process UTS and root-directory overrides (`ProcessConfig.Hostname` / `RootDir`)** —
  Lands: deferred until triggered by user if a co-pod container workload requires
  distinct hostnames or chroot trees on a shared host filesystem (user-authorized
  deferral; zero retrofit — additive fields on `ProcessConfig`). Records why raw
  Linux `unshare`/`setns`/`mount` syscalls remain permanently out of scope (`Host`
  already models full machine/container isolation and `ProcessConfig.PIDNamespace`
  models shared-disk PID namespace isolation) while preserving the declarative
  `ProcessConfig` path for sub-host UTS or root-path overrides. See
  [sub-host-process-uts-and-rootdir.md](sub-host-process-uts-and-rootdir.md).
