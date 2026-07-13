// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"context"
	"fmt"
	"internal/synctest"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
	_ "unsafe" // for go:linkname
)

//go:linkname dstGoroutineLeakGCFP runtime.dstGoroutineLeakGCFP
func dstGoroutineLeakGCFP()

//go:linkname dstGoroutineLeakPendingFP runtime.dstGoroutineLeakPendingFP
func dstGoroutineLeakPendingFP() bool

//go:linkname dstDisassociatedAllocFP runtime.dstDisassociatedAllocFP
func dstDisassociatedAllocFP() (before, after uint64)

// TestDSTFaultFromNonBubbleGoroutinePanics: every fault-injection and
// clock-fault API invoked during an active run from a goroutine OUTSIDE the
// run's bubble fails loudly — never executing at an OS wall-clock instant the
// seed does not control (crash, partition, disk) and never silently no-oping
// (clock faults) — exactly as the victim-naming rule already fails loud on a
// typo'd victim. Outside a run the APIs remain documented no-ops.
func TestDSTFaultFromNonBubbleGoroutinePanics(t *testing.T) {
	faults := []struct {
		name string
		call func()
	}{
		{"Crash", func() { Crash("h") }},
		{"CrashHost", func() { CrashHost("h") }},
		{"StepClock", func() { StepClock("h", time.Second) }},
		{"DriftClock", func() { DriftClock("h", 1000) }},
		{"Partition", func() { Partition("h", "g") }},
		{"Heal", func() { Heal("h", "g") }},
		{"Isolate", func() { Isolate("h") }},
		{"HealHost", func() { HealHost("h") }},
		{"PartitionOneWay", func() { PartitionOneWay("h", "g") }},
		{"PartitionRefuse", func() { PartitionRefuse("h", "g") }},
		{"Reset", func() { Reset("h", "g") }},
		{"ResetProcess", func() { ResetProcess("p") }},
		{"FailDisk", func() { FailDisk("h") }},
		{"HealDisk", func() { HealDisk("h") }},
		{"FailFile", func() { FailFile("h", "/f") }},
		{"HealFile", func() { HealFile("h", "/f") }},
		{"LimitDisk", func() { LimitDisk("h", 1<<20) }},
		{"UnlimitDisk", func() { UnlimitDisk("h") }},
		{"SlowDisk", func() { SlowDisk("h", time.Millisecond) }},
	}

	// The foreign goroutine exists BEFORE the run and calls each API mid-run —
	// once bare, once from inside a FOREIGN synctest bubble (a distinct
	// scheduling domain: merely being in some bubble must not pass the guard).
	inject := make(chan func())
	result := make(chan string)
	go func() {
		for call := range inject {
			func() {
				defer func() {
					if r := recover(); r != nil {
						result <- fmt.Sprint(r)
					} else {
						result <- ""
					}
				}()
				call()
			}()
		}
	}()
	defer close(inject)

	var got map[string]string
	Test(t, 1, func(t *testing.T) {
		Host("h", HostConfig{}, func() {})
		Host("g", HostConfig{}, func() {})
		done := make(chan struct{})
		go Process("p", func() { <-done })
		got = make(map[string]string, 2*len(faults))
		for _, f := range faults {
			inject <- f.call
			got[f.name] = <-result
			call := f.call
			inject <- func() { // a foreign bubble is still outside the run's
				var msg string
				synctest.Run(func() {
					defer func() {
						if r := recover(); r != nil {
							msg = fmt.Sprint(r)
						}
					}()
					call()
				})
				if msg != "" {
					panic(msg) // re-raise on the injector for uniform reporting
				}
			}
			got[f.name+"/foreign-bubble"] = <-result
		}
		close(done)
	})
	for _, f := range faults {
		for _, pos := range []string{f.name, f.name + "/foreign-bubble"} {
			msg := got[pos]
			if msg == "" {
				t.Errorf("%s during an active run did not panic", pos)
				continue
			}
			if !strings.Contains(msg, f.name) || !strings.Contains(msg, "outside the run's bubble") {
				t.Errorf("%s panic = %q, want the caller-position diagnostic naming the API", pos, msg)
			}
		}
	}
}

