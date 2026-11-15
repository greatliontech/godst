// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"internal/race"
	"os"
	"testing"
)

func TestDSTProcessSameHostConcurrentAdmissionRemainsAllowed(t *testing.T) {
	pids := make(chan int, 2)
	release := make(chan struct{})
	var first, second int
	Run(1, func() {
		Host("H", func() {
			for range 2 {
				go Process("p", func() {
					pids <- os.Getpid()
					<-release
				})
			}
			first, second = <-pids, <-pids
			close(release)
		})
	})
	if first == second {
		t.Fatalf("concurrent same-host invocations reused pid %d", first)
	}
}

func TestDSTProcessCrossHostAdmissionAtomicDPOR(t *testing.T) {
	// Access-level race instrumentation creates a large equivalent tail after
	// admission. Bound the walk explicitly; every visited schedule checks that
	// concurrent same-name Process declarations on distinct hosts succeed with
	// independent host-scoped identities and crash domains.
	res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 64, MaxSteps: 256}, func() error {
		start := make(chan struct{})
		ready := make(chan struct{}, 2)
		results := make(chan string, 2)
		attempt := func(host string) {
			go Host(host, func() {
				ready <- struct{}{}
				<-start
				Process("p", func() {
					results <- host
					select {}
				})
			})
		}
		attempt("A")
		attempt("B")
		<-ready
		<-ready
		close(start)
		first, second := <-results, <-results
		if !((first == "A" && second == "B") || (first == "B" && second == "A")) {
			panic("same-name process admission across two hosts did not admit both")
		}
		procA := lookupProc(lookupHost("A"), "p")
		procB := lookupProc(lookupHost("B"), "p")
		if procA == 0 || procB == 0 || procA == procB {
			panic("same-name processes across hosts shared or dropped logical proc IDs")
		}
		instsA := activeProcInstances(procA)
		instsB := activeProcInstances(procB)
		if len(instsA) != 1 || len(instsB) != 1 || instsA[0].slot == instsB[0].slot {
			panic("cross-host same-name processes did not get distinct live instances and slots")
		}
		CrashProcess("A", "p")
		if len(activeProcInstances(procA)) != 0 || len(activeProcInstances(procB)) != 1 {
			panic("CrashProcess(\"A\", \"p\") disturbed host B's process")
		}
		CrashHost("B")
		if len(activeProcInstances(procB)) != 0 {
			panic("CrashHost(\"B\") spared host B's process")
		}
		return nil
	})
	if len(res.Failures) != 0 {
		t.Fatalf("cross-host admission exploration found failures: %#v", res.Failures)
	}
	if race.Enabled && res.Schedules < 2 {
		t.Fatalf("cross-host admission exploration covered %d schedule(s), want competing orders", res.Schedules)
	}
}
