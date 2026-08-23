# The two callback batch loops discriminate their driver two different ways

`runFinqBlocks` decides whether the process finalizer goroutine or the DST
bubble drain is driving it by pointer compare (`gp == fing`);
`runCleanupBlock` decides the same question by `findfunc(getg().startpc)`
against `FuncID_runCleanups`. Each then keeps a dual ledger (executed vs
discarded) keyed on that answer. Untagged both discriminations fold to the
stock loop (`!dstBuild || …`), so this is tagged-only shape: one question,
two mechanisms, two ledgers to keep in step.

Collapse direction: a caller-supplied role (the stock driver passes the
process role, the drain passes its own), or a per-g flag the drain sets,
serving both loops; the `findfunc` lookup disappears from the tagged path
too. Invariants to preserve: the drain's per-entry ledger exactness across a
mid-block callback death, and `fingStatus` transitions only on the real
fing.

Lands: user decision.
