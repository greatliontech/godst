// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"sync"
	"testing"
)

// memSink keeps the test allocations live (escapes), so the compiler does not
// elide the make calls whose bytes the per-process counter must see.
var memSink []byte

// procBytes reads the calling goroutine's process allocation counter (the L2 OOM
// metric). Read inside the run — the table resets at run end.
func procBytes() int64 {
	_, p := dstCurrentNode()
	return dstProcAllocBytes(p)
}

// TestDSTMemPerProcessAccounting verifies per-process allocation accounting: a
// process's heap allocations accrue to its own counter, a process that allocates
// more reports more, and the un-budgeted root (proc 0) is not counted.
func TestDSTMemPerProcessAccounting(t *testing.T) {
	var bigBytes, smallBytes, rootBytes int64
	Run(1, func() {
		memSink = make([]byte, 1<<20)
		rootBytes = procBytes() // root is proc 0 -> never counted
		Process("big", func() {
			memSink = make([]byte, 8<<20) // 8 MB
			bigBytes = procBytes()
		})
		Process("small", func() {
			memSink = make([]byte, 1024) // 1 KB
			smallBytes = procBytes()
		})
	})
	if rootBytes != 0 {
		t.Errorf("root (proc 0) accounted %d bytes, want 0 (un-budgeted)", rootBytes)
	}
	if smallBytes <= 0 {
		t.Errorf("small process accounted %d bytes, want > 0", smallBytes)
	}
	if bigBytes <= smallBytes {
		t.Errorf("big process %d bytes <= small process %d bytes (per-process attribution wrong)", bigBytes, smallBytes)
	}
	if bigBytes < 8<<20 {
		t.Errorf("big process accounted %d bytes, want >= 8 MB", bigBytes)
	}
}

// TestDSTMemChildAttributed verifies a process's child goroutine's allocations
// accrue to the process (the counter follows g.dstProc, inherited at newproc1).
func TestDSTMemChildAttributed(t *testing.T) {
	var before, after int64
	Run(1, func() {
		Process("p", func() {
			before = procBytes()
			done := make(chan struct{})
			go func() {
				memSink = make([]byte, 4<<20) // child allocates 4 MB
				close(done)
			}()
			<-done
			after = procBytes()
		})
	})
	if after-before < 4<<20 {
		t.Errorf("child's 4 MB allocation not attributed to its process: delta %d bytes", after-before)
	}
}

// TestDSTMemDeterminism enforces DST-MEMALLOC-DET (refined): the per-process counter
// is deterministic to the granularity the OOM fault needs — the budget-CROSSING
// decision is a deterministic function of the seed — while the exact byte count
// carries sub-observable runtime-pool-refill noise (the per-process analogue of the
// GC's DST-MEM-1: a sudog cache refill from a channel op is charged to whichever
// process empties the process-global, cross-run pool, so the raw count is not
// byte-exact across runs in concurrent programs). This concurrent program with
// channel synchronization exercises that pool churn; two same-seed runs must agree
// on the budget crossing and stay within the noise band — and must NOT diverge at
// the MB scale (a real attribution bug).
func TestDSTMemDeterminism(t *testing.T) {
	const budget = 4 << 20 // a meaningful OOM-style budget, far above the pool-noise floor
	const noise = 64 << 10 // sub-observable tolerance: real divergence is MB-scale, the noise ~hundreds of bytes
	type sample struct{ heavy, light int64 }
	run := func() sample {
		var s sample
		var mu sync.Mutex
		Run(7, func() {
			ch := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				Process("heavy", func() {
					allocSink(8 << 20) // crosses the budget
					<-ch
					v := procBytes()
					mu.Lock()
					s.heavy = v
					mu.Unlock()
				})
			}()
			go func() {
				defer wg.Done()
				Process("light", func() {
					allocSink(4096) // a few KB — under the budget
					ch <- struct{}{}
					v := procBytes()
					mu.Lock()
					s.light = v
					mu.Unlock()
				})
			}()
			wg.Wait()
		})
		return s
	}
	a, b := run(), run()

	absI := func(x int64) int64 {
		if x < 0 {
			return -x
		}
		return x
	}
	// (1) Counts are deterministic up to sub-observable pool noise — bounded, not divergent.
	if absI(a.heavy-b.heavy) > noise || absI(a.light-b.light) > noise {
		t.Errorf("per-process accounting diverged beyond the %d B pool-noise band: heavy %d vs %d, light %d vs %d",
			noise, a.heavy, b.heavy, a.light, b.light)
	}
	// (2) OOM-relevant determinism: the budget-crossing decision replays exactly (the
	//     sub-observable noise cannot flip a budget-scale crossing).
	if (a.heavy >= budget) != (b.heavy >= budget) || (a.light >= budget) != (b.light >= budget) {
		t.Errorf("budget-crossing nondeterministic vs %d B: heavy %d/%d light %d/%d", budget, a.heavy, b.heavy, a.light, b.light)
	}
	// Non-vacuous: the heavy process crosses the budget, the light one does not.
	if a.heavy < budget || a.light >= budget {
		t.Errorf("expected heavy>=%d>light; got heavy=%d light=%d", budget, a.heavy, a.light)
	}
}

// TestDSTMemProcessBound verifies the per-process accounting bound is enforced
// loudly: an over-bound process id panics (a non-silent limit) rather than silently
// dropping accounting. It calls the enforcement choke point (dstProcAllocEnsure,
// which Process calls) directly with a value far past any reasonable cap, so it
// stays valid if dstMaxSimProcs is retuned.
func TestDSTMemProcessBound(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("dstProcAllocEnsure past the process bound did not panic (silent cap)")
		}
	}()
	dstProcAllocEnsure(1 << 20) // far beyond dstMaxSimProcs; must panic
}

// TestDSTMemIndependent verifies two processes have independent counters: one
// allocating heavily does not inflate the other.
func TestDSTMemIndependent(t *testing.T) {
	var aBytes, bBytes int64
	Run(1, func() {
		Process("a", func() { aBytes = procBytes() }) // allocates ~nothing of its own
		Process("b", func() {
			memSink = make([]byte, 16<<20) // 16 MB
			bBytes = procBytes()
		})
	})
	if bBytes < 16<<20 {
		t.Errorf("process b accounted %d bytes, want >= 16 MB", bBytes)
	}
	if aBytes >= bBytes {
		t.Errorf("process a (%d) accounted >= process b (%d); counters not independent", aBytes, bBytes)
	}
}
