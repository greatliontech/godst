// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"testing"
	"unsafe"
)

//go:linkname dstAccessYield runtime.dstAccessYield
func dstAccessYield(addr unsafe.Pointer, write bool)

func TestExploreAccessLogOverflowReportsIncomplete(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	dstExploreInit(16, 64, 16, 0)
	x := 0
	_, _, tr := runOnce(1, nil, func() bool {
		dstAccessYield(unsafe.Pointer(&x), true)
		x = 1
		return false
	})
	if !tr.overflow {
		t.Fatalf("access-log overflow did not mark trace incomplete")
	}
}
