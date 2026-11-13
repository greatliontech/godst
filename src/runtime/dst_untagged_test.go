// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDSTRunRequiresBuildTag verifies the documented build-constraint panic:
// a binary built WITHOUT -tags dst refuses simulation.Run with the
// reproducible-map-hash-key message.
func TestDSTRunRequiresBuildTag(t *testing.T) {
	exe, err := buildTestProg(t, "testprog") // no -tags dst
	if err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSpace(runBuiltTestProg(t, exe, "DSTRunNoTag"))
	if !strings.Contains(out, "requires building with -tags dst") {
		t.Fatalf("untagged simulation.Run = %q, want the -tags dst build panic", out)
	}
}

// TestDSTFaultAPILinksUntagged pins the tag boundary of the fault API: the
// simulated filesystem lives behind -tags dst, so an untagged binary that calls
// CrashHost (or any fault whose implementation reaches os) must still LINK and
// no-op outside a run.
func TestDSTFaultAPILinksUntagged(t *testing.T) {
	exe, err := buildTestProg(t, "testprog") // no -tags dst
	if err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSpace(runBuiltTestProg(t, exe, "DSTFaultAPINoTag"))
	if out != "fault api no-op" {
		t.Fatalf("untagged fault API = %q, want a clean no-op", out)
	}
}

// TestDSTUntaggedCodeFootprint pins the zero-code-footprint contract
// (design.md, "Untagged footprint"): in a non-`-tags dst` build, the
// dstBuild-guarded legs on the anchored paths below fold out — the compiled
// symbols reference no runtime.dst* symbol.
func TestDSTUntaggedCodeFootprint(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips build-mode objdump checks")
	}
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	src := []byte(`package main

import (
	"os"
	"runtime"
)

type big struct{ buf [64]byte }

func main() {
	println(runtime.NumCPU())
	b := &big{}
	runtime.SetFinalizer(b, func(*big) { println("f") })
	runtime.AddCleanup(new(int), func(int) {}, 0)
	b = nil
	runtime.GC()
	if f, err := os.CreateTemp("", "probe"); err == nil {
		f.Seek(0, 0)
		f.Close()
		os.Remove(f.Name())
	}
	defer func() { recover() }()
	panic("x")
}
`)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), src, 0o666); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "app")
	cmd := testenv.CleanCmdEnv(exec.Command(testenv.GoToolPath(t), "build", "-o", exe, "main.go"))
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, probe := range []struct {
		pattern       string
		mustBePresent bool
		mustBeAbsent  bool
	}{
		{pattern: "runtime.goready$", mustBeAbsent: true},
		{pattern: "runtime.gopanic$", mustBePresent: true},
		{pattern: "runtime.runFinalizers$", mustBePresent: true},
		{pattern: "runtime.NumCPU$"},
		{pattern: "runtime.AddCleanup", mustBePresent: true},
		{pattern: "runtime.GC$", mustBePresent: true},
		{pattern: "runtime.gdestroy$", mustBePresent: true},
		{pattern: "runtime.addfinalizer$", mustBePresent: true},
		{pattern: "runtime.addCleanup$", mustBePresent: true},
		{pattern: "runtime.queuefinalizer$", mustBePresent: true},
		{pattern: "runtime.freeSpecial$", mustBePresent: true},
		{pattern: `runtime\.\(\*cleanupQueue\)\.enqueue$`, mustBePresent: true},
		{pattern: "runtime.runCleanups$", mustBePresent: true},
		{pattern: "runtime.goroutineheader$", mustBePresent: true},
		{pattern: `runtime\.\(\*synctestBubble\)\.changegstatus$`, mustBePresent: true},
		{pattern: "runtime.sysmonUpdateGOMAXPROCS$", mustBePresent: true},
	} {
		cmd = testenv.CleanCmdEnv(exec.Command(testenv.GoToolPath(t), "tool", "objdump", "-s", probe.pattern, exe))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("objdump %s: %v\n%s", probe.pattern, err, out)
		}
		if !strings.Contains(string(out), "TEXT ") {
			if probe.mustBePresent {
				t.Errorf("untagged binary has no symbol matching %s — the anchor drifted; re-anchor the fold test", probe.pattern)
			}
			if probe.mustBeAbsent && runtime.DSTBuild {
				self := testenv.CleanCmdEnv(exec.Command(testenv.GoToolPath(t), "tool", "objdump", "-s", probe.pattern, os.Args[0]))
				if selfOut, err := self.CombinedOutput(); err != nil || !strings.Contains(string(selfOut), "TEXT ") {
					t.Errorf("absence anchor %s names no symbol even in the tagged test binary — pattern typo?", probe.pattern)
				}
			}
			continue
		}
		if probe.mustBeAbsent {
			t.Errorf("untagged binary has a symbol matching %s — stock inlines it everywhere; a hook cost it its inlinability", probe.pattern)
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			text := strings.Join(fields[3:], " ")
			branch := strings.HasPrefix(text, "CALL ") || strings.HasPrefix(text, "JMP ")
			if strings.Contains(text, "runtime.dst") || branch && strings.Contains(text, "dst") {
				t.Errorf("untagged %s references a dst symbol: %s", probe.pattern, strings.TrimSpace(line))
			}
		}
	}
}
