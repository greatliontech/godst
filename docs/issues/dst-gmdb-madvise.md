# DST gmdb madvise handling

Lands: 2

## Gap

gmdb calls `Madvise` for best-effort page-cache hints after mapping files. DST currently fences raw `Madvise` through the syscall boundary.

## Required outcome

`Madvise` on DST mappings accepts `MADV_POPULATE_READ`, `MADV_HUGEPAGE`, and `MADV_COLD` without touching the host. The operation returns either success or a deterministic unsupported errno that gmdb already tolerates. It never panics because the call crossed the raw syscall fence.
