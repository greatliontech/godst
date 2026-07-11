# The SYN-ACK half ignores cancellation and endpoint teardown

Lands: when the second handshake traversal observes context expiry, reset, and
host/process death before returning a successful connection

## Gap

Severity H. After the accept handoff, `dstConnectSYNACK` performs an
unconditional `time.Sleep`. A deadline between one-way latency and the full RTT
therefore expires but `DialContext` still returns success. A reset, process
exit, or host crash during the same sleep can close the transport while the
dial later returns the dead connection with a nil error.

## Required outcome

The complete handshake is context-interruptible and revalidates endpoint
liveness before success. Tests expire a deadline and crash/reset the target
during the second half; no arm may return `(conn, nil)`.
