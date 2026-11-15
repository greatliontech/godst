# Simulated Unix domain sockets (`unix` / `unixpacket` over per-host VFS `S_IFSOCK` inodes)

**Lands:** deferred until triggered by user when a workload requires local IPC
over `AF_UNIX` sockets (user-authorized deferral; zero retrofit — additive
unfencing in `src/net/` and `src/os/`).

## Context and deferral rationale

`godst` currently fences `"unix"`, `"unixgram"`, and `"unixpacket"` in `src/net/`
(`design.md`). Local single-host and co-pod communication in `pando`, `tugboat`,
and `protodb` uses either loopback TCP (`127.0.0.1`) or shared host files, so
`AF_UNIX` is not required for those subsystems. Supporting Unix domain sockets
later requires zero changes to `testing/simulation`'s public API: it is a purely
internal coordination seam between `src/net/` and `src/os/`'s per-host filesystem
tree.

## Scope when triggered

1. **Per-host VFS socket inodes (`src/os/` + `src/net/`):**
   - Binding a `"unix"` (`SOCK_STREAM`) or `"unixpacket"` (`SOCK_SEQPACKET`)
     listener creates a `fs.ModeSocket` (`S_IFSOCK`) directory entry in the calling
     goroutine's host VFS tree (respecting parent-directory existence, `os.Root` /
     `cwd` relative path resolution, and Linux `sun_path` length limits), failing
     with `syscall.EADDRINUSE` if the path already exists.
   - Closing the `net.Listener` stops accepting new dials (`ECONNREFUSED` on the
     stale socket inode) while leaving the filesystem entry present unless unlinked
     (or matching Go `net.UnixListener`'s default unlink-on-close behavior when
     created by `net.Listen`), and `os.Remove` unlinks the path without severing
     already-established connections.
   - On `CrashHost`, unsynced `S_IFSOCK` directory entries revert with parent
     directory durability, while durable `S_IFSOCK` entries survive as dead socket
     files (`ECONNREFUSED` until `os.Remove` + re-bind), matching real Linux local
     daemon recovery hazards.
2. **Transport and isolation semantics:**
   - Reuse the in-memory `dstWireEnd` transport for `"unix"` stream sockets (zero
     link latency, bounded send buffer backpressure, half-close `CloseWrite` /
     `CloseRead`, unread-data `RST` vs `FIN` on `Close`/exit/crash) and preserve
     record boundaries for `"unixpacket"`.
   - Enforce strict host-local isolation (`DST-NODE-ISOLATION`): dialing `/run/app.sock`
     on host `"h1"` resolves only against `"h1"`'s VFS tree and can never reach a
     listener bound at `/run/app.sock` on `"h2"`. Inter-host network partitions
     (`Partition`, `IsolateHost`) never affect host-local Unix domain sockets.
