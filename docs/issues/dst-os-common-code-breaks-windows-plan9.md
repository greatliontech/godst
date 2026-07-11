# Common DST os hooks break ordinary Windows and Plan 9 builds

Lands: when untagged os builds compile on every supported file layout and DST
field access is selected only where that layout provides it

## Gap

Severity H. Common `os/file.go` and Windows-selected `file_posix.go` access
`f.dstf`, but the Windows and Plan 9 `file` structs have no such field.
Ordinary `GOOS=windows` and `GOOS=plan9` builds fail while importing `os`, even
without the `dst` tag.

## Required outcome

The untagged standard library cross-compiles for Windows and Plan 9 with no DST
code or data requirement. Enforcing cross-build tests cover both targets and
the tagged platform policy is documented separately.
