// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os_test

import (
	"os"
	"testing"
	"testing/simulation"
)

// TestDSTNodeFSIsolation verifies the L2 per-host filesystem (DST-NODE-ISOLATION):
// different hosts have independent trees (no cross-host read/write leak), co-located
// processes share their host's tree, and the working directory is per-process even
// on a shared host.
func TestDSTNodeFSIsolation(t *testing.T) {
	var (
		gotA, gotB   string
		aSeesOnlyB   bool
		shared       string
		p1cwd, p2cwd string
	)
	simulation.Run(1, func() {
		// Cross-host isolation: two hosts write the SAME path independently.
		simulation.Host("hA", func() {
			os.WriteFile("/data", []byte("A"), 0o644)
		})
		simulation.Host("hB", func() {
			os.WriteFile("/data", []byte("B"), 0o644)
			os.WriteFile("/onlyB", []byte("x"), 0o644)
		})
		simulation.Host("hA", func() { // re-enter host hA: same tree
			b, _ := os.ReadFile("/data")
			gotA = string(b)
			_, err := os.Stat("/onlyB") // host hA never created this
			aSeesOnlyB = err == nil
		})
		simulation.Host("hB", func() {
			b, _ := os.ReadFile("/data")
			gotB = string(b)
		})

		// Co-located processes share their host tree; cwd is per-process.
		simulation.Host("h1", func() {
			simulation.Process("p1", func() {
				os.WriteFile("/shared", []byte("hello"), 0o644)
				os.Mkdir("/p1dir", 0o755)
				os.Chdir("/p1dir")
				p1cwd, _ = os.Getwd()
			})
			simulation.Process("p2", func() {
				b, _ := os.ReadFile("/shared") // written by co-located p1
				shared = string(b)
				p2cwd, _ = os.Getwd() // must NOT see p1's Chdir
			})
		})
	})

	if gotA != "A" {
		t.Errorf("host hA reads /data = %q, want %q (cross-host write leak)", gotA, "A")
	}
	if gotB != "B" {
		t.Errorf("host hB reads /data = %q, want %q", gotB, "B")
	}
	if aSeesOnlyB {
		t.Errorf("host hA sees /onlyB, created only on host hB (cross-host read leak)")
	}
	if shared != "hello" {
		t.Errorf("co-located p2 reads /shared = %q, want %q (host tree not shared)", shared, "hello")
	}
	if p1cwd != "/p1dir" {
		t.Errorf("p1 cwd = %q, want /p1dir", p1cwd)
	}
	if p2cwd != "/" {
		t.Errorf("p2 cwd = %q, want / (per-process cwd leaked from p1)", p2cwd)
	}
}

// TestDSTNodeCwdIsolation pins the cross-host working-directory isolation the
// same-host case above does not: a process id is not unique across hosts (a bare
// Host runs at process id 0 on every host; a same-named Process on two hosts shares
// a process id), so cwd must be keyed by the (host, process) pair — and per-host
// /tmp must be independent.
func TestDSTNodeCwdIsolation(t *testing.T) {
	var (
		hBcwd     string
		hBStatErr error
		hBTmpErr  error
		srvCwd    string
	)
	simulation.Run(1, func() {
		// Bare hosts (both at process 0): hA's Chdir must not move hB's cwd.
		simulation.Host("hA", func() {
			os.Mkdir("/onlyA", 0o755)
			os.Chdir("/onlyA")
			os.WriteFile("/tmp/fromA", []byte("a"), 0o644)
		})
		simulation.Host("hB", func() {
			hBcwd, _ = os.Getwd()
			_, hBStatErr = os.Stat(".")        // "." must resolve to hB's own root
			_, hBTmpErr = os.Stat("/tmp/fromA") // per-host /tmp: hA's file absent on hB
		})
		// Same-named process on two hosts: different machines, independent cwd.
		simulation.Host("h2", func() {
			simulation.Process("srv", func() { os.Mkdir("/d", 0o755); os.Chdir("/d") })
		})
		simulation.Host("h3", func() {
			simulation.Process("srv", func() { srvCwd, _ = os.Getwd() })
		})
	})

	if hBcwd != "/" {
		t.Errorf("bare host hB cwd = %q, want / (cross-host cwd leak from hA)", hBcwd)
	}
	if hBStatErr != nil {
		t.Errorf(`host hB Stat(".") = %v, want nil (cwd should resolve to hB's own root)`, hBStatErr)
	}
	if hBTmpErr == nil {
		t.Errorf("host hB sees /tmp/fromA written on hA (per-host /tmp breach)")
	}
	if srvCwd != "/" {
		t.Errorf(`process "srv" on host h3 cwd = %q, want / (shared proc-id cwd leak from h2)`, srvCwd)
	}
}
