# DST audit: a leaked *Root across runs defeats the epoch gate and nil-derefs

Lands: chunk 10 of docs/plans/dst-audit-fixes.md

## Gap

Severity M (full-surface audit, 2026-07-10; reproduced). `dstRoot` has no
epoch and no `dstRoot*` operation consults `dstFSEpoch`; `dstRootOpenFile`
(`src/os/dst_root.go:159`) over a dead-run node builds a `dstFile` and
`dstNewFile` (`dst_fs.go:1067`) stamps it with the CURRENT epoch, defeating the
very gate `dstFile.epoch` exists to enforce (`dst_fs.go:936-943`). Reproduced:
run 1 `OpenRoot("/d")` with `/d/f`; run 2 `leaked.OpenFile("f", O_RDWR)`, then
`os.Stat("/tmp")` (rolls the epoch, `dstNodeReleaseRunLocked` nils `node.pc`,
`dst_pagecache_linux.go:151-158`), then `f.Write(...)` → SIGSEGV at
`dst_pagecache_linux.go:118`. Ordered before the roll, the same leaked Root
silently reads the prior run's un-released tree, so run-2 observable state
depends on op ordering relative to the first rolled op — a determinism leak.

## Required outcome

A `*Root` (and files opened through it) leaked across a run boundary is
refused with the same deterministic, host-isolated behavior a leaked `*File`
gets — never a nil deref, never a read of a prior run's tree. Pinned by a
test opening a Root in one run and using it (and a file from it) in the next.
