# MIPS Syscall9 bypasses the DST raw-kernel fence

Lands: when every MIPS raw syscall entry, including Syscall9, applies the same
bubble fence and virtual-fd dispatch policy

## Gap

Severity H. On linux/mips and linux/mipsle, `Syscall9` enters the kernel
directly through assembly without consulting `dstFenceActive`, the generic
trampolines, or reserved-fd checks. A bubble can invoke `SYS_OPENAT` through
this entry and mint a real host fd; named wrappers such as SyncFileRange also
diverge from the fenced boundary.

## Required outcome

Syscall9 is fenced before `entersyscall` with the same refusal shape and
split-safety as the other raw entries. A MIPS test under emulation attempts
host `openat` and proves no descriptor is created.
