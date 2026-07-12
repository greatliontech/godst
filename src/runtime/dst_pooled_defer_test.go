// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package runtime_test

import (
	"runtime"
	"testing"
	"testing/simulation"
)

func heapDeferNoop() {}

//go:noinline
func allocateHeapDefers(n int) {
	for i := 0; i < n; i++ {
		defer heapDeferNoop()
	}
}

func TestDSTPooledDeferAccounting(t *testing.T) {
	simulation.Run(1, func() {
		before := runtime.DstPooledAllocBytes()
		allocateHeapDefers(50000)
		after := runtime.DstPooledAllocBytes()
		if after <= before {
			t.Fatalf("heap defers added %d pooled bytes, want > 0", after-before)
		}
		runtime.GC()
		if marked := runtime.DstPooledMarkedBytes(); marked != after {
			t.Fatalf("pooled marked snapshot = %d, want current pooled allocation %d", marked, after)
		}
	})
}
