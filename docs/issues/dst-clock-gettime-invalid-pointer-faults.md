# Virtual clock_gettime faults on invalid output pointers

Lands: when modeled raw clock reads return kernel-shaped EFAULT for every
invalid user address

## Gap

Severity M. `dstTryClockGettime` checks only a null pointer, then writes through
an arbitrary `uintptr` with Go unsafe stores. Pointer 1, an unmapped page, or a
read-only page faults or panics the process where the real syscall returns
`EFAULT`.

## Required outcome

The modeled time32 and time64 entries preserve raw ABI error semantics without
unsafe process-fatal writes. Subprocess tests call all trampoline forms with
invalid and read-only addresses and receive `EFAULT`.
