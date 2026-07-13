// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import "testing"

func TestDSTHostScopeMetadataIsNotProcessAllocation(t *testing.T) {
	var before, after int64
	Run(1, func() {
		Process("p", func() {
			host, proc := dstCurrentNode()
			before = dstProcAllocBytes(proc)
			oldHost, oldProc := dstPushHostScope(host, proc)
			after = dstProcAllocBytes(proc)
			dstPopHostScope(oldHost, oldProc)
		})
	})
	if after != before {
		t.Fatalf("Host scope metadata changed process allocation from %d to %d bytes", before, after)
	}
}
