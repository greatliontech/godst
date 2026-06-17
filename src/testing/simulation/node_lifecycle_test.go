// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// TestDSTNodeRestart pins the restart contract (docs/dst/faults.md:375): re-running
// a process (same name, same host) gets a NEW pid, but host-level identity
// (hostname, NumCPU, clock) is unchanged, and its per-process allocation accounting
// ACCUMULATES — the counter is keyed by the interned process id, which a restart
// reuses, so the OOM metric carries across a restart.
func TestDSTNodeRestart(t *testing.T) {
	var pid1, pid2, cpu1, cpu2 int
	var host1, host2 string
	var mem1, mem2 int64
	var rootBase, wall1, wall2 time.Time
	Run(1, func() {
		rootBase = time.Now() // root, unskewed
		Host("h", HostConfig{Clock: Skew(50 * time.Millisecond), Hostname: "node-x", NumCPU: 3}, func() {
			Process("p", func() {
				memSink = make([]byte, 256<<10)
				pid1, host1, cpu1, mem1, wall1 = os.Getpid(), hostname(), runtime.NumCPU(), procBytes(), time.Now()
			})
			Process("p", func() { // restart: same name, same host
				memSink = make([]byte, 256<<10)
				pid2, host2, cpu2, mem2, wall2 = os.Getpid(), hostname(), runtime.NumCPU(), procBytes(), time.Now()
			})
		})
	})

	if pid1 == pid2 {
		t.Errorf("restart kept pid %d; a restarted process must get a new pid", pid1)
	}
	if host1 != "node-x" || host2 != "node-x" {
		t.Errorf("hostname not stable across restart: %q then %q, want node-x", host1, host2)
	}
	if cpu1 != 3 || cpu2 != 3 {
		t.Errorf("NumCPU not stable across restart: %d then %d, want 3", cpu1, cpu2)
	}
	if d1, d2 := wall1.Sub(rootBase), wall2.Sub(rootBase); d1 != 50*time.Millisecond || d2 != 50*time.Millisecond {
		t.Errorf("host clock skew not stable across restart: %v then %v, want 50ms", d1, d2)
	}
	if mem2 <= mem1 {
		t.Errorf("per-process accounting did not accumulate across restart: %d then %d (must carry over the interned process id)", mem1, mem2)
	}
}

// TestDSTNodeNested verifies save/restore under nested Host/Process declarations
// (beyond the panic-restore that node_test.go covers): the inner declaration stamps
// a distinct identity for its dynamic extent, and the outer identity is restored
// when it returns — for both the host and process axes.
func TestDSTNodeNested(t *testing.T) {
	var outer, inner, afterInner nodeIDs
	var outerHN, innerHN string
	var pOuter, pInner, afterPInner nodeIDs
	Run(1, func() {
		Host("outer", HostConfig{Hostname: "outer-h"}, func() {
			outer, outerHN = curNode(), hostname()
			Host("inner", HostConfig{Hostname: "inner-h"}, func() {
				inner, innerHN = curNode(), hostname()
			})
			afterInner = curNode() // restored to outer

			Process("po", func() {
				pOuter = curNode()
				Process("pi", func() { pInner = curNode() }) // nested process
				afterPInner = curNode()                      // restored to po
			})
		})
	})

	if inner.host == outer.host {
		t.Errorf("nested Host did not change host id: outer=%v inner=%v", outer, inner)
	}
	if afterInner != outer {
		t.Errorf("host identity not restored after nested Host: got %v, want %v", afterInner, outer)
	}
	if outerHN != "outer-h" || innerHN != "inner-h" {
		t.Errorf("nested hostnames wrong: outer=%q inner=%q", outerHN, innerHN)
	}
	if pInner.proc == pOuter.proc {
		t.Errorf("nested Process did not change process id: outer=%v inner=%v", pOuter, pInner)
	}
	if pInner.host != pOuter.host {
		t.Errorf("nested Process changed host (should stay on the same host): outer=%v inner=%v", pOuter, pInner)
	}
	if afterPInner != pOuter {
		t.Errorf("process identity not restored after nested Process: got %v, want %v", afterPInner, pOuter)
	}
}

// TestDSTNodeMidRunJoin models a node joining mid-run (faults.md: Host/Process are
// callable at any time, since there is no os/exec under DST). After the simulation
// clock has already advanced and other work has run, a freshly declared host gets a
// correct, distinct identity and its own filesystem.
func TestDSTNodeMidRunJoin(t *testing.T) {
	var earlyHN, joinerHN string
	var earlyHost, joinerHost uint32
	var joinerFile string
	var advanced time.Duration
	Run(1, func() {
		start := time.Now()
		Host("early", HostConfig{}, func() {
			earlyHN = hostname()
			earlyHost, _ = dstCurrentNode()
			os.WriteFile("/d", []byte("early"), 0o644)
		})
		// Let the clock advance and a background goroutine run before the join.
		done := make(chan struct{})
		go func() { time.Sleep(time.Second); close(done) }()
		<-done
		advanced = time.Since(start)

		Host("joiner", HostConfig{Hostname: "late-joiner"}, func() {
			joinerHN = hostname()
			joinerHost, _ = dstCurrentNode()
			// the joiner's filesystem is its own — it must not see early's /d.
			if _, err := os.ReadFile("/d"); err == nil {
				joinerFile = "LEAK: joiner sees early's /d"
			} else {
				os.WriteFile("/d", []byte("joiner"), 0o644)
				b, _ := os.ReadFile("/d")
				joinerFile = string(b)
			}
		})
	})

	if advanced != time.Second {
		t.Errorf("clock advanced %v before the join, want 1s", advanced)
	}
	if earlyHN != "early" || joinerHN != "late-joiner" {
		t.Errorf("hostnames: early=%q joiner=%q", earlyHN, joinerHN)
	}
	if earlyHost == joinerHost || joinerHost == 0 {
		t.Errorf("mid-run joiner did not get a distinct host id: early=%d joiner=%d", earlyHost, joinerHost)
	}
	if joinerFile != "joiner" {
		t.Errorf("mid-run joiner filesystem wrong: %q (want isolated, own write/read)", joinerFile)
	}
}
