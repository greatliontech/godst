# loong64 named Fstat exposes harness page-cache descriptors

Lands: 62

## Gap

Severity M. Tagged loong64 `fstatFD` routes real descriptors to `fstatFDDST`,
which calls internal `linux.Syscall6(SYS_STATX)` directly. This bypasses
`dstSyscallPageCacheFDTrap`, so a bubble naming a harness memfd receives its
real metadata instead of `EBADF`.

## Blocker

The required runtime witness needs `qemu-loongarch64`, provided by Arch Linux's
`qemu-user` package. It is not installed on the current host. Cross-build and
structural checks can prove the Fstat call shape but cannot prove that an
executing loong64 binary refuses a live harness page-cache descriptor before
the direct statx syscall. Resume this work only with the emulator available so
the kernel-facing path executes before the issue closes.

## Required outcome

Named and raw fd wrappers preserve page-cache invisibility on loong64. The
memfd isolation test includes `syscall.Fstat` under emulation and observes
`EBADF` without host dispatch.
