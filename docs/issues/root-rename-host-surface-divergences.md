# os.Root.Rename: sim ladder diverges from the host rooted surface beyond the slash rule

Host-probed (ext4 and tmpfs agree) `os.Root.Rename` shapes the simulated
`dstRootRename` does not reproduce. The host rooted implementation is
Go's own openat-walk (not raw renameat(2)), and it both carries an
existing-directory-newname refusal and orders its checks differently:

| shape                              | host os.Root.Rename | sim dstRootRename |
|------------------------------------|---------------------|-------------------|
| dir → "existing-empty-dir/"        | `EEXIST`            | replaces (success) |
| dir → "existing-nonempty-dir/"     | `EEXIST`            | `ENOTEMPTY`       |
| dir → "self/" (same node)          | `EEXIST`            | no-op success     |
| missing → "existingfile/"          | `ENOTDIR`           | `ENOENT`          |

The first three are the rooted surface's existing-directory-target
refusal (`EEXIST`, documented on `os.Root.Rename`: "If newname already
exists and is not a directory, Rename replaces it" — a directory newname
is refused); the sim replaces or no-ops instead, admitting sim-only
executions (a directory silently replacing another through a Root). The
fourth is check ordering: the host asserts the new final's trailing
slash before the old final's existence, the sim the reverse.

The dir-source trailing-slash arm itself (dir → "missing/" succeeds,
file → "missing/" `ENOTDIR`) is already fixed and regression-pinned in
the un-rooted and rooted paths alike; this issue tracks only the
remaining rooted-surface shapes above, which need `dstRootRename` to
mirror the rooted implementation's preamble and ordering, with rows
probed through `os.Root.Rename` (not rename(2), whose ladder differs).

Lands: when dstRootRename is aligned with the host rooted-rename
surface's probed matrix.
