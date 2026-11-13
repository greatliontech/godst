// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// enqueue queues a single cleanup for execution.
//
// Called by the sweeper, and only the sweeper.
func (q *cleanupQueue) enqueue(c cleanupFn) {
	mp := acquirem()
	pp := mp.p.ptr()
	b := pp.cleanups
	if b == nil {
		if q.flushed.Load() {
			q.flushed.Store(false)
		}
		b = (*cleanupBlock)(q.free.pop())
		if b == nil {
			b = (*cleanupBlock)(persistentalloc(cleanupBlockSize, tagAlign, &memstats.gcMiscSys))
			for {
				next := (*cleanupBlock)(q.all.Load())
				b.alllink = next
				if q.all.CompareAndSwap(unsafe.Pointer(next), unsafe.Pointer(b)) {
					break
				}
			}
		}
		pp.cleanups = b
	}
	if full := b.enqueue(c); full {
		q.full.push(&b.lfnode)
		pp.cleanups = nil
		q.addWork(1)
	}
	pp.cleanupsQueued++
	releasem(mp)
}

// runCleanups is the entrypoint for all cleanup-running goroutines.
func runCleanups() {
	for {
		b := gcCleanups.dequeue()
		if raceenabled {
			// Approximately: adds a happens-before edge between the cleanup
			// argument being mutated and the call to the cleanup below.
			racefingo()
		}

		gcCleanups.beginRunningCleanups()
		for i := 0; i < int(b.n); i++ {
			c := b.cleanups[i]
			b.cleanups[i] = cleanupFn{}

			var racectx uintptr
			if raceenabled {
				// Enter a new race context so the race detector can catch
				// potential races between cleanups, even if they execute on
				// the same goroutine.
				//
				// Synchronize on fn. This would fail to find races on the
				// closed-over values in fn (suppose arg is passed to multiple
				// AddCleanup calls) if arg was not unique, but it is.
				racerelease(unsafe.Pointer(c.arg))
				racectx = raceEnterNewCtx()
				raceacquire(unsafe.Pointer(c.arg))
			}

			// Execute the next cleanup.
			c.call(c.fn, c.arg)

			if raceenabled {
				// Restore the old context.
				raceRestoreCtx(racectx)
			}
		}
		gcCleanups.endRunningCleanups()
		gcCleanups.executed.Add(int64(b.n))

		atomic.Store(&b.n, 0) // Synchronize with markroot. See comment in cleanupBlockHeader.
		gcCleanups.free.push(&b.lfnode)
	}
}
