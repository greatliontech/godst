# DST gmdb virtual fd syscall foundation

Lands: 1

## Gap

DST simulated files are `os.File` backends without stable simulated descriptors. `File.Fd` panics, `SyscallConn` is unsupported, and raw `syscall` / `golang.org/x/sys/unix` calls are fenced for bubble goroutines. gmdb reaches durability, locking, mmap, process liveness, and clock operations through descriptor and syscall APIs.

## Required outcome

A DST run on Linux has a per-process virtual fd table for simulated files and directories. Selected split-safe Linux syscall wrappers dispatch virtual fds to DST-owned state, pre-run host fds keep the inherited-handle stance, and unsupported syscalls remain fenced. A simulated file never allocates or exposes a host fd.

The generic raw syscall trampolines are not the dispatch seam for virtual file operations: they are `nosplit` and receive `uintptr` arguments after pointer conversion. Direct raw syscall use of virtual fd numbers must be refused before host dispatch. gmdb's `golang.org/x/sys/unix` compatibility requires a later split-safe front door above the generic raw syscall trampolines; until that exists, x/sys-style virtual-fd calls remain fenced rather than reaching the host.

Closing a virtual fd releases the process-owned resources attached to that fd, including lock and mapping ownership added by later chunks.
