# fdatasync on a directory fd: sim EINVAL vs Linux success (recorded stance vs fidelity mandate)

Host-probed: `syscall.Fdatasync(dirfd)` succeeds on Linux; the sim answers a
deterministic EINVAL. The stance IS recorded (design.md: "Fdatasync on a
simulated directory is a deterministic EINVAL; directory entry durability is
through Fsync") — but it is a false positive against production for the real
crash-safety idiom "create/rename, then fdatasync the DIRECTORY" (used
instead of fsync for the same entry-durability effect; `syscall.Fdatasync`
and `x/sys/unix.Fdatasync` are the same modeled op). A SUT whose durability
discipline fdatasyncs the directory fails under simulation and works in
production.

Spec-amend candidate (user ruling): either directory fdatasync commits entry
durability exactly as directory Fsync does (host-faithful; the sim loses the
loud signal that a caller relied on data-only semantics for entries), or the
recorded EINVAL refusal is re-justified against the fidelity mandate with the
idiom named. Default per the mandate (no false positives on legal production
shapes) points at the commit arm.

Lands: when directory fdatasync either commits entry durability
(host-faithful, with a durability-matrix pin) or its refusal is re-recorded
with an explicit fidelity-mandate justification by user ruling.
