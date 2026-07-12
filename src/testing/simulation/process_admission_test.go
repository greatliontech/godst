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
		Host("H", HostConfig{}, func() {
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
	// admission. Bound the walk explicitly; every visited schedule checks the
	// complete admission and crash invariant, and BudgetHit reports the cap.
	res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 64, MaxSteps: 256}, func() bool {
		start := make(chan struct{})
		ready := make(chan struct{}, 2)
		results := make(chan string, 2)
		attempt := func(host string) {
			go Host(host, HostConfig{}, func() {
				defer func() {
					if recover() != nil {
						results <- "refused"
					}
				}()
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
		winner := first
		if winner == "refused" {
			winner = second
		}
		if winner == "refused" || first != "refused" && second != "refused" {
			panic("same-name process admission did not produce exactly one winner and one refusal")
		}
		proc := internProc("p")
		pids := activeProcPIDs(proc)
		activeProcs.mu.Lock()
		host := activeProcs.host[proc]
		activeProcs.mu.Unlock()
		if len(pids) != 1 || host != lookupHost(winner) {
			panic("refused process start published liveness or winner host was lost")
		}
		CrashHost("A")
		CrashHost("B")
		if len(activeProcPIDs(proc)) != 0 {
			panic("crashing both candidate hosts spared the admitted process")
		}
		return false
	})
	if len(res.Failures) != 0 {
		t.Fatalf("cross-host admission exploration found failures: %#v", res.Failures)
	}
	if race.Enabled && res.Schedules < 2 {
		t.Fatalf("cross-host admission exploration covered %d schedule(s), want competing orders", res.Schedules)
	}
}