// TestDSTForeignRuntimeGCPanics: runtime.GC() invoked during an active run
// from a goroutine outside the run's bubble fails loudly. A foreign
// user-forced cycle would mark the bubble's heap (discovering finalizers and
// weak pointers) and zero the allocation-trigger counter at a wall-clock
// instant the seed does not control — the same silent-nondeterminism class
// the fault-API caller guard kills. A bubble goroutine's runtime.GC() stays
// sanctioned: it runs at that call's deterministic point in the schedule.
func TestDSTForeignRuntimeGCPanics(t *testing.T) {
	inject := make(chan func())
	result := make(chan string)
	go func() {
		for call := range inject {
			func() {
				defer func() {
					if r := recover(); r != nil {
						result <- fmt.Sprint(r)
					} else {
						result <- ""
					}
				}()
				call()
			}()
		}
	}()
	defer close(inject)

	var bare, foreignBubble string
	inBubble := false
	Test(t, 1, func(t *testing.T) {
		Host("h", HostConfig{}, func() {})
		inject <- func() { runtime.GC() }
		bare = <-result
		inject <- func() { // being in SOME bubble must not pass the guard
			var msg string
			synctest.Run(func() {
				defer func() {
					if r := recover(); r != nil {
						msg = fmt.Sprint(r)
					}
				}()
				runtime.GC()
			})
			if msg != "" {
				panic(msg) // re-raise on the injector for uniform reporting
			}
		}
		foreignBubble = <-result
		runtime.GC() // the run's own goroutine: a sanctioned, deterministic starter
		inBubble = true
	})
	for pos, msg := range map[string]string{"bare": bare, "foreign-bubble": foreignBubble} {
		if msg == "" {
			t.Errorf("foreign runtime.GC() (%s) during an active run did not panic", pos)
		} else if !strings.Contains(msg, "outside the run's bubble") {
			t.Errorf("foreign runtime.GC() (%s) panic = %q, want the caller-position diagnostic", pos, msg)
		}
	}
	if !inBubble {
		t.Error("in-bubble runtime.GC() did not complete")
	}
}

func TestDSTForeignGCControlPanics(t *testing.T) {
	oldPercent := debug.SetGCPercent(100)
	defer debug.SetGCPercent(oldPercent)

	inject := make(chan func())
	result := make(chan string)
	go func() {
		for call := range inject {
			func() {
				defer func() {
					if r := recover(); r != nil {
						result <- fmt.Sprint(r)
					} else {
						result <- ""
					}
				}()
				call()
			}()
		}
	}()
	defer close(inject)

	calls := []struct {
		name string
		call func()
	}{
		{name: "SetGCPercent", call: func() { debug.SetGCPercent(50) }},
		{name: "FreeOSMemory", call: debug.FreeOSMemory},
		{name: "goroutineLeakGC", call: dstGoroutineLeakGCFP},
	}
	panics := make(map[string]string)
	Test(t, 1, func(t *testing.T) {
		for _, tc := range calls {
			inject <- tc.call
			panics[tc.name] = <-result
		}
		if got := debug.SetGCPercent(100); got != 100 {
			t.Errorf("foreign SetGCPercent changed live GOGC to %d", got)
		}
		if got := debug.SetGCPercent(80); got != 100 {
			t.Errorf("bubble SetGCPercent old value = %d, want 100", got)
		}
		if got := debug.SetGCPercent(100); got != 80 {
			t.Errorf("bubble SetGCPercent restore old value = %d, want 80", got)
		}
		if dstGoroutineLeakPendingFP() {
			t.Error("foreign goroutineLeakGC armed the pending flag")
		}
	})
	for _, tc := range calls {
		if msg := panics[tc.name]; !strings.Contains(msg, "outside the run's bubble") {
			t.Errorf("foreign %s panic = %q, want caller-position refusal", tc.name, msg)
		}
	}
}

