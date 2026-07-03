// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"reflect"
	"sync"
	"testing"
)

type nodeIDs struct{ host, proc uint32 }

func curNode() nodeIDs { h, p := dstCurrentNode(); return nodeIDs{h, p} }

// TestDSTNodeIdentity verifies the L1 host/process identity model: the default
// (root) identity is (0,0); Host/Process stamp nonzero ids; a child goroutine
// inherits its parent's identity; the stamp is restored when the body returns;
// co-located processes share a host while a bare process gets its own.
func TestDSTNodeIdentity(t *testing.T) {
	var (
		root, p1, p1child, afterP1 nodeIDs
		h1a, h1b, afterH1, n3      nodeIDs
	)

	Run(1, func() {
		root = curNode()

		Process("p1", func() {
			p1 = curNode()
			done := make(chan struct{})
			go func() {
				p1child = curNode()
				close(done)
			}()
			<-done
		})
		afterP1 = curNode()

		Host("h1", HostConfig{}, func() {
			Process("a", func() { h1a = curNode() })
			Process("b", func() { h1b = curNode() })
		})
		afterH1 = curNode()

		Process("n3", func() { n3 = curNode() })
	})

	if root != (nodeIDs{0, 0}) {
		t.Errorf("root identity = %+v, want {0 0}", root)
	}
	if p1.host == 0 || p1.proc == 0 {
		t.Errorf("Process p1 identity = %+v, want both nonzero", p1)
	}
	if p1child != p1 {
		t.Errorf("child identity = %+v, want inherited %+v", p1child, p1)
	}
	if afterP1 != root {
		t.Errorf("identity after Process returns = %+v, want restored %+v", afterP1, root)
	}
	if afterH1 != root {
		t.Errorf("identity after Host returns = %+v, want restored %+v", afterH1, root)
	}
	if h1a.host != h1b.host {
		t.Errorf("co-located processes on different hosts: %+v vs %+v", h1a, h1b)
	}
	if h1a.proc == h1b.proc {
		t.Errorf("co-located processes share a proc id: %+v vs %+v", h1a, h1b)
	}
	if n3.host == 0 || n3.proc == 0 {
		t.Errorf("bare Process identity = %+v, want both nonzero", n3)
	}
	if n3.host == h1a.host {
		t.Errorf("bare Process shares its implicit host with h1: n3=%+v h1=%+v", n3, h1a)
	}
}

// TestDSTNodeRestoreOnPanic checks that the identity stamp is restored when a
// Host/Process body exits *abnormally* (panic, and by the same defer mechanism
// runtime.Goexit / t.Fatal), not only on normal return — a stamp that leaked past a
// crashing body would mis-attribute later goroutines (the DST-FAULT-VICTIM
// foundation a process-crash fault later relies on).
func TestDSTNodeRestoreOnPanic(t *testing.T) {
	var afterPanic nodeIDs
	Run(1, func() {
		func() {
			defer func() {
				_ = recover()
				afterPanic = curNode()
			}()
			Process("p", func() { panic("boom") })
		}()
	})
	if afterPanic != (nodeIDs{0, 0}) {
		t.Errorf("identity after a panicking Process body = %+v, want restored {0 0}", afterPanic)
	}
}

// TestDSTNodeReset checks that the name→id interning resets per run, so id
// assignment is a function of call order *within* a run. Two runs declaring
// *different* single processes must both get the same fresh ids; without the reset
// the second run's process would continue the first run's counter.
func TestDSTNodeReset(t *testing.T) {
	var a, b nodeIDs
	Run(1, func() { Process("alpha", func() { a = curNode() }) })
	Run(1, func() { Process("beta", func() { b = curNode() }) })
	if a != b {
		t.Errorf("per-run interning reset broken: alpha=%+v beta=%+v, want equal fresh ids", a, b)
	}
	if a != (nodeIDs{1, 1}) {
		t.Errorf("first process ids = %+v, want {1 1} (ids start fresh each run)", a)
	}
}

// TestDSTNodeDeterministic checks that the same seed assigns the same ids to the
// same declaration sequence (the interning reset + reproducibility).
func TestDSTNodeDeterministic(t *testing.T) {
	run := func() [3]nodeIDs {
		var got [3]nodeIDs
		Run(7, func() {
			Host("h1", HostConfig{}, func() {
				Process("a", func() { got[0] = curNode() })
				Process("b", func() { got[1] = curNode() })
			})
			Process("c", func() { got[2] = curNode() })
		})
		return got
	}
	if a, b := run(), run(); a != b {
		t.Errorf("non-deterministic node ids across same-seed runs:\n a=%+v\n b=%+v", a, b)
	}
}

// TestDSTNodeConcurrentDeterministic checks that id assignment under *concurrent*
// declarations is deterministic — the schedule (hence mutex-acquisition order in
// the interner) is a function of the seed, so the name→id mapping replays.
func TestDSTNodeConcurrentDeterministic(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e"}
	run := func() map[string]nodeIDs {
		got := map[string]nodeIDs{}
		Run(42, func() {
			var mu sync.Mutex
			var wg sync.WaitGroup
			for _, name := range names {
				wg.Add(1)
				go func(name string) {
					defer wg.Done()
					Process(name, func() {
						mu.Lock()
						got[name] = curNode()
						mu.Unlock()
					})
				}(name)
			}
			wg.Wait()
		})
		return got
	}
	a, b := run(), run()
	if len(a) != len(names) {
		t.Fatalf("captured %d of %d processes: %+v", len(a), len(names), a)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("non-deterministic concurrent node ids:\n a=%+v\n b=%+v", a, b)
	}
}

// TestDSTNodeDeclareBeforeAnyRun: Host before the first Run in the process must be
// inert (the documented outside-a-run behavior), not a nil-map panic — the registry
// maps are created by the run envelope's nodeRegReset, which has not run yet. The
// pre-first-Run state is recreated directly (tests in this binary may already have
// run).
func TestDSTNodeDeclareBeforeAnyRun(t *testing.T) {
	nodeReg.mu.Lock()
	saveH, saveP := nodeReg.hosts, nodeReg.procs
	nodeReg.hosts, nodeReg.procs = nil, nil
	nodeReg.mu.Unlock()
	defer func() {
		nodeReg.mu.Lock()
		nodeReg.hosts, nodeReg.procs = saveH, saveP
		nodeReg.mu.Unlock()
	}()
	ran := false
	Host("pre-run-host", HostConfig{}, func() { ran = true })
	if !ran {
		t.Fatal("Host outside any run did not run its body")
	}
}
