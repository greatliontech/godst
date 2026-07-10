# DST audit: page-cache memfds live in ordinary fd numbers and are killable by SUT close

Lands: chunk 6 of docs/plans/dst-audit-fixes.md

## Gap

Severity M (full-surface audit, 2026-07-10; reproduced). `dstPageCacheNew`
(`src/runtime/dst_pagecache_linux.go:247-255`) creates memfds in the ordinary
low host-fd number space, and the syscall boundary passes `close` on
non-virtual fds through to the host
(`src/syscall/dst_fd_wrappers_linux.go:7-15`; raw dispatch allows SYS_CLOSE).
A bubble goroutine running the classic daemonize idiom
(`for fd := 3; fd < 64; fd++ { syscall.Close(fd) }`, a harmless EBADF sweep in
production) closes the harness's memfds. Reproduced: a subsequent `Truncate`
dies `fatal error: dst: page cache resize failed`
(`dst_pagecache_linux.go:267`), and an mmap dies `dst: page cache mapping
failed` (`:300`). Silent variant: a freed number reused by a later
`memfd_create` makes `node.pc.fd` for file A name file B's cache, so fd-based
ops on A silently address B's bytes while A's live mappings still show A's —
host-fd-layout-dependent, so the same seed diverges across hosts.

## Required outcome

The SUT cannot reach the harness's page-cache fds: either the memfds live in a
reserved number space refused at the syscall boundary (like the virtual-fd
range), or `close` of a page-cache fd from a bubble goroutine is a no-op/EBADF
as it would be for a fd the SUT never opened. Pinned by a close-loop test over
low fd numbers followed by a resize and an mmap.