func TestDSTDisassociatedAllocationCounts(t *testing.T) {
	oldPercent := debug.SetGCPercent(100)
	defer debug.SetGCPercent(oldPercent)

	Test(t, 1, func(t *testing.T) {
		runtime.GC()
		before, after := dstDisassociatedAllocFP()
		if after <= before {
			t.Fatalf("dstHeapAlloc did not advance while bubble pointer was cleared: before=%d after=%d", before, after)
		}
	})
}

// TestDSTRunActivationExcludesInFlightGuardedOps: the run-START activation
// edge cannot split a guarded op — a guard that loaded runActive=false holds
// callerGate's read side through its op, and enterSimulation's flip takes the
// write side, so activation WAITS for the in-flight op to finish and the op
// completes wholly against pre-run state (the documented no-op). White-box:
// hold the gate as a guarded op would, start Run concurrently, and pin that
// activation does not happen until the hold is released. Mutation: dropping
// the callerGate.Lock around enterSimulation's CAS lets runActive flip while
// the read side is held, tripping the poll below.
func TestDSTRunActivationExcludesInFlightGuardedOps(t *testing.T) {
	for _, g := range []struct {
		name string
		hold func() func()
	}{
		{"fault-guard", func() func() { return requireBubbleFaultCaller("gate-probe") }},
		{"decl-guard", func() func() { return requireBubbleDeclCaller("gate-probe") }},
	} {
		t.Run(g.name, func(t *testing.T) {
			release := g.hold() // no run active: passes, holds the gate
			done := make(chan struct{})
			go func() {
				Run(1, func() {})
				close(done)
			}()
			// Real-time poll (no run is active yet): activation must not land
			// while the guarded op is in flight.
			for i := 0; i < 50; i++ {
				if runActive.Load() {
					release()
					<-done
					t.Fatal("Run activated while a guarded op held callerGate")
				}
				time.Sleep(time.Millisecond)
			}
			release()
			<-done // and with the gate released, activation and the run complete
		})
	}
}

func TestDSTActiveCallerGuardsReleaseAfterValidation(t *testing.T) {
	Run(1, func() {
		for _, guard := range []struct {
			name string
			call func() func()
		}{
			{"fault", func() func() { return requireBubbleFaultCaller("gate-probe") }},
			{"declaration", func() func() { return requireBubbleDeclCaller("gate-probe") }},
		} {
			release := guard.call()
			if !callerGate.TryLock() {
				release()
				t.Fatalf("active %s guard retained callerGate after validation", guard.name)
			}
			callerGate.Unlock()
			release()
		}
	})
}

func TestDSTActiveCallerGuardsDoNotAcquireBehindWriter(t *testing.T) {
	for _, guard := range []struct {
		name string
		call func() func()
	}{
		{"fault", func() func() { return requireBubbleFaultCaller("gate-probe") }},
		{"declaration", func() func() { return requireBubbleDeclCaller("gate-probe") }},
	} {
		t.Run(guard.name, func(t *testing.T) {
			bodyEntered := make(chan struct{})
			writerHeld := make(chan struct{})
			guardDone := make(chan struct{})
			finish := make(chan struct{})
			runDone := make(chan struct{})
			go func() {
				Run(1, func() {
					close(bodyEntered)
					<-writerHeld
					release := guard.call()
					release()
					close(guardDone)
					<-finish
				})
				close(runDone)
			}()
			<-bodyEntered
			callerGate.Lock()
			close(writerHeld)
			timedOut := false
			select {
			case <-guardDone:
			case <-time.After(time.Second):
				timedOut = true
			}
			callerGate.Unlock()
			if timedOut {
				<-guardDone
			}
			close(finish)
			<-runDone
			if timedOut {
				t.Fatal("active guard attempted RLock behind a pending writer")
			}
		})
	}
}

