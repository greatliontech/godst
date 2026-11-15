# Raw and zoned block devices (`S_IFBLK`, `BLK*` ioctls, `O_DIRECT`, ZNS/SMR, `io_uring`)

**Lands:** deferred until triggered by user when `dataport`'s storage-node
extent-store specification settles (user-authorized deferral; zero retrofit to
existing `testing/simulation` signatures or filesystem-backed consumers).

## Context and deferral rationale

Filesystem-backed storage engines (`pando`, `tugboat/wal`, `protodb`) operate on
regular files, directories, `mmap`, `flock`, and `fallocate` over each simulated
host's in-memory POSIX VFS (`src/os/dst_fs.go`). By contrast, raw block-device
engines such as `dataport` bypass the filesystem and write directly to `/dev/nvme*`
or `/dev/sd*` block nodes (`S_IFBLK`) using `O_DIRECT` aligned `pread`/`pwrite`
(or `io_uring`), block `ioctl` geometry queries, trim/discard commands, and
optional ZNS/host-managed SMR sequential-zone state machines.

Currently in `godst`, `/dev/null` (`S_IFCHR`) is the only modeled device node,
`ioctl` is uniformly fenced (`design.md`, "The interception boundary"),
`O_DIRECT` is accepted without alignment checks, and power-loss tears (`CrashTear`)
operate at 4 KiB VFS page granularity rather than physical/logical block sectors.
Because `dataport`'s storage-node extent-store specification has not
yet settled whether v1 targets conventional `S_IFBLK` block devices, ZNS/FDP zones,
or `io_uring` vs `pread`/`pwrite`, building speculative kernel block/zone ABI
machinery prematurely risks modeling the wrong subset. Adding block devices later
is purely additive (`HostConfig` device declarations + `os`/`syscall` device
dispatch) and requires zero breaking changes to `testing/simulation` or existing
consumers.

## Scope when triggered

1. **Declarative per-host block devices (`HostConfig`):**
   - Allow declaring fixed-capacity block devices (for example `/dev/nvme0n1`) on a
     simulated `Host` with configurable capacity, logical sector size (`512` or
     `4096` bytes), physical block size (`4096` bytes), volatile write-cache flag,
     and optional zone geometry, backed by sparse 4 KiB copy-on-write pages so
     multi-terabyte logical capacities consume memory proportional only to written
     extents.
   - `Stat` / `Fstat` report `fs.ModeDevice` (`S_IFBLK` without `ModeCharDevice`) and
     `Seek(0, io.SeekEnd)` returns the fixed device byte capacity; writes at or past
     device capacity fail with `ENOSPC` (`0` growable EOF).
2. **Selected `S_IFBLK` `ioctl` dispatch:**
   - Dispatch read-only geometry queries (`BLKGETSIZE64`, `BLKSSZGET`, `BLKPBSZGET`,
     `BLKIOMIN`, `BLKIOOPT`) and range discard/zeroing (`BLKDISCARD`, `BLKZEROOUT`)
     specifically on simulated `S_IFBLK` virtual descriptors while keeping `ioctl`
     fenced on all other descriptors.
3. **`O_DIRECT` alignment and volatile drive write-cache durability:**
   - When opened with `syscall.O_DIRECT`, enforce Linux logical-sector alignment on
     file/device offset, transfer length, and user buffer address (`uintptr(ptr)`),
     failing unaligned calls immediately with `syscall.EINVAL`.
   - Distinguish **kernel page cache** from **drive volatile write cache**: an
     `O_DIRECT` write bypasses the OS page cache, yet on a device with a volatile
     write cache it reaches non-volatile media only upon `fdatasync`/`fsync` (which
     issues NVMe `Flush` / SCSI `SYNCHRONIZE CACHE`). Consequently, `CrashProcess`
     preserves uncommitted `O_DIRECT` writes (drive retains power), whereas
     `CrashHost` (power loss) may lose or sector-tear (`512 B` / `4096 B`)
     unflushed `O_DIRECT` writes independently and out of order across sectors.
4. **Optional ZNS / host-managed SMR zone model (if adopted by `dataport`):**
   - Per-zone state machine (`EMPTY`, `IMP_OPEN`, `EXP_OPEN`, `CLOSED`, `FULL`,
     `READONLY`, `OFFLINE`), sequential write-pointer (`WP`) enforcement returning
     `EIO` on unaligned writes, `BLKREPORTZONE` / `BLKRESETZONE` / `BLKFINISHZONE`
     ioctls, `max_active_zones` / `max_open_zones` caps, and power-loss `WP`
     advancement on torn zone appends.
5. **Optional deterministic `io_uring` subset (if adopted by `dataport`):**
   - Intercept `io_uring_setup` / `io_uring_enter` + shared SQ/CQ ring memory for
     `IORING_OP_READ` / `WRITE` / `READV` / `WRITEV` / `FSYNC` over virtual FDs,
     routing completions deterministically through the simulation scheduler and
     virtual disk-latency clock so out-of-order NVMe completion interleavings are
     explored reproducibly by seed.
