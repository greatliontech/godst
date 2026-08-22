// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && amd64

package syscall_test

import (
	"strings"
	"syscall"
	"testing"
	"testing/simulation"
	_ "unsafe" // for go:linkname
)

// syscall.gettimeofday is the push-linknamed entry golang.org/x/sys/unix's
// amd64 assembly jumps to (JMP syscall·gettimeofday(SB)). Upstream exports the
// raw vDSO assembly under that name; the fork exports the fenced Go wrapper
// instead, so the entry — not only Gettimeofday/Time — refuses a bubble
// goroutine's host wall-clock read.
//
//go:linkname dstGettimeofdayEntry syscall.gettimeofday
func dstGettimeofdayEntry(tv *syscall.Timeval) syscall.Errno

func TestDSTGettimeofdayEntryRequiresCapability(t *testing.T) {
	var panicValue any
	simulation.Run(1, func() {
		defer func() { panicValue = recover() }()
		var tv syscall.Timeval
		dstGettimeofdayEntry(&tv)
	})
	if panicValue == nil || !strings.Contains(panicValue.(string), "unsupported under deterministic simulation") {
		t.Fatalf("syscall.gettimeofday panic = %v, want the fence shape", panicValue)
	}
}
