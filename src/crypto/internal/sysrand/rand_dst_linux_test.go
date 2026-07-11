// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && cgo && linux

package sysrand_test

import (
	"crypto/internal/sysrand/internal/seccomp"
	crand "crypto/rand"
	"internal/testenv"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/simulation"
)

var dstFallbackSink byte

func TestDSTForeignNoGetrandom(t *testing.T) {
	if os.Getenv("GO_DST_GETRANDOM_DISABLED") == "1" {
		var start, done atomic.Bool
		go func() {
			for !start.Load() {
				runtime.Gosched()
			}
			var b [32]byte
			_, _ = crand.Read(b[:])
			dstFallbackSink ^= b[0]
			done.Store(true)
		}()
		simulation.Run(1, func() {
			start.Store(true)
			for !done.Load() {
				runtime.Gosched()
			}
		})
		allocs := testing.AllocsPerRun(10, func() {
			var b [32]byte
			_, _ = crand.Read(b[:])
			dstFallbackSink ^= b[0]
		})
		if allocs != 0 {
			t.Fatalf("fallback crypto/rand allocations = %v, want 0", allocs)
		}
		return
	}

	if testing.Short() {
		t.Skip("skipping test in short mode")
	}
	testenv.MustHaveExec(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		if err := seccomp.DisableGetrandom(); err != nil {
			t.Errorf("failed to disable getrandom: %v", err)
			return
		}
		cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestDSTForeignNoGetrandom$")
		cmd.Env = append(os.Environ(), "GO_DST_GETRANDOM_DISABLED=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("fallback subprocess failed: %v\n%s", err, out)
		}
	}()
	<-done
}
