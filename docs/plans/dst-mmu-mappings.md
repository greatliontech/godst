# Plan: MMU-backed simulated mappings

Derived from `docs/dst/design.md` § Memory mappings.

## Why

Two recorded divergences — mapping past EOF is `EINVAL`, and truncating a file
under a live mapping is fenced — are the same defect wearing two faces: a
mapping is modelled as a Go `[]byte` copy of the file's bytes, so the model has
no way to express *mapped but not accessible*. Every alternative (clamping the
slice, zero-filling, or asking the SUT to avoid the shape) reintroduces the
divergence somewhere else: a reader gets zeros where production takes `SIGBUS`,
which is precisely the torn-file bug class DST exists to catch.

Both shapes are on a real database's hot path: `mmapRO` maps a `MaxSize`
reservation over a short file, and `maybeShrink` ftruncates at the end of every
commit while that reservation is live.

The fix is to stop simulating the mapping and use the machine's own MMU. A
mapped file's page cache becomes a `memfd`; its length is a real `ftruncate`;
each mapping is a real `mmap(MAP_SHARED)` of it. The kernel then supplies the
whole contract — a load past the end traps, a shrink makes the cut pages trap
and zeroes the partial page's tail, a re-growth does not resurrect the dropped
bytes, and two mappings of one file share bytes while keeping their own
protections. That last property is why a single anonymous arena is not enough:
a database maps its data file read-only while writing to it through `write(2)`,
and one arena has one protection per page.

The fault is then converted into the death of the *simulated* process — the
outcome production produces — leaving peers and the harness running. `Mprotect`
becomes hardware protection rather than bookkeeping, mappings of one file from
two processes become genuinely shared memory rather than copy-and-write-back,
and the surrogate machinery (spare-base selection, write-back, per-write mapping
fan-out) is deleted rather than extended.

Determinism is preserved because nothing address-derived is observable: a fault
is attributed to a process, and the simulated page size stays 4096.

## Chunks

- [x] 1. The page cache, and fault attribution. `memfd` create/resize/map/unmap
      primitives; a registry of live mappings; a `sigpanic` hook that turns a
      `SIGSEGV`/`SIGBUS` inside a mapping into the death of the current simulated
      process (crash mark + park forever, unrecoverable — checked before
      `paniconfault`, so a SUT's `SetPanicOnFault` cannot swallow it). Refuse a
      host whose page size exceeds the simulated 4096, loudly.
- [ ] 2. A node's bytes live in its page cache. `data` aliases a writable mapping
      of the `memfd` on first `Mmap`; growth and shrink become `ftruncate` plus a
      reslice rather than a reallocation. Mappings hand out real subslices.
      Delete `dstMMapDataLocked`'s candidate hunt, `dstMMapNewBase`,
      `dstMMapWriteLocked`, `dstMMapSyncLocked`, `dstMMapSyncEntryLocked`.
      Constraint (from review): the os layer must touch mapped bytes only from
      ordinary goroutine context — a fault taken while `!canpanic()` (runtime
      lock held, mallocing, on the system stack, mid-syscall) never reaches
      `sigpanic` and would abort the harness instead of killing the process.
- [ ] 3. The two divergences die. Mapping past EOF is allowed (a reservation);
      access past EOF faults. Truncate-shrink under a live mapping is allowed;
      access to the cut pages faults. Remove the `EINVAL` and
      `dstMMapShrinkFencedLocked`. `Mprotect` and `Madvise` act on the hardware.
      Re-anchor the tests that pinned the old stance.
- [ ] 4. Fault semantics under crash. A process crash keeps the arena (the page
      cache outlives the process); a host crash drops it; a tear rebuilds it
      from the durable image. `Explore`/`Replay` reproduce a fault exactly.
- [ ] 5. Compatibility coverage. Extend the harness with the reservation mapping,
      per-commit shrink, and a reader that respects the high-water mark — plus
      the negative: a read past it kills that process and only that process.
- [ ] 6. Spec, release, close-out. Amend § Memory mappings; retarget every cite;
      run the full enforcing matrix; delete this plan.
