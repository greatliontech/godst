// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"internal/testenv"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

type finalizerFrameObject struct {
	value int
	ptr   *byte
}

var finalizerFrameAck chan int

func smallFinalizerFrame(p *finalizerFrameObject) [4096]byte {
	var result [4096]byte
	result[p.value%len(result)] = byte(p.value)
	finalizerFrameAck <- p.value
	return result
}

func largeFinalizerFrame(value any) [65536]byte {
	p := value.(*finalizerFrameObject)
	var result [65536]byte
	result[p.value%len(result)] = byte(p.value)
	finalizerFrameAck <- p.value
	return result
}

//go:noinline
func installFinalizerFrame(value int, finalizer any) {
	p := &finalizerFrameObject{value: value, ptr: new(byte)}
	runtime.SetFinalizer(p, finalizer)
}

// TestFinalizerFrameSurvivesBatches checks the allocation site's cumulative
// count, rather than total process allocations or wall time. Each callback
// finishes and the driver parks before the next batch can become eligible.
func TestFinalizerFrameSurvivesBatches(t *testing.T) {
	if os.Getenv("TEST_FINALIZER_FRAME_CHILD") != "1" {
		cmd := testenv.CleanCmdEnv(exec.CommandContext(t.Context(), testenv.Executable(t), "-test.run=^TestFinalizerFrameSurvivesBatches$", "-test.v", "-test.timeout=30s"))
		cmd.Env = append(cmd.Env, "TEST_FINALIZER_FRAME_CHILD=1", "GODEBUG=memprofilerate=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "PASS\n") {
			t.Fatalf("finalizer frame child: %v\n%s", err, out)
		}
		t.Logf("%s", out)
		return
	}
	debug.SetGCPercent(-1)
	finalizerFrameAck = make(chan int, 1)
	drain := func(value int, fn any) {
		t.Helper()
		installFinalizerFrame(value, fn)
		runtime.GC()
		select {
		case got := <-finalizerFrameAck:
			if got != value {
				t.Fatalf("finalized %d, want %d", got, value)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("finalizer did not execute")
		}
		if !runtime.BlockUntilEmptyFinalizerQueue(int64(10 * time.Second)) {
			t.Fatal("finalizer driver did not finish its batch")
		}
	}
	count := func() int64 {
		return finalizerScratchAllocations("runtime.runFinalizers")
	}
	drain(1, smallFinalizerFrame)
	warm := count()
	if warm == 0 {
		t.Fatal("scratch allocation site was not observed")
	}
	for i := 2; i <= 5; i++ {
		drain(i, smallFinalizerFrame)
	}
	if got := count(); got != warm {
		t.Fatalf("equal-size batches allocated %d new scratch frames", got-warm)
	}
	drain(6, largeFinalizerFrame)
	grown := count()
	if grown != warm+1 {
		t.Fatalf("larger return allocated %d scratch frames, want 1", grown-warm)
	}
	for i := 7; i <= 10; i++ {
		drain(i, smallFinalizerFrame)
		drain(i+10, largeFinalizerFrame)
	}
	if got := count(); got != grown {
		t.Fatalf("equal/smaller batches allocated %d new scratch frames", got-grown)
	}
	t.Logf("scratch allocations: warm=%d, after growth=%d, subsequent batches=0", warm, grown)
}

func finalizerScratchAllocations(driver string) int64 {
	// Publish the allocation records, including already freed frames.
	runtime.GC()
	runtime.GC()
	n, _ := runtime.MemProfile(nil, true)
	for {
		records := make([]runtime.MemProfileRecord, n+16)
		var ok bool
		n, ok = runtime.MemProfile(records, true)
		if !ok {
			continue
		}
		var total int64
		for _, record := range records[:n] {
			frames := runtime.CallersFrames(record.Stack())
			frame, more := frames.Next()
			for more && strings.HasPrefix(frame.Function, "runtime.mallocgc") {
				frame, more = frames.Next()
			}
			if frame.Function != "runtime.runFinqBlocks" || !more {
				continue
			}
			frame, _ = frames.Next()
			if frame.Function == driver {
				total += record.AllocObjects
			}
		}
		return total
	}
}
