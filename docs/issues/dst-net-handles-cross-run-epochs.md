# Network handles remain usable across run epochs

Lands: when stateful simulated connection and listener operations reject use
outside their creation epoch without changing pure metadata or cleanup behavior

## Gap

Severity M. Network registries roll by epoch, but `dstConn` and `dstListener`
carry no epoch and their methods do not validate the active run. A handle leaked
from one run can be used in the next, combining old transport channels with new
clock, partition, reset, and fault-RNG state; operations may cross bubbles or
mutate stale registry state.

## Required outcome

Every connection and listener is an epoch-scoped stateful capability, like
simulated files and roots. Cross-run Read, Write, Accept, and deadline
operations return the chosen closed/stale identity without touching either
run's transport state. LocalAddr, RemoteAddr, and listener Addr return immutable
creation-time metadata; Close remains safe cleanup and touches only the stale
handle, never current-epoch registries or fault state.
