# Rooted MkdirAll walk shares the Root resolver

**Lands:** user decision.

`dstRootMkdirAll` (`src/os/dst_root.go`) re-implements the component walk
that `dstRootResolveLocked` performs for every other rooted operation —
`.`/`..` handling, escape detection, ancestor ENOTDIR — with a creating
step and its own last-component rule (an existing non-directory target is
EEXIST). Two copies of one walk is where the host divergence fixed alongside
the 1.26.7 port lived (the copies' last-component handling had drifted).
Collapse: a prefix-by-prefix walk over the resolver, or a resolver mode that
creates missing intermediate directories and reports the failing component
(the Op/Path the host names), so `MkdirAll` shares the single resolution
ladder design.md pins for rooted paths. `MkdirAll` now also keeps its own
copy of the resolver's cleaned-prefix bookkeeping (`names`) for the error
path, and the resolver still carries an unreachable `!cur.isDir` guard (cur
is always a directory: the root node, a verified child, or a `..` pop) that
the collapse removes.
