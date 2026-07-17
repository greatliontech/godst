// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"os"
	"runtime"
	"strconv"
	"testing"
)

func hostname() string { h, _ := os.Hostname(); return h }

// TestDSTIdentityPerHostHostname verifies os.Hostname is per-host: an unconfigured
// host reports its declared name, HostConfig.Hostname overrides, the root (no Host)
// reports the run default, an implicit host (a bare Process) reports the process
// name, and co-located processes / child goroutines inherit the host's hostname.
func TestDSTIdentityPerHostHostname(t *testing.T) {
	var root, n1, n2, coloc, child, implicit, empty string
	Run(1, func() {
		root = hostname()
		Host("node1", HostConfig{}, func() {
			n1 = hostname()
			Process("p", func() { coloc = hostname() }) // co-located: inherits node1
			done := make(chan struct{})
			go func() { child = hostname(); close(done) }()
			<-done
		})
		Host("node2", HostConfig{Hostname: "db-primary"}, func() { n2 = hostname() })
		Process("n3", func() { implicit = hostname() }) // implicit host named after the process
		// A declared host reports its recorded hostname, even empty — "declared" (the
		// set bit) is the single source of truth, not a non-empty fallback to root.
		Host("", HostConfig{}, func() { empty = hostname() })
	})

	for _, tc := range []struct{ got, want, what string }{
		{root, "sim", "root (no Host) hostname"},
		{n1, "node1", "unconfigured host reports its name"},
		{n2, "db-primary", "HostConfig.Hostname override"},
		{coloc, "node1", "co-located process inherits host hostname"},
		{child, "node1", "child goroutine inherits host hostname"},
		{implicit, "n3", "implicit host reports the process name"},
		{empty, "", "declared empty-name host reports empty, not the root default"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.what, tc.got, tc.want)
		}
	}
}

// TestDSTIdentityPerHostNumCPU verifies runtime.NumCPU is per-host: HostConfig.NumCPU
// overrides, an unconfigured host and the root use the run default (8), and the
// override is inherited by the host's subtree.
func TestDSTIdentityPerHostNumCPU(t *testing.T) {
	var root, cfg, dflt, child int
	Run(1, func() {
		root = runtime.NumCPU()
		Host("big", HostConfig{NumCPU: 4}, func() {
			cfg = runtime.NumCPU()
			done := make(chan struct{})
			go func() { child = runtime.NumCPU(); close(done) }()
			<-done
		})
		Host("plain", HostConfig{}, func() { dflt = runtime.NumCPU() })
	})
	if root != 8 {
		t.Errorf("root NumCPU = %d, want 8 (default)", root)
	}
	if cfg != 4 {
		t.Errorf("HostConfig{NumCPU:4} NumCPU = %d, want 4", cfg)
	}
	if child != 4 {
		t.Errorf("child goroutine NumCPU = %d, want 4 (inherited)", child)
	}
	if dflt != 8 {
		t.Errorf("unconfigured host NumCPU = %d, want 8 (default)", dflt)
	}
}

// TestDSTIdentityPerProcessPid verifies os.Getpid is per-process: the root reports
// the default pid, each Process gets a distinct pid, a process's subtree shares its
// pid, and a restart (re-declaring the same name) gets a *new* pid.
func TestDSTIdentityPerProcessPid(t *testing.T) {
	var root, pA, pAchild, pB, pArestart int
	Run(1, func() {
		root = os.Getpid()
		Process("a", func() {
			pA = os.Getpid()
			done := make(chan struct{})
			go func() { pAchild = os.Getpid(); close(done) }()
			<-done
		})
		Process("b", func() { pB = os.Getpid() })
		Process("a", func() { pArestart = os.Getpid() }) // restart: same name, new pid
	})
	if root != 1 {
		t.Errorf("root pid = %d, want 1 (default)", root)
	}
	if pA == root || pB == root {
		t.Errorf("process pids %d/%d collide with root %d", pA, pB, root)
	}
	if pA == pB {
		t.Errorf("distinct processes share pid %d", pA)
	}
	if pAchild != pA {
		t.Errorf("child pid = %d, want %d (subtree shares the process pid)", pAchild, pA)
	}
	if pArestart == pA {
		t.Errorf("restart of process \"a\" kept pid %d, want a new one", pA)
	}
}

