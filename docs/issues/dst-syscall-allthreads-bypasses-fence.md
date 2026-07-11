# DST syscall: AllThreadsSyscall bypasses the interception boundary

Lands: when AllThreadsSyscall/AllThreadsSyscall6 from a bubble goroutine are
fenced like the trampolines, or the bypass is recorded as a loud-by-accident
limit at the boundary's spec

## Gap

Severity M (audit-found 2026-07-11; adjacent — the route predates the
host-close fence). syscall.AllThreadsSyscall dispatches through
runtime.syscall_runtime_doAllThreadsSyscall with no dstFenceActive check: a
bubble goroutine's AllThreadsSyscall(SYS_CLOSE, fd) on a pre-run host fd
REACHES THE KERNEL on the first thread (demonstrated live: r1=0 before the
runtime threw "results differ between threads; runtime corrupted" as later
threads saw EBADF) — falsifying the boundary's no-bubble-destruction
invariant for this route, though never silently (the differ-throw is fatal).
Uniform-returning traps (SYS_PRCTL, Setegid via SYS_SETRESGID) succeed
SILENTLY from bubble context — host-state mutation the fence exists to
refuse, with no fd involved at all.

## Required outcome

Bubble-context AllThreadsSyscall is refused at entry (the fence's loud
shape), or the boundary's spec records the route as an accepted, always-loud
(for fd-table ops) / silent (for uniform traps) bypass — the silent-trap leg
argues for the fence.
