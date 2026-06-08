# Finalizer/cleanup-chain tails with bubble channel ops may fatal in the post-Run reap

**Lands:** when a SUT needs finalizer/cleanup chains whose tail touches a bubble channel. Narrow; protodb does not hit it (its finalizers are independent and channel-free, and it uses no channel-touching cleanups).

**Chunk C update:** the cleanup drain (Chunk C) applies the *same* single-GC-per-quiescence drain to `runtime.AddCleanup` cleanups, so this limitation extends to **cleanup** chains identically (a cleanup reachable only through another finalizable/cleanup-bearing object's pending callback, whose own cleanup touches a bubble channel, may be reaped on the async cleanup pool — `g.bubble == nil` — and fatal). Redeferred, not resolved: Chunk C deliberately keeps the single-GC philosophy (a fixpoint would hang on self-re-registering callbacks). Substitute "finalizer/cleanup" for "finalizer" throughout below.

## Fault (cited reachable mechanism)

The DST quiescence drain (D4, `synctest.go` `dstDrainAtQuiescence`) runs exactly
one GC per quiescence point, by design: an object kept alive only by another
finalizable object's still-pending finalizer is in the quiescent live set and must
not run yet (invariant DST-FIN-2; design.md D4). Finalizer chains therefore
resolve one level per quiescence, matching production.

Reachable in-spec path to a fatal:

1. SUT allocates `A` (finalizable) whose only out-edge reaches `B` (finalizable),
   and `B`'s finalizer does a bubble channel op (e.g. `ch <- x`).
2. `A` (and so `B`, reachable only via `A`) is dropped near Run end, with no
   further quiescence point before `f` returns.
3. At Run end, `dstStopGCDrain` runs one `dstDrainAtQuiescence`: the single
   GC discovers `A` (not `B` — kept alive by `A`'s pending finalizer); the drain
   runs `A`'s finalizer and `A` is freed. There is no second GC, so `B` is never
   discovered in-bubble.
4. The post-Run reap (`runtime.GC()×2` in `dst.Run`, after `dstDeactivate`)
   discovers `B` dead and runs its finalizer on the async `fing` goroutine, whose
   `g.bubble == nil`. `B`'s `ch <- x` then fatals
   `send on synctest channel from outside bubble`.

Chains of plain (non-channel) finalizers do **not** fatal — the reap's `fing`
runs them fine; only a *channel-touching* chain tail not resolved in-bubble does.

## Not a regression

Before Chunk B, `fing` ran *all* finalizers async during the bubble, so any
channel-touching finalizer fataled (not just chain tails). Chunk B fixes the
common case (finalizers run on the bubble drain). This is a residual, narrower
manifestation of the same reap hazard, surfaced by Chunk B partially fixing the
feature — not a new break.

## Options (decide when it lands)

- **Bounded Run-end fixpoint.** In `dstStopGCDrain` only (the SUT has
  exited, so there is no goroutine to unblock), loop GC+drain until `finPending`
  is false, bounded to avoid the self-re-registering-finalizer infinite loop
  (`SetFinalizer(p, fn)` inside `fn`). Resolves chains fully in-bubble. Needs a
  progress/identity bound to distinguish a deepening chain from self-resurrection.
- **Reap bubble-awareness.** Make the post-Run reap not fatal on a bubble-stamped
  channel op — fundamentally hard, since the bubble is gone by then.
- **Document as a constraint.** A SUT must not have a finalizer reachable only
  through another finalizable object whose finalizer touches a bubble channel.

The single-GC-per-quiescence choice (no fixpoint) is deliberate: a per-quiescence
fixpoint hangs on a self-re-registering finalizer and over-runs relative to
DST-FIN-2. See `synctest.go` `dstDrainAtQuiescence` and design.md D4.
