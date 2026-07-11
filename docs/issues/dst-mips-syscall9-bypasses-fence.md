# MIPS Syscall9 bypasses the DST raw-kernel fence

Lands: 61

## Gap

Severity H. On linux/mips and linux/mipsle, `Syscall9` enters the kernel
directly through assembly without consulting `dstFenceActive`, the generic
trampolines, or reserved-fd checks. A bubble can invoke `SYS_OPENAT` through
this entry and mint a real host fd; named wrappers such as SyncFileRange also
diverge from the fenced boundary.

## Blocker

The required runtime witness needs `qemu-mips` and `qemu-mipsel`, provided by
Arch Linux's `qemu-user` package. They are not installed on the current host.
Cross-build, symbol, source-order, and compiler-assembly checks can prove the
wrapper shape but cannot prove that the MIPS runtime reaches the expected
refusal, dispatches a virtual fd correctly, and creates no host descriptor.
Resume this work only with the emulator available so those kernel-facing paths
execute before the issue closes.

## Required outcome

Syscall9 is fenced before `entersyscall` with the same refusal shape and
split-safety as the other raw entries. A MIPS test under emulation attempts
host `openat` and proves no descriptor is created.
