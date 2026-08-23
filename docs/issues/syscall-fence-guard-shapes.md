# The syscall fences use two guard shapes for one concept

`dstSimFenced` gates the simulation's syscall interception two ways in one
package: the zero-cost nested block (`if dstSimFenced { if … }` — the I/O
wrappers, the fd/kill/mmap wrapper splits, and `gettimeofday`; the shape
`runtime/dst.go`'s gating rule requires inside functions stock inlines) and
the conjunction `if dstSimFenced && dstFenceActive() {` at the
resource-minting and trampoline fences (~15 sites). The conjunction is a
charged shape to the inliner; it costs nothing today only because none of
its hosts is inlinable in stock, which no test pins.

Collapse direction: the nested block everywhere, applied mechanically, with
the inline-decision parity sweep (fork vs stock `-gcflags=-m`) as the
oracle.

Lands: user decision.
