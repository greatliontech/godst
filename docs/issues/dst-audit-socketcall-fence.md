# DST audit: socketcall arches bypass the syscall fence

Lands: when the interception boundary refuses socket-family syscalls on socketcall architectures

## Gap

Severity H (full-surface audit, 2026-07-10; reproduced). On 386 and s390x,
`syscall.Socket`/`Socketpair`/`Bind`/`Connect` dispatch through the
`socketcall`/`rawsocketcall` assembly entries (`src/syscall/syscall_linux_386.go:175`,
`src/syscall/syscall_linux_s390x.go:165`, `src/syscall/asm_linux_386.s:51,75`),
which issue `SYS_SOCKETCALL` directly and never pass through the fenced
`Syscall`/`Syscall6`/`RawSyscall6` trampolines (`src/syscall/syscall_linux.go:64-131`).
A bubble goroutine calling `syscall.Socket(AF_INET, SOCK_STREAM, 0)` in a
`-tags dst` GOARCH=386 binary receives a live host socket fd instead of the
"unsupported under deterministic simulation" refusal; amd64 correctly panics.
Subsequent bind/connect on it is real host network I/O — a silent simulation
escape violating DST-NODE-ISOLATION (design.md, interception boundary, which
names Socket/Socketpair explicitly).

The 386 fence test (`src/testing/simulation/dst_fence_socket_linux_386_test.go`)
probes `RawSyscall(SYS_SOCKETCALL, …)` — the trampoline path, which IS fenced —
so it passes while the wrapper path a real SUT takes stays open.

## Required outcome

On socketcall architectures, the named socket-family wrappers (or the
socketcall/rawsocketcall entries) consult the fence exactly as the trampolines
do: a bubble goroutine's call fails with the standard refusal shape, non-bubble
callers fall through to the host unchanged. The 386 fence test exercises the
`syscall.Socket` wrapper path, not only the raw trampoline.
