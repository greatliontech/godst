# DST audit: the pre-seeded /tmp baseline is born unsynced and erased by every host crash

Lands: chunk 7 of docs/plans/dst-audit-fixes.md

## Gap

Severity H (full-surface audit, 2026-07-10; reproduced). `newDstFSDisk`
(`src/os/dst_fs.go:274-278`) creates root and `/tmp` via `dstFSNewNode`, which
leaves `syncedEntries` nil — the disk's initial (mkfs) image is not part of the
durable image. `dstRestoreNodeLocked` (`src/os/dst_host_crash.go:102`) rebuilds
`root.entries` from the empty durable set on `CrashHost`, so the `"tmp"` entry
vanishes. A SUT cannot make it durable short of fsyncing `"/"`, which no
POSIX-disciplined program does for a pre-existing directory. Reproduced:
create `/tmp/f`, write, `f.Sync()`, open `/tmp` and `Sync()` it (the full
fsync-file-then-fsync-dir discipline the spec pins), `CrashHost` → reading
`tmp/f` fails ENOENT. Violates "synced state survives byte-exactly"; a
false-positive generator for every crash-recovery SUT keeping state under the
one directory the spec guarantees exists. Existing tests mask it by creating
files at `"/"` and syncing `"/"`
(`src/testing/simulation/crash_host_linux_test.go:54`).

## Required outcome

The initial tree a run boots with (root, `/tmp`) is durable from birth — a
host crash preserves it, and fsync-disciplined state under `/tmp` survives
byte-exactly. Pinned by a crash test whose files live under `/tmp` with
directory fsync on `/tmp`, not `/`.
