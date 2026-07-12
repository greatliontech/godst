# MIPS clock_gettime64 lacks a runtime witness

Lands: 61

## Gap

Severity L. The linux/mips and linux/mipsle `clock_gettime64` dispatch compiles
with the shared 32-bit implementation, but its architecture-specific syscall
number and o32 calling convention have not executed under DST. Cross-builds do
not prove that valid destinations receive virtual time or that invalid,
read-only, partial, and wrapping destinations return `EFAULT` without exposing
host-clock bytes.

## Blocker

The runtime witness needs `qemu-mips` and `qemu-mipsel`, provided by Arch
Linux's `qemu-user` package. They are not installed on the current host.

## Required outcome

Run the MIPS and MIPSLE time64 virtual-clock and invalid-pointer behavior under
emulation, including the partial-copy virtual-byte invariant, before this issue
closes.
