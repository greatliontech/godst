# Host NumCPU silently wraps through the runtime table

Lands: when every accepted positive HostConfig.NumCPU is represented exactly,
or oversized values fail before host publication

## Gap

Severity L. `dstSetHostIdent` converts the public `int` NumCPU to `int32`
without validation. On 64-bit systems, values above `MaxInt32` wrap negative or
to unrelated positive counts; a negative stored value falls back to the run
default rather than reporting the configured host identity.

## Required outcome

Host identity never silently changes an accepted positive NumCPU. Boundary
tests cover `MaxInt32`, `MaxInt32+1`, and a value that wraps positive.
