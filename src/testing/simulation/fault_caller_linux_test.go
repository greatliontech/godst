// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"fmt"
	"internal/synctest"
	"runtime"
	"strings"
	"testing"
	"time"
)

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
