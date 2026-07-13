// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDSTHostInvalidClockDeclarationIsStateNeutral(t *testing.T) {
	type identity struct {
		hostname string
		cpus     int
		now      time.Time
	}
	var panicValues []any
	var bodyRan bool
	Run(1, func() {
		rootHost, rootProc := dstCurrentNode()
		rootNow := time.Now()
		probe := make(chan struct{})
		observed := make(chan identity, 1)
		Host("h", HostConfig{Hostname: "old-name", NumCPU: 3, Clock: Skew(50 * time.Millisecond)}, func() {
			go func() {
				<-probe
				hostname, _ := os.Hostname()
				observed <- identity{hostname: hostname, cpus: runtime.NumCPU(), now: time.Now()}
			}()
		})
		for range 2 {
			func() {
				defer func() { panicValues = append(panicValues, recover()) }()
				Host("h", HostConfig{Hostname: "new-name", NumCPU: 7, Clock: Skew(time.Duration(math.MinInt64))}, func() { bodyRan = true })
			}()
		}
		host, proc := dstCurrentNode()
		if host != rootHost || proc != rootProc {
			t.Fatalf("failed Host leaked caller stamp %d/%d, want %d/%d", host, proc, rootHost, rootProc)
		}
		close(probe)
		got := <-observed
		if got.hostname != "old-name" || got.cpus != 3 || got.now.Sub(rootNow) != 50*time.Millisecond {
			t.Fatalf("failed Host changed prior state: hostname=%q cpus=%d offset=%v", got.hostname, got.cpus, got.now.Sub(rootNow))
		}
		hid := lookupHost("h")
		CrashHost("h")
		func() {
			defer func() { panicValues = append(panicValues, recover()) }()
			Host("h", HostConfig{Clock: Skew(time.Duration(math.MinInt64))}, func() { bodyRan = true })
		}()
		if !dstNetHostDead(hid) {
			t.Fatal("failed Host re-declaration rebooted a powered-off host")
		}
	})
	if bodyRan || len(panicValues) != 3 {
		t.Fatalf("invalid Host body ran=%v panics=%d, want false and 3", bodyRan, len(panicValues))
	}
	for i, value := range panicValues {
		msg, _ := value.(string)
		if !strings.Contains(msg, "before the epoch") {
			t.Fatalf("invalid Host panic %d = %v", i+1, value)
		}
	}
}

func TestDSTHostInvalidClockDeclarationPreservesPendingTimer(t *testing.T) {
	var elapsed time.Duration
	Run(1, func() {
		armed := make(chan struct{})
		done := make(chan struct{})
		Host("h", HostConfig{Clock: Drift(1_000_000_000)}, func() {
			go func() {
				close(armed)
				time.Sleep(time.Second)
				close(done)
			}()
		})
		<-armed
		start := time.Now()
		func() {
			defer func() { recover() }()
			Host("h", HostConfig{Clock: Skew(time.Duration(math.MinInt64))}, func() {})
		}()
		<-done
		elapsed = time.Since(start)
	})
	if elapsed != 500*time.Millisecond {
		t.Fatalf("pending timer fired after %v base time, want 500ms", elapsed)
	}
}

func TestDSTExplicitHostIdentityWinsConcurrentImplicitDeclaration(t *testing.T) {
	Run(1, func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			Process("h", func() {})
		}()
		go func() {
			defer wg.Done()
			Host("h", HostConfig{Hostname: "explicit", NumCPU: 3}, func() {})
		}()
		wg.Wait()
		Process("h", func() {})

		oldHost, oldProc := dstCurrentNode()
		dstSetNode(lookupHost("h"), oldProc)
		defer dstSetNode(oldHost, oldProc)
		hostname, _ := os.Hostname()
		if hostname != "explicit" || runtime.NumCPU() != 3 {
			t.Fatalf("identity after concurrent declarations = %q/%d, want explicit/3", hostname, runtime.NumCPU())
		}
	})
}

func TestDSTHostTableExhaustionIsStateNeutral(t *testing.T) {
	var overflow any
	var overflowBody, existingBody bool
	Run(1, func() {
		nodeReg.mu.Lock()
		nodeReg.hosts["h1"] = 1
		nodeReg.nextHost = 4095
		nodeReg.mu.Unlock()
		rootHost, rootProc := dstCurrentNode()
		nodeReg.mu.Lock()
		beforeHosts, beforeNext := len(nodeReg.hosts), nodeReg.nextHost
		nodeReg.mu.Unlock()
		func() {
			defer func() { overflow = recover() }()
			Host("overflow", HostConfig{}, func() { overflowBody = true })
		}()
		nodeReg.mu.Lock()
		_, interned := nodeReg.hosts["overflow"]
		unchanged := len(nodeReg.hosts) == beforeHosts && nodeReg.nextHost == beforeNext
		nodeReg.mu.Unlock()
		host, proc := dstCurrentNode()
		if interned || !unchanged || host != rootHost || proc != rootProc {
			t.Fatalf("host-table exhaustion state: interned=%v unchanged=%v caller=%d/%d want=%d/%d hosts=%d next=%d", interned, unchanged, host, proc, rootHost, rootProc, beforeHosts, beforeNext)
		}
		Host("h1", HostConfig{}, func() { existingBody = true })
	})
	msg, _ := overflow.(string)
	if overflow == nil || !strings.Contains(msg, "too many distinct hosts") || overflowBody || !existingBody {
		t.Fatalf("host-table exhaustion = panic %v overflowBody=%v existingBody=%v", overflow, overflowBody, existingBody)
	}
}

func TestDSTImplicitHostTableExhaustionIsStateNeutral(t *testing.T) {
	var overflow any
	var overflowBody, existingBody bool
	Run(1, func() {
		nodeReg.mu.Lock()
		nodeReg.hosts["h1"] = 1
		nodeReg.nextHost = 4095
		nodeReg.mu.Unlock()
		rootHost, rootProc := dstCurrentNode()
		func() {
			defer func() { overflow = recover() }()
			Process("overflow", func() { overflowBody = true })
		}()
		nodeReg.mu.Lock()
		_, hostInterned := nodeReg.hosts["overflow"]
		_, procInterned := nodeReg.procs["overflow"]
		unchanged := nodeReg.nextHost == 4095
		nodeReg.mu.Unlock()
		host, proc := dstCurrentNode()
		if hostInterned || procInterned || !unchanged || host != rootHost || proc != rootProc {
			t.Fatalf("implicit host exhaustion state: host=%v proc=%v unchanged=%v caller=%d/%d want=%d/%d", hostInterned, procInterned, unchanged, host, proc, rootHost, rootProc)
		}
		Process("h1", func() { existingBody = true })
	})
	msg, _ := overflow.(string)
	if overflow == nil || !strings.Contains(msg, "too many distinct hosts") || overflowBody || !existingBody {
		t.Fatalf("implicit host exhaustion = panic %v overflowBody=%v existingBody=%v", overflow, overflowBody, existingBody)
	}
}
