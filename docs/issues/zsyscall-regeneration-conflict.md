# Hand edits to generated zsyscall files will fight regeneration

The dst fd-wrapper split hand-edits generated files: `Close` → `closeFD`,
`Fstat` → `fstatFD`, `Seek` → `seekFD` in `src/syscall/zsyscall_linux_*.go`,
with the dst-side wrappers living in unconstrained companions
(`dst_fd_wrappers_linux.go`, `dst_kill_linux.go`,
`dst_mmap_wrappers_linux.go`). `mksyscall`-driven regeneration of the
zsyscall files would silently revert the renames and produce duplicate
symbol or missing-wrapper build breakage, and nothing marks the files as
diverged from their generator.

Resolution direction: either teach the `//sys` lines to emit the split names
(so regeneration converges), or move the renames out of the generated files
entirely.

Lands: when the linux zsyscall files are next regenerated.
