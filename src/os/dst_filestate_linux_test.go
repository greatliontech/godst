// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// TestDSTFileStateRowReaped pins the out-of-line state table's one
// otherwise-unenforced leg: a row's weak backref dies with its file and
// the sweep then releases the row, so the table cannot grow across the
// many simulations a single test process runs. The sweep is
// ownership-blind — no run-scoped callback channel is involved — so
// this probe exercises the identical path a simulated process's rows
// take (os/dst_filestate.go).
func TestDSTFileStateRowReaped(t *testing.T) {
	present := os.DSTFileStateReapProbe()
	deadline := time.Now().Add(10 * time.Second)
	for present() {
		if time.Now().After(deadline) {
			t.Fatal("state row for a dead file was never reaped")
		}
		runtime.GC()
		os.DSTFileStateSweep()
		time.Sleep(time.Millisecond)
	}
}
