# Zero-length raw mapping operations bypass ownership checks

Lands: when raw and named zero-length mapping operations share one documented
ownership and errno contract

## Gap

Severity L. `dstRawDispatch` returns success for zero-length raw `madvise` and
`mprotect` before checking whether the address belongs to a simulated mapping,
while named wrappers return `EINVAL`. An arbitrary host address is therefore
accepted through the raw path despite the boundary rule that non-mapping
addresses are refused.

## Required outcome

Named and raw Mprotect, Madvise, and Munmap agree for empty ranges and never
launder a host address as a simulated success. Tests cover aligned, unaligned,
mapping, and non-mapping addresses.
