# Graceful FIN bypasses link latency and jitter

Lands: when peer-close arrival is scheduled through the same link-delay model
as other TCP control traffic

## Gap

Severity M. `dstStream.closeWrite` records `closeAt` as the sender's current
base time and wakes the receiver immediately. On a delayed cross-host link, a
receiver can observe EOF before the FIN could arrive, and before a shorter read
deadline that production would hit first.

## Required outcome

FIN arrival pays the configured one-way base latency and jitter while preserving
partition behavior and queued-data drain. A read deadline shorter than the link
delay fires before EOF; a later read receives EOF after the FIN arrival.
