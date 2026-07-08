# DST gmdb clock_gettime virtualization

Lands: 9

## Gap

gmdb reads monotonic clocks through `ClockGettime(CLOCK_MONOTONIC)` and `ClockGettime(CLOCK_BOOTTIME)`. Go's `time` package already reads DST virtual time, but raw `ClockGettime` currently crosses the fenced syscall boundary.

## Required outcome

`ClockGettime(CLOCK_MONOTONIC)` returns the DST virtual monotonic base clock for bubble goroutines. `ClockGettime(CLOCK_BOOTTIME)` returns a DST virtual boottime clock that advances across any simulated suspend interval once suspend is modeled; until then, it may coincide with the monotonic base clock. Neither call leaks the host clock.
