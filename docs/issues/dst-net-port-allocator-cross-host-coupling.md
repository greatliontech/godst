# Ephemeral port allocator state couples independent hosts

Lands: when listener and dialer ephemeral allocation state is scoped to the
owning host

## Gap

Severity M. `dstNet.nextListenPort` and `nextPort` are single per-run counters.
An allocation on host A shifts the first observable port assigned on host B,
creating a cross-host channel outside the simulated network and contradicting
the per-host port-space contract.

## Required outcome

Each host's allocation sequence depends only on that host's own bindings and
schedule. A test compares host B's first listen and dial ports with and without
prior allocations on host A.
