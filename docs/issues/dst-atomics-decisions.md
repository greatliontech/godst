# dst-atomics-decisions — atomics as exploration decision points

**Lands:** pending feature.

`sync/atomic` operations (and `len(ch)`/`cap(ch)`) are not Level-2 exploration
transitions: a two-goroutine CAS-winner SUT explores one of two outcome classes
under both Exhaustive and DPOR yet reports `Exhausted=true`. The boundary is
documented in `docs/dst/design.md` ("Completeness boundary") and on the
`Explore` API; TSan models atomics as synchronization, so no false races — the
missed direction is assertion failures whose reachability depends on atomic
interleaving order.

Candidate mechanism: hook atomics as decision points — a `dstSyncAcquire`
analog at TSan's atomic entry points (`internal/runtime/atomic` race shims),
announcing the atomic's address as a write-conflict so DPOR explores both
orders of same-address atomic pairs. Cost concern: atomics are hot; the hook
must be confined to `-tags dst -race` scheduled runs like the access hooks
(build-mode inertness, `TestDSTAccessYieldBuildModeInert` pattern).

Removing the documented exclusions from the Completeness boundary is the
acceptance criterion; the 290-program sweep gains atomic-ordering programs.
