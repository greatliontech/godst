// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && 386

package syscall_test

import (
	"runtime"
	"syscall"
	"testing"
)

func TestDSTSocketcallStackBackedResult(t *testing.T) {
	testDSTSocketcallStackBackedResult(t, 512)
}

//go:noinline
func testDSTSocketcallStackBackedResult(t *testing.T, depth int) {
	var pad [128]byte
	pad[0] = byte(depth)

	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("Socketpair at stack depth %d: %v", depth, err)
	}
	closePair := func() error {
		var first error
		for _, fd := range fds {
			if err := syscall.Close(fd); err != nil && first == nil {
				first = err
			}
		}
		return first
	}
	typ, err := syscall.GetsockoptInt(fds[0], syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil {
		t.Fatalf("GetsockoptInt at stack depth %d: %v (closing pair: %v)", depth, err, closePair())
	}
	if typ != syscall.SOCK_STREAM {
		t.Fatalf("SO_TYPE at stack depth %d = %d, want %d (closing pair: %v)", depth, typ, syscall.SOCK_STREAM, closePair())
	}
	if err := closePair(); err != nil {
		t.Fatalf("closing socket pair at stack depth %d: %v", depth, err)
	}

	if depth > 0 {
		testDSTSocketcallStackBackedResult(t, depth-1)
	}
	runtime.KeepAlive(&pad)
}
