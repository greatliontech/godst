# User-arena allocations bypass the DST heap and process counters

Lands: when every supported user-arena allocation contributes deterministic
heap-trigger and per-process allocation bytes, or arena builds are refused

## Gap

Severity H. `userArena.alloc` reserves objects directly from arena chunks, and
`newUserArenaChunk` suppresses the ordinary heap trigger while DST is active.
Neither path adds object or chunk bytes to `dstHeapAlloc` or `dstProcAlloc`.
With `GOEXPERIMENT=arenas`, a nonblocking simulation can therefore allocate
many retained chunks without crossing `Options.MemoryLimit`, the deterministic
GOGC trigger, or the process OOM counter.

## Required outcome

Arena allocation has one explicit supported shape: its bytes feed both DST
counters at deterministic points, or simulation entry rejects the experiment
before state publication. An arena-enabled test crosses a small memory limit
and pins the selected behavior.
