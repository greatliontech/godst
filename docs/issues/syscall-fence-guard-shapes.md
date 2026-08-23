# The syscall fences use three guard shapes for one concept

`dstSimFenced` gates the simulation's syscall interception three ways in one
package: the zero-cost nested block (`if dstSimFenced { if … }` — Read,
Write, Pread, Pwrite, Mmap, Munmap, the shape `runtime/dst.go`'s gating rule
requires inside functions stock inlines), the conjunction
`if dstSimFenced && dstFenceActive() {` at the resource-minting and
trampoline fences (~15 sites), and — until the fd-wrapper split is folded —
an unguarded `dstTry*` call. The conjunction is a charged shape to the
inliner; it costs nothing today only because none of its hosts is inlinable
in stock, which no test pins.

Collapse direction: one shape for the package (the nested block), applied
mechanically, with the inline-decision parity sweep (fork vs stock
`-gcflags=-m`) as the oracle.

Lands: user decision.
