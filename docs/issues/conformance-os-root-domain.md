# Conformance harness: an `os.Root` domain

**Lands:** user decision.

The differential conformance harness (`src/testing/simulation/conformance`)
drives pipes, TCP, and the filesystem through `os` path operations; the
`os.Root` surface — which the simulation implements separately in
`src/os/dst_root.go` (its own path resolution, `Mkdir`/`MkdirAll`/`Remove`/
`Rename`/`Chmod`/… dispatch) — has no domain, so host-vs-sim divergences in
it are found only by hand probes. The 1.26.7 port audit found one that way
(`Root.MkdirAll` on an existing non-directory target: host EEXIST, sim
ENOTDIR; now pinned by `TestDSTRootMkdirConformsToHost` in `src/os`). A
Root domain would mirror the fs op grammar through a `*os.Root` handle
(open, create, mkdir, mkdirall, remove, removeall, rename, stat, readdir,
trailing-slash and dot variants, escapes) under the same allowlist
discipline, so the generator keeps pace with the modeled surface instead of
a per-finding table test.