func TestDSTActiveProcessExitDoesNotAcquireBehindWriter(t *testing.T) {
	bodyEntered := make(chan struct{})
	processReady := make(chan struct{})
	exitNow := make(chan struct{})
	processDone := make(chan struct{})
	finish := make(chan struct{})
	runDone := make(chan struct{})
	go func() {
		Run(1, func() {
			go func() {
				Process("p", func() {
					close(processReady)
					<-exitNow
				})
				close(processDone)
			}()
			<-processReady
			close(bodyEntered)
			<-finish
		})
		close(runDone)
	}()
	<-bodyEntered
	callerGate.Lock()
	close(exitNow)
	timedOut := false
	select {
	case <-processDone:
	case <-time.After(time.Second):
		timedOut = true
	}
	callerGate.Unlock()
	if timedOut {
		<-processDone
	}
	close(finish)
	<-runDone
	if timedOut {
		t.Fatal("active Process exit attempted RLock behind a pending writer")
	}
}

func TestDSTKilledCallerGateWaitersDoNotStrandDeactivation(t *testing.T) {
	if os.Getenv("DST_CALLER_GATE_KILLED_WAITER") == "1" {
		runKilledCallerGateWaiters()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDSTKilledCallerGateWaitersDoNotStrandDeactivation$")
	cmd.Env = append(os.Environ(), "DST_CALLER_GATE_KILLED_WAITER=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("run deactivation hung behind a killed caller-gate reader:\n%s", out)
	}
	if err != nil {
		t.Fatalf("killed caller-gate waiter helper failed: %v\n%s", err, out)
	}
}

func runKilledCallerGateWaiters() {
	Run(1, func() {
		p2Ready := make(chan struct{})
		p2Stop := make(chan struct{})
		p2Done := make(chan struct{})
		Host("other", HostConfig{}, func() {
			go func() {
				Process("p2", func() {
					close(p2Ready)
					<-p2Stop
				})
				close(p2Done)
			}()
		})
		<-p2Ready

		exitReady := make(chan struct{})
		exitNow := make(chan struct{})
		Host("victim", HostConfig{}, func() {
			go Process("exiting", func() {
				close(exitReady)
				<-exitNow
			})
		})
		<-exitReady

		procTeardownMu.Lock()
		close(exitNow)
		crashStarted := make(chan struct{})
		go Host("victim", HostConfig{}, func() {
			close(crashStarted)
			Crash("p2")
		})
		<-crashStarted
		for range 20 {
			runtime.Gosched()
		}
		dstMarkHostGoroutinesCrashed(lookupHost("victim"))
		procTeardownMu.Unlock()

		close(p2Stop)
		<-p2Done
	})
}

// TestDSTRunDeactivationExcludesInFlightGuardedOps: the closing edge holds
// too — leaveSimulation's flip takes callerGate's write side, so a reader
// held across the run's end delays deactivation until it releases (white-box:
// the reader is taken directly, since a foreign guard call mid-run panics by
// design). Mutation: dropping the Lock in leaveSimulation lets runActive flip
// while the read side is held, tripping the poll.
func TestDSTRunDeactivationExcludesInFlightGuardedOps(t *testing.T) {
	bodyEntered := make(chan struct{})
	gateHeld := make(chan struct{})
	done := make(chan struct{})
	go func() {
		Run(1, func() {
			close(bodyEntered) // the run is active from here
			<-gateHeld         // and its body waits until the reader is in place
		})
		close(done)
	}()
	<-bodyEntered
	callerGate.RLock()
	close(gateHeld)
	for i := 0; i < 50; i++ {
		if !runActive.Load() {
			callerGate.RUnlock()
			<-done
			t.Fatal("Run deactivated while a guarded op held callerGate")
		}
		time.Sleep(time.Millisecond)
	}
	callerGate.RUnlock()
	<-done
	if runActive.Load() {
		t.Fatal("run did not deactivate after the gate was released")
	}
}

// TestDSTTopologyFromNonBubbleGoroutinePanics: the DECLARATION APIs (Host,
// Process) carry the fault APIs' caller-position guard — they mutate run
// state too (a mid-run Host re-declaration is a reboot: host-up relay plus
// clock re-establishment; Process starts SUT goroutines), so a foreign
// caller during an active run fails loudly instead of rebooting a machine or
// scheduling goroutines at a wall-clock instant the seed does not control.
// Both bare foreign goroutines and foreign synctest bubbles are outside the
// run's bubble. In-run declarations from bubble goroutines stay sanctioned;
// the guard fires before the intern tables are touched, so a refused
// declaration leaves no trace.
func TestDSTTopologyFromNonBubbleGoroutinePanics(t *testing.T) {
	decls := []struct {
		name string
		call func()
	}{
		{"Host", func() { Host("foreign-h", HostConfig{}, func() {}) }},
		{"Process", func() { Process("foreign-p", func() {}) }},
	}

	inject := make(chan func())
	result := make(chan string)
	go func() {
		for call := range inject {
			func() {
				defer func() {
					if r := recover(); r != nil {
						result <- fmt.Sprint(r)
					} else {
						result <- ""
					}
				}()
				call()
			}()
		}
	}()
	defer close(inject)

	var got map[string]string
	inBubble := false
	Test(t, 1, func(t *testing.T) {
		got = make(map[string]string, 2*len(decls))
		for _, d := range decls {
			inject <- d.call
			got[d.name] = <-result
			call := d.call
			inject <- func() { // a foreign bubble is still outside the run's
				var msg string
				synctest.Run(func() {
					defer func() {
						if r := recover(); r != nil {
							msg = fmt.Sprint(r)
						}
					}()
					call()
				})
				if msg != "" {
					panic(msg) // re-raise on the injector for uniform reporting
				}
			}
			got[d.name+"/foreign-bubble"] = <-result
		}
		// The refused declarations left no trace: the names were never
		// interned, so the victim rule still fails loud on them.
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "unknown host") {
					t.Errorf("CrashHost(\"foreign-h\") after a refused declaration = %v, want the unknown-host panic (the guard must fire before the intern tables)", r)
				}
			}()
			CrashHost("foreign-h")
		}()
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "unknown process") {
					t.Errorf("Crash(\"foreign-p\") after a refused declaration = %v, want the unknown-process panic (the guard must fire before the intern tables)", r)
				}
			}()
			Crash("foreign-p")
		}()
		// The run's own goroutines: sanctioned declarations still work.
		Host("h", HostConfig{}, func() {})
		done := make(chan struct{})
		go Process("p", func() { <-done })
		close(done)
		inBubble = true
	})
	for _, d := range decls {
		for _, pos := range []string{d.name, d.name + "/foreign-bubble"} {
			msg := got[pos]
			if msg == "" {
				t.Errorf("%s during an active run did not panic", pos)
				continue
			}
			if !strings.Contains(msg, d.name) || !strings.Contains(msg, "outside the run's bubble") {
				t.Errorf("%s panic = %q, want the caller-position diagnostic naming the API", pos, msg)
			}
		}
	}
	if !inBubble {
		t.Error("in-bubble declarations did not complete")
	}
}

