# Network base delays are scaled by endpoint clock drift

Lands: when handshake and payload-delivery waits consume universe base time
independently of the calling host's rate

## Gap

Severity M. Network code computes delivery instants with `dstBaseNanos`, but
waits with ordinary relative `time.Timer` and `time.Sleep`. The runtime converts
those durations for the caller's host drift. A rate-2 dialer pays half the
configured base traversal, while a rate-0.5 receiver can wait twice the
remaining base delivery time.

## Required outcome

Configured link latency, jitter, and throttle are base time on every host rate.
The sender-clock retransmission horizon remains as specified. Tests observe
handshake and payload timings from an unskewed host while endpoints run at fast
and slow rates.