// TestDSTIdentitySound enforces DST-IDENTITY-SOUND: under a run the simulated
// identity replaces the real machine's, so the readings are the deterministic
// simulated values, not the developer's box (whose real hostname is almost never
// "node-x" and whose real NumCPU/pid would vary per machine and run).
func TestDSTIdentitySound(t *testing.T) {
	realHost, _ := os.Hostname()
	realCPU := runtime.NumCPU()
	var simHost string
	var simCPU, simPid int
	Run(1, func() {
		Host("node-x", HostConfig{NumCPU: 3}, func() {
			Process("svc", func() {
				simHost = hostname()
				simCPU = runtime.NumCPU()
				simPid = os.Getpid()
			})
		})
	})
	if simHost != "node-x" {
		t.Errorf("simulated hostname = %q, want %q (real machine identity leaked: %q)", simHost, "node-x", realHost)
	}
	if simCPU != 3 {
		t.Errorf("simulated NumCPU = %d, want 3 (real = %d)", simCPU, realCPU)
	}
	if simPid <= 0 {
		t.Errorf("simulated pid = %d, want a positive simulated value", simPid)
	}
}

// TestDSTIdentityDeterminism enforces DST-IDENTITY-DET: same seed + same config →
// identical per-host/per-process identity readings across two runs.
func TestDSTIdentityDeterminism(t *testing.T) {
	type sample struct {
		hostnames []string
		cpus      []int
		pids      []int
	}
	run := func() sample {
		var s sample
		Run(99, func() {
			Host("alpha", HostConfig{NumCPU: 2}, func() {
				Process("p1", func() {
					s.hostnames = append(s.hostnames, hostname())
					s.cpus = append(s.cpus, runtime.NumCPU())
					s.pids = append(s.pids, os.Getpid())
				})
				Process("p2", func() { s.pids = append(s.pids, os.Getpid()) })
			})
			Host("beta", HostConfig{Hostname: "b-host"}, func() {
				s.hostnames = append(s.hostnames, hostname())
				s.cpus = append(s.cpus, runtime.NumCPU())
			})
		})
		return s
	}
	a, b := run(), run()
	if !slicesEqualStr(a.hostnames, b.hostnames) || !slicesEqualInt(a.cpus, b.cpus) || !slicesEqualInt(a.pids, b.pids) {
		t.Errorf("identity readings not reproducible across same-seed runs:\n a=%+v\n b=%+v", a, b)
	}
}

// TestDSTIdentityN1Collapse pins the N=1 collapse: a program that declares no
// Host/Process reports the run defaults (hostname "sim", pid 1, NumCPU 8) — the
// pre-distributed behavior, unchanged.
func TestDSTIdentityN1Collapse(t *testing.T) {
	var h string
	var pid, cpu int
	Run(1, func() {
		h = hostname()
		pid = os.Getpid()
		cpu = runtime.NumCPU()
	})
	if h != "sim" || pid != 1 || cpu != 8 {
		t.Errorf("N=1 identity = (%q, pid %d, %d CPU), want (\"sim\", 1, 8)", h, pid, cpu)
	}
}

func slicesEqualStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func slicesEqualInt(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDSTIdentityNegativePIDDefaults: a negative Options.PID falls back to the default
// (mirroring NumCPU's <=0 rule) — os.Getpid() must never report a pid no real process
// can observe.
func TestDSTIdentityNegativePIDDefaults(t *testing.T) {
	var pid int
	RunWith(1, Options{PID: -5}, func() { pid = os.Getpid() })
	if pid != 1 {
		t.Errorf("Options{PID: -5}: os.Getpid() = %d, want the default 1 (negative pids are not real identities)", pid)
	}
}

func TestDSTIdentityOversizedPIDPanics(t *testing.T) {
	if strconv.IntSize == 32 {
		t.Skip("int cannot hold a pid larger than the runtime pid field")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("RunWith with oversized Options.PID did not panic")
		}
	}()
	pid := int64(maxPID)
	pid++
	RunWith(1, Options{PID: int(pid)}, func() {})
}

func TestDSTIdentityPIDAllocatorOverflowPanics(t *testing.T) {
	dstSetSimEnv(defaultHostname, maxPID, defaultNumCPU, 0)
	defer dstClearSimEnv()
	defer func() {
		if recover() == nil {
			t.Fatal("pid allocation after max Options.PID did not panic")
		}
	}()
	_ = dstAllocPid()
}
