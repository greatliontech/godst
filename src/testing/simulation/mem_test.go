// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import "testing"

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

// TestDSTMemDeterminism enforces DST-MEMALLOC-DET: same seed → identical per-process
// allocation byte counts.
func TestDSTMemDeterminism(t *testing.T) {
	run := func() [2]int64 {
		var got [2]int64
		Run(7, func() {
			Process("a", func() {
				memSink = make([]byte, 100000)
				got[0] = procBytes()
			})
			Process("b", func() {
				for i := 0; i < 64; i++ {
					memSink = make([]byte, 1000)
				}
				got[1] = procBytes()
			})
		})
		return got
	}
	a, b := run(), run()
	if a != b {
		t.Errorf("per-process allocation not reproducible across same-seed runs: %v vs %v", a, b)
	}
	if a[0] <= 0 || a[1] <= 0 {
		t.Errorf("processes accounted %v bytes, want both > 0", a)
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
