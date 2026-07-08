# DST gmdb read-only file mmap

Lands: 2

## Gap

gmdb reads database pages through `mmap` of regular files. DST currently fences `Mmap`, and simulated files have no descriptor that `Mmap` can name.

## Required outcome

`Mmap(fd, offset, length, PROT_READ, MAP_SHARED)` on a simulated regular file returns a mapping over that file's current bytes. Writes through normal file operations on the same simulated file become visible to reads through the mapping, matching a shared page-cache view.

`Munmap` unregisters the mapping. `Mprotect(PROT_READ)` succeeds for supported mappings and leaves no writable access path after a mapping has been made read-only.
