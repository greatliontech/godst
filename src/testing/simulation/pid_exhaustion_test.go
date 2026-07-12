// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"math"
	"os"
	"strings"
	"testing"
)

func TestDSTProcessPIDExhaustionIsStateNeutral(t *testing.T) {
	var panics []any
	var bodyRan bool
	RunWith(1, Options{PID: math.MaxInt32}, func() {
		rootHost, rootProc := dstCurrentNode()
		rootPID, rootHostname := os.Getpid(), pidTestHostname()
		if err := os.WriteFile("/root-state", []byte("intact"), 0o600); err != nil {
			panic(err)
		}
		nodeReg.mu.Lock()
		hosts, procs, nextHost, nextProc := len(nodeReg.hosts), len(nodeReg.procs), nodeReg.nextHost, nodeReg.nextProc
		nodeReg.mu.Unlock()
		for range 2 {
			func() {
				defer func() { panics = append(panics, recover()) }()
				Process("never-published", func() { bodyRan = true })
			}()
		}
		host, proc := dstCurrentNode()
		content, err := os.ReadFile("/root-state")
		if err != nil || string(content) != "intact" || host != rootHost || proc != rootProc || os.Getpid() != rootPID || pidTestHostname() != rootHostname {
			t.Fatalf("PID exhaustion changed root state: host/proc=%d/%d pid=%d hostname=%q file=%q,%v", host, proc, os.Getpid(), pidTestHostname(), content, err)
		}
		nodeReg.mu.Lock()
		unchanged := len(nodeReg.hosts) == hosts && len(nodeReg.procs) == procs && nodeReg.nextHost == nextHost && nodeReg.nextProc == nextProc
		nodeReg.mu.Unlock()
		activeProcs.mu.Lock()
		victimPIDs, victimHosts := len(activeProcs.pids), len(activeProcs.host)
		activeProcs.mu.Unlock()
		if !unchanged || victimPIDs != 0 || victimHosts != 0 {
			t.Fatal("PID exhaustion published host, process, or victim registration")
		}
	})
	if bodyRan || len(panics) != 2 {
		t.Fatalf("PID exhaustion ran body=%v panics=%d, want false and 2", bodyRan, len(panics))
	}
	for i, value := range panics {
		msg, _ := value.(string)
		if !strings.Contains(msg, "pid allocation overflows") {
			t.Fatalf("PID exhaustion panic %d = %v", i+1, value)
		}
	}
}

func pidTestHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	return hostname
}
