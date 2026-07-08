// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"syscall"
	"testing"
	"testing/simulation"
)

func TestDSTRawSyscallNoErrorIdentityNotFenced(t *testing.T) {
	simulation.Run(1, func() {
		if pid := syscall.Getpid(); pid <= 0 {
			t.Fatalf("syscall.Getpid = %d, want host pid", pid)
		}
		_ = syscall.Getppid()
		_ = syscall.Gettid()
		_ = syscall.Getuid()
		_ = syscall.Getgid()
		_ = syscall.Geteuid()
		_ = syscall.Getegid()
	})
}
