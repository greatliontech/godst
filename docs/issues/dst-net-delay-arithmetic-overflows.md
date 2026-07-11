# Network delay arithmetic can wrap into immediate delivery

Lands: when every accepted latency, jitter, bandwidth, and base-time sum clamps
or fails before signed overflow

## Gap

Severity L. `dstTransmitNanos` protects the fractional product but not
`q*1e9`, and `pushLocked` adds transmit end, latency, and jitter with unchecked
signed arithmetic. Public configuration can make mathematically positive
delays wrap negative, placing payload or handshake delivery in the past.

## Required outcome

All delay arithmetic is overflow-safe and never schedules earlier than now.
Boundary tests use a big-integer oracle for throttle duration and near-MaxInt64
latency/jitter/base combinations.
