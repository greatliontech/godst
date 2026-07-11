# DST: an in-flight host close can straddle memfd creation (cross-M TOCTOU)

Lands: when page-cache creation is atomic with respect to in-flight host
dispatch (e.g. memfds re-homed to a refused high number range — which shrinks
but does not fully close the window — or an equivalent kernel-side guarantee)

## Gap

Severity M (review-found 2026-07-10; residual of the memfd-isolation fault
after the fd-registry fence landed). The fence check and the kernel dispatch
are not atomic across Ms: a bubble goroutine's `syscall.Close(N)` on a
then-free number N passes the fence (N unreserved), its M enters the kernel
dispatch stretch, sysmon's syscall retake (deliberately preserved under DST)
hands the P away, `dstPageCacheNew` runs and the kernel assigns N to the new
memfd, and the in-flight `close(N)` lands after the assignment — killing the
newborn page cache. The registry lock stops scheduler-level interleaving
only; the closing M is already mid-flight. Bound: needs a µs-scale syscall to
straddle a sysmon tick plus an exact number collision, and the triggering
retake is wall-clock scheduling the design already tolerates for syscalls.

Observed in the wild (2026-07-11, chunk-4 battery): DSTMemfdFDIsolation — the
daemonize close-sweep prog — died ~1-in-8 runs with "epollwait on fd 4 failed
with 9 / fatal error: runtime: netpoll failed" from the teardown quiescence
drain's GC (startTheWorldWithSema → netpoll): the netpoll epoll fd is
created lazily, and here it was assigned a number an in-flight swept close
(dispatched for its authentic EBADF while the number was free) was already
mid-kernel for — the straddle killed the RUNTIME'S OWN epoll fd. The
window's victims are not limited to page-cache memfds — any host fd created
during the straddle is exposed; the fix must close the window for the whole
host-fd space, not re-home memfds alone.

## Required outcome

A host close dispatched before a memfd's number was assigned cannot destroy
the page cache — creation is atomic with respect to in-flight bubble
dispatch, or the residual window is proven empty for the numbers memfds can
receive. The chosen mechanism is recorded beside the invisibility contract in
design.md.
