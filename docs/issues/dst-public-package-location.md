# Relocate the public DST API package out of `src/runtime/dst`

**Lands:** before the DST public API is documented/released (pre-v1 clean break; do it while there is no installed base, so no compatibility shim is needed).

## Problem

The public DST entry point — package `dst`, import path `runtime/dst`, at
`src/runtime/dst/dst.go` (the `Run(seed, f)` API) — is placed under `src/runtime/`
but is a *consumer* of the runtime, not part of it. It is analogous to
`testing/synctest`, which wraps the runtime-internal `synctestRun` via the thin
`internal/synctest` linkname shim. By that analogy the public DST API belongs
somewhere like `testing/simulation` (or `testing/dst`), not under `runtime/`.

This is purely about the **public wrapper**. It is independent of the low-level
DST mechanism and does not block any GC chunk.

## What must NOT move

The DST *mechanism* is woven into runtime internals and gated on `dstActive()`:
per-g RNG routing (`rand.go`), the per-bubble relative GC trigger and STW forcing
(`mgc.go`), the finalizer drain (`mfinal.go`, `synctest.go`), the async-`fing`
gate (`proc.go`), the scavenger park (`mgcscavenge.go`), and the activation/baseline
state (`dst.go`). These are runtime *behaviour*, reachable only with runtime
internals (`gopark`, `finq`, the `synctestBubble` struct), and must stay in
package `runtime`. Only the public `Run` wrapper relocates; it would reach the
runtime via a linkname shim exactly as `testing/synctest` does today.

## Why deferred (and why it does not bite Chunk B)

Considered during Chunk B (the finalizer drain) because the drain goroutine's
start function is `runtime`-prefixed and so is classified by `isSystemGoroutine`.
That classification is independent of where the *public* package lives — the drain
body is irreducibly runtime-internal regardless — so relocating the package would
not have changed it. Chunk B identifies the drain by PC in `isSystemGoroutine`
(the same idiom the runtime uses for `sighandler`), which is the correct fix and
leaves this relocation as a separate, deliberate cleanup.

## Scope when it lands

- Move `Run` (and its package doc / determinism contract) to the new location.
- Repoint the `//go:linkname` shims and the `runtime/dst`-internal tests
  (`src/runtime/dst_test.go` builds testprogs that import `runtime/dst`; update
  the import path).
- Pre-v1 clean break: replace the old import path outright, no deprecation alias
  (no external installed base).
