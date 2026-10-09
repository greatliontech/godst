// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package runtime_test

import (
	"internal/testenv"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"testing/simulation"
)

func chainedFinalizerFrame(p *finalizerFrameObject) [65536]byte {
	var result [65536]byte
	result[p.value] = byte(p.value)
	if p.value == 1 {
		// The second callback is discovered after this batch was detached,
		// so the same drain has to take a second batch before returning.
		installFinalizerFrame(2, chainedFinalizerFrame)
		runtime.GC()
	}
	finalizerFrameAck <- p.value
	return result
}

func TestDSTFinalizerFrameOwnedByDrain(t *testing.T) {
	if os.Getenv("TEST_DST_FINALIZER_FRAME_CHILD") != "1" {
		cmd := testenv.CleanCmdEnv(exec.CommandContext(t.Context(), testenv.Executable(t), "-test.run=^TestDSTFinalizerFrameOwnedByDrain$", "-test.v", "-test.timeout=30s"))
		cmd.Env = append(cmd.Env, "TEST_DST_FINALIZER_FRAME_CHILD=1", "GODEBUG=memprofilerate=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "PASS\n") {
			t.Fatalf("bubble frame child: %v\n%s", err, out)
		}
		t.Logf("%s", out)
		return
	}
	prior := finalizerScratchAllocations("runtime.dstDrainFinq")
	for run := 0; run < 2; run++ {
		simulation.Run(13, func() {
			finalizerFrameAck = make(chan int, 2)
			installFinalizerFrame(1, chainedFinalizerFrame)
			runtime.GC()
			if first, second := <-finalizerFrameAck, <-finalizerFrameAck; first != 1 || second != 2 {
				panic("finalizers ran out of order")
			}
		})
		got := finalizerScratchAllocations("runtime.dstDrainFinq")
		if got != prior+1 {
			t.Fatalf("run %d allocated %d drain scratch frames, want 1", run, got-prior)
		}
		prior = got
	}
}
