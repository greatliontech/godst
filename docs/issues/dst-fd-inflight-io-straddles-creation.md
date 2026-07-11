# DST: an in-flight allowlisted read/write can straddle host-fd creation

Lands: when allowlisted non-close dispatch is atomic with respect to host-fd
creation, or the residual window is proven empty for the numbers harness fds
can receive

## Gap

Severity M (review-found 2026-07-11; residual of the memfd-creation TOCTOU
after the host-close fence landed). The host-close fence removes
bubble-originated *destruction* of host fds — a close is answered `EBADF` and
never dispatched — but the fence-check-to-kernel-dispatch stretch is still
not atomic across Ms for the traps that DO dispatch. A bubble goroutine's
`syscall.Write(N, p)` (or read/lseek, or the `x/sys/unix` equivalents through
the trampolines) on a then-free real number N passes the fence
(`dstSyscallPageCacheFDTrap(N)` false while N is unreserved, SYS_WRITE
allowlisted), its M enters the dispatch stretch, sysmon's syscall retake
hands the P away, the harness or runtime assigns N to a newborn host fd (a
page-cache memfd, the lazily-created netpoll epoll fd), and the in-flight
`write(2)` lands on the newborn — silently writing another simulated file's
bytes where production would answer `EBADF`. A read leaks page-cache bytes
into the SUT; an lseek mutates a newborn's offset. Same interleaving the
close-straddle demonstrated in the wild (the netpoll epoll crash recorded at
the host-close fence in design.md "The interception boundary"); only the trap
differs. Reaching it needs a stale-fd SUT bug (an I/O call naming a number
the simulated process never owned) plus the exact straddle, so it is rarer
than the close sweep was — daemonize-style sweeps close, they do not write —
but it is in-spec reachable and uncaught.

## Required outcome

An allowlisted non-close dispatch issued before a harness fd's number was
assigned cannot read, write, or reposition the newborn fd — dispatch is
atomic with respect to host-fd creation, or the window is proven empty for
the numbers harness fds can receive. The chosen mechanism (or the emptiness
proof) is recorded beside the host-close paragraph in design.md, replacing
its "recorded gap" pointer.
