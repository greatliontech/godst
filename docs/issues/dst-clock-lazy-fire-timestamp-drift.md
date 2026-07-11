# DST clock: a lazily-fired timer's delivered timestamp mixes regimes on a drifted host

Lands: when the delivered-value contract for lazily-fired timers on rate≠1
hosts is stated in faults.md (host-scaled delay, or the mixed-regime reading
recorded as the modeled behavior)

## Gap

Severity L. `sendTime` computes the delivered value as `Now().Add(-delta)`
where `delta = now - when` is a base-time span but `Now()` is the host's
rate-scaled wall. For a timer that fires lazily long after its due time on a
host at rate r != 1, the subtraction removes a base-ns delay from a host-ns
reading, so the value lands r*delta - delta
away from the host-perceived due instant. On rate-1 hosts (and for the
DriftClock overdue-conversion cases, whose shift discount restores the
unchanged-rate value) the delivered timestamp is exact; the divergence needs
a lazily-fired timer AND a standing rate departure.

## Required outcome

faults.md states what timestamp a lazily-fired timer delivers on a drifted
host: either the delay is host-scaled at fire (value = host-perceived due
instant) or the mixed-regime reading is recorded as modeled behavior with
its bound.
