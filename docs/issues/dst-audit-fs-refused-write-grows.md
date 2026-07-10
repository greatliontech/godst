# DST audit: zero-effective-length and fully-refused writes still grow the file

Lands: chunk 9 of docs/plans/dst-audit-fixes.md

## Gap

Severity M (full-surface audit, 2026-07-10; reproduced). `writeAtLocked`
(`src/os/dst_fs.go:1339-1341`) grows the node whenever `off+len(b) >
len(data)`, including `len(b) == 0`, and unconditionally bumps mtime. Two
anchored consequences: (1) `f.Seek(100, 0); f.Write(nil)` reports Stat size 100
(real kernel: 0; POSIX: a zero-length write has no effect) and bumps mtime on a
no-op write. (2) With `LimitDisk(host, 8)`, `Seek(1<<20); Write([]byte("x"))`
returns `(0, ENOSPC)` yet the file grows to 1 MiB: `d.write`
(`dst_fs.go:1275-1276`) calls `writeAtLocked(b[:0], d.off)` on refusal, and the
zero-fill growth counts in `residentLocked`, leaving the disk 131,072× over cap
with no path to recover the budget — the fault model's capacity invariant
broken by its own refusal path. `pwrite` already guards
(`dst_fs.go:1313-1316`); `dstFDWrite`'s zero-length short-circuit means the
`syscall.Write` surface already diverges from `File.Write` for the same op.

## Required outcome

A write whose effective slice is empty (zero-length, or fully refused by the
ENOSPC cap) leaves file size, content, resident-byte accounting, and mtime
unchanged. Pinned by a zero-length-write-at-offset test and a
write-refused-by-cap test asserting size and resident bytes do not grow.
