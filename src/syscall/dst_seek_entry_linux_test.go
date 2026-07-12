// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (386 || arm)

package syscall_test

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/simulation"
	_ "unsafe" // for go:linkname
)

//go:linkname dstSeekEntry syscall.seek
func dstSeekEntry(fd int, offset int64, whence int) (int64, syscall.Errno)

func TestDSTSeekEntryRequiresCapability(t *testing.T) {
	host, err := os.CreateTemp("", "dst-seek-entry")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(host.Name())
	defer host.Close()

	var panicValue any
	simulation.Run(1, func() {
		defer func() { panicValue = recover() }()
		dstSeekEntry(int(host.Fd()), 0, 0)
	})
	if panicValue == nil || !strings.Contains(panicValue.(string), "unsupported under deterministic simulation") {
		t.Fatalf("syscall.seek panic = %v, want the fence shape", panicValue)
	}
}
