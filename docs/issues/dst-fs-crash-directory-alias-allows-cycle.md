# Crash-recovered directory aliases can be renamed into themselves

Lands: when rename ancestor checks use node reachability and cannot create a
cycle from aliased directory names

## Gap

Severity M. Crash tear can recover old and new names pointing at one directory.
`dstRename` checks descendant moves lexically. Renaming `/a` to `/b/child`
therefore passes when `/a` and `/b` are aliases, inserts the node into its own
entries, and makes recursive traversal, RemoveAll, or capacity accounting
non-terminating.

## Required outcome

Rename rejects every destination contained by the source node, regardless of
which alias names the path. A crash-tear seed sweep reaches the alias image and
pins `EINVAL` plus terminating traversal afterward.
