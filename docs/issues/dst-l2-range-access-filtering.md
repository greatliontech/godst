# DST Level-2 Range Access Filtering

**Lands:** when dst-race range/composite access hooks participate in shared-address filtering.

## Fault

The compiler emits `runtime.dstAccessYield(addr, write)` before both scalar race hooks and composite
`racereadrange` / `racewriterange` hooks. The race hook receives `(addr, size)`, but the DST hook only
receives `addr`.

That means an overlapping range access and scalar field access can get different DST conflict identities:
a range write records the composite base address, while a field read records `base+offset`. The race
detector still sees the overlap through `(addr, size)`, but DST's shared-address filter can treat the two
hooks as independent and avoid the yield/backtrack needed to cover both orders.

## Required Shape

Range/composite DST access hooks need a conflict identity that detects interval overlap, not just exact
base-address equality. The fix should preserve DST-L2-4: no hook changes outside `-tags dst -race`, and
the race hook/oracle remains unchanged.

## Validation

Add an auto-instrumented SUT where one goroutine performs a composite/range read or write and another
performs an overlapping scalar field access. DPOR must match Exhaustive on the reachable outcome set under
`-tags dst -race`, and a mutation that collapses ranges back to base-address equality must fail.
