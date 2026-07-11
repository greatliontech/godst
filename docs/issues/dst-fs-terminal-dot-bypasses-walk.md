# Terminal dot checks bypass physical path error ordering

Lands: when Remove, RemoveAll, and Rename physically resolve intermediates
before applying terminal dot or dot-dot restrictions

## Gap

Severity L. The named removal and rename functions inspect `path.Base` before
`dstFSResolve`. Paths such as `/missing/..` and `/file/.` return `EINVAL` or
`EBUSY` before the physical walk can return `ENOENT` or `ENOTDIR`, unlike the
component-wise contract and kernel lookup ordering.

## Required outcome

First failing path component determines the error. Table tests cover both
rename operands and removal variants with missing and regular-file
intermediates ending in `.` or `..`.
