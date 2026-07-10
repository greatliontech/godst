# DST audit: dstMprotect refuses protection changes Linux allows

Lands: when mprotect tracks fd writability, or the spec records the modeled restriction

## Gap

Severity M (full-surface audit, 2026-07-10; probed on production Linux).
`dstMprotect` (`src/os/dst_mmap_linux.go:363-368`) gates on `entry.writable` —
the map-time prot — not the fd's access mode, and rejects anything that is not
PROT_READ or (PROT_READ|PROT_WRITE on a writable-created mapping). On real
Linux, `mmap(PROT_READ, MAP_SHARED)` of an O_RDWR fd then
`mprotect(PROT_READ|PROT_WRITE)` succeeds (VM_MAYWRITE follows fd access), and
`mprotect(PROT_NONE)` always succeeds; under dst both return EACCES. A
production-legal execution (RO-map-then-upgrade, or a PROT_NONE guard toggling
over a mapping) is unreachable — the SUT takes an error path no kernel
produces. The fd's writability (`file.wr`, in hand at `dst_mmap_linux.go:131`)
is not recorded on the entry; the comment at `:364-365` misstates the check.
This restriction is pinned by `src/os/dst_fd_linux_test.go:161-162` but the
spec (design.md § mappings) states only creation-time access checks and the
alignment EINVAL — spec-vs-code contradiction; spec wins by default.

## Required outcome

Either the entry records fd writability so mprotect permits any protection the
fd's access mode allows (including PROT_NONE), matching Linux; or the spec
records "mprotect may not raise protection above map-time prot, PROT_NONE
unmodeled" as a deliberate limit and the pinning test's comment matches. The
contradiction between the spec and `dst_fd_linux_test.go` is resolved.
