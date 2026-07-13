# Fault-layer reset drops a survivor's delivered bytes — an execution no real kernel produces

The injected faults that reset connections — `simulation.Reset(a,b)`,
`ResetProcess(p)`, and the crash faults' RST teardown — close BOTH ends'
transports, so the surviving peer's next read fails `ECONNRESET` without
draining, destroying bytes already **delivered to the survivor's receive
queue** (and bytes written toward it before the fault, which travel ahead
of any real RST on the in-order link). A real kernel cannot produce that
execution: an incoming RST sets the socket error, but `tcp_recvmsg`
reports pending data before the error, so the survivor always drains its
already-queued bytes first. The non-fault paths (a user-called `Close()`
with unread inbound data, process-exit teardown) model that
drain-then-reset shape; the fault paths deliberately do not, and both
faults.md passages now frame the no-drain teardown as a recorded
fault-model collapse rather than a kernel-real behavior.

The open Soundness question: the fault layer's charter is executions a
real deployment could produce (⊆-real). Destroying a survivor's delivered
bytes is not one of them — a SUT could observe data loss on a connection
whose bytes had already arrived, and fail a run for a fault pattern no
production incident can reproduce. Whether the fault layer should model
drain-then-reset for survivors (reset the victim's ends only, letting the
survivor drain to the stable `ECONNRESET` identity, as the close(2)
conditional's `dstResetBothEnds` now does) or keep the destroy-outright
collapse (arguing a middlebox RST can race arbitrarily far back in the
delivery pipeline) is a Soundness ruling for the user, not an
implementation choice.

Lands: when the fault-layer drain semantics question is ruled on.
