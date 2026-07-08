# DST gmdb virtual fd syscall foundation

Lands: 1

## Gap

DST simulated files are `os.File` backends without stable simulated descriptors. `File.Fd` panics, `SyscallConn` is unsupported, and raw `syscall` / `golang.org/x/sys/unix` calls are fenced for bubble goroutines. gmdb reaches durability, locking, mmap, process liveness, and clock operations through descriptor and syscall APIs.

## Required outcome

A DST run has a per-process virtual fd table for simulated files and directories. The selected Linux syscall wrappers and trampolines needed by gmdb dispatch virtual fds to DST-owned state, pre-run host fds keep the inherited-handle stance, and unsupported syscalls remain fenced. A simulated file never allocates or exposes a host fd.

Closing a virtual fd releases the process-owned resources attached to that fd, including lock and mapping ownership added by later chunks.
