# Simulated UDP datagram network (`udp`/`udp4`/`udp6` + `LinkConfig` packet faults)

**Lands:** deferred until triggered by user when `mindns` or a QUIC transport
enters DST integration (user-authorized deferral; zero retrofit — additive
unfencing in `src/net/` plus additive packet-fault fields on `LinkConfig`).

## Context and deferral rationale

`godst` currently models `"tcp"`, `"tcp4"`, and `"tcp6"` streams end to end and
fences `"udp"`, `"udp4"`, and `"udp6"` sockets loudly (`design.md`,
`DST-FAULT-NONFORECLOSE` in `faults.md`). None of the TCP-based engines (`pando`,
`tugboat`, `search`, `protodb`) open UDP sockets, whereas DNS servers (`mindns`)
and UDP-based transports (QUIC, gossip membership protocols) require datagram
delivery and datagram-specific link faults.

Because `testing/simulation.LinkConfig` is already a struct value consumed by
`ConfigureLink` and `ConfigureLinkOneWay`, adding packet-granular fault fields
later is source-compatible for every existing caller, and unfencing `"udp*"` in
`src/net/` touches zero existing TCP contracts.

## Scope when triggered

1. **Per-host UDP socket and port registry (`src/net/`):**
   - Model `net.ListenPacket`, `net.ListenUDP`, `net.Dial("udp*", ...)`, and
     `net.DialUDP` over per-host UDP port spaces (independent of each host's TCP
     port space, matching Linux `AF_INET`/`AF_INET6` port tables) with deterministic
     ephemeral port allocation, loopback (`127.0.0.1` / `::1` / `"localhost"`)
     isolation, routable `10.<b>.<c>.<d>` addressing, `SO_REUSEADDR`, and virtual-FD
     `SyscallConn` control hooks.
   - Preserve exact datagram boundaries (never coalescing writes, truncating excess
     datagram bytes on short `ReadFrom` buffers per POSIX `recvmsg` semantics),
     bounded per-socket receive buffers (dropping incoming datagrams on buffer full
     rather than backpressuring the sender), and connected-UDP `ECONNREFUSED` error
     reporting when sending to a live host with no bound UDP listener (while
     blackholed or powered-off hosts drop silently).
   - Attribute UDP sockets to `(host, procSlot)` so `CrashProcess`, `CrashHost`, and
     normal process exit tear down owned sockets deterministically.
2. **Datagram link policies and faults (`Options.Network.CrossHost` & `LinkConfig`):**
   - Apply base one-way latency, jitter, bandwidth serialization, `Partition`,
     `PartitionOneWay`, `PartitionRefuse`, and `IsolateHost` to cross-host UDP
     datagrams.
   - Extend `LinkConfig` (shared by both `Options.Network.CrossHost` and
     `ConfigureLink` / `ConfigureLinkOneWay`) with datagram-only fault
     parameters — packet loss probability, packet duplication probability, and
     allowable out-of-order delivery (removing TCP's head-of-line FIFO bunching for
     jittered datagrams) — drawn from the stream-isolated `dstFaultRand` RNG so
     TCP links continue to enforce `DST-NET-FIFO` (`faults.md` collapse-check) while
     UDP links explore real datagram degrees of freedom.
   - Record UDP bind/close and faulted-drop summaries in the off-heap failure-only
     causal event timeline without logging per-packet steady-state traffic.
