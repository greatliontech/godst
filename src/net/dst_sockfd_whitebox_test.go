// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package net

import (
	"testing"
	"testing/simulation"
)

// TestDSTNetSockFDReuse: freed virtual socket descriptors are reused (a
// real kernel reuses closed fds), so a long churn of dials never exhausts
// the space with a sim-only EMFILE.
func TestDSTNetSockFDReuse(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		o1 := newDstSockOpts()
		a, ok := dstSockFDAlloc(o1)
		if !ok {
			t.Error("alloc failed")
		}
		dstSockFDFree(a, o1)
		dstSockFDFree(a, o1) // double free: owner no longer registered, no-op
		o2 := newDstSockOpts()
		b, _ := dstSockFDAlloc(o2)
		c, _ := dstSockFDAlloc(newDstSockOpts())
		if b != a {
			t.Errorf("freed descriptor not reused: freed %d, got %d", a, b)
		}
		if c == b {
			t.Errorf("double free double-issued descriptor %d", c)
		}
		// free -> realloc -> stale free: the OLD owner's late free must not
		// unregister the NEW socket (the ownership guard).
		dstSockFDFree(b, o1)
		if got := dstSockFDLookup(b); got != o2 {
			t.Errorf("stale free unregistered the reissued descriptor %d", b)
		}
		dstSockFDFree(b, o2)
		if dstSockFDLookup(b) != nil {
			t.Errorf("owner free did not retire descriptor %d", b)
		}
	})
}
