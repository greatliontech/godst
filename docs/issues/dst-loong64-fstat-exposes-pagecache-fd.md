# loong64 named Fstat exposes harness page-cache descriptors

Lands: when loong64 Fstat applies the page-cache fd invisibility trap before
its direct statx syscall

## Gap

Severity M. Tagged loong64 `fstatFD` routes real descriptors to `fstatFDDST`,
which calls internal `linux.Syscall6(SYS_STATX)` directly. This bypasses
`dstSyscallPageCacheFDTrap`, so a bubble naming a harness memfd receives its
real metadata instead of `EBADF`.

## Required outcome

Named and raw fd wrappers preserve page-cache invisibility on loong64. The
memfd isolation test includes `syscall.Fstat` under emulation and observes
`EBADF` without host dispatch.