// TestDSTTopologyOutsideRunKeepsBehavior: outside a run the declaration APIs
// keep their documented behavior — Host runs f, Process runs f; no panic.
func TestDSTTopologyOutsideRunKeepsBehavior(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("declaration API outside a run panicked: %v", r)
		}
	}()
	ranHost, ranProc := false, false
	Host("outside-h", HostConfig{}, func() { ranHost = true })
	Process("outside-p", func() { ranProc = true })
	if !ranHost || !ranProc {
		t.Errorf("outside a run Host/Process must still run f (host=%v proc=%v)", ranHost, ranProc)
	}
}

// TestDSTFaultOutsideRunIsNoop: the guard changes nothing outside a run —
// every fault API remains the documented no-op.
func TestDSTFaultOutsideRunIsNoop(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("fault API outside a run panicked: %v", r)
		}
	}()
	Crash("nobody")
	CrashHost("nobody")
	StepClock("nobody", time.Second)
	DriftClock("nobody", 1000)
	Partition("a", "b")
	Heal("a", "b")
	Isolate("a")
	HealHost("a")
	PartitionOneWay("a", "b")
	PartitionRefuse("a", "b")
	Reset("a", "b")
	ResetProcess("p")
	FailDisk("a")
	HealDisk("a")
	FailFile("a", "/f")
	HealFile("a", "/f")
	LimitDisk("a", 1)
	UnlimitDisk("a")
	SlowDisk("a", time.Millisecond)
}
