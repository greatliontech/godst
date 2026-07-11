// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"internal/platform"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sizeSpecializedProbeSrc allocates through the exact shapes the
// sizespecializedmalloc experiment lowers to direct size-specialized calls in
// user packages: escaping new of small fixed-size types, one noscan and one
// pointerful, both at most 96 bytes so they stay under
// minSizeForMallocHeader on every architecture (128 on 32-bit platforms; the
// experiment specializes nothing above that bound, and runtime-mediated
// allocations like makeslice never bypass the dispatcher regardless). It
// prints the refusal panic if Run refuses, else two same-seed NumGC deltas
// and whether they are equal and nonzero — so a build where the experiment
// silently bypasses the deterministic trigger prints "gcs 0 0 false" instead
// of either healthy outcome, and a build where the runtime's generated-site
// backstop fires dies with its throw.
const sizeSpecializedProbeSrc = `package main

import (
	"fmt"
	"runtime"
	"testing/simulation"
)

type node struct {
	next *node
	buf  [88]byte
}

var (
	ring  [64]*[96]byte
	nodes [64]*node
	tiny  [64]*int64
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("panic:", r)
		}
	}()
	numGC := func() (d uint32) {
		simulation.Run(12345, func() {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			start := ms.NumGC
			for i := 0; i < 1<<16; i++ {
				// All three specialized families: small noscan, small
				// pointerful, and tiny (< 16 bytes, noscan).
				ring[i%len(ring)] = new([96]byte)
				nodes[i%len(nodes)] = &node{next: nodes[i%len(nodes)]}
				tiny[i%len(tiny)] = new(int64)
			}
			runtime.ReadMemStats(&ms)
			d = ms.NumGC - start
		})
		return
	}
	a, b := numGC(), numGC()
	fmt.Println("gcs", a, b, a == b && a > 0)
}
`

// buildAndRunSizeSpecializedProbe builds the probe against this GOROOT with
// GOEXPERIMENT=sizespecializedmalloc plus the given extra build flags, runs
// it, and returns the run's combined output and error. The build re-derives
// std under the experiment fingerprint — expensive on a cold cache, hence
// -short skips the callers. Both commands run under CleanCmdEnv (an outer
// GODEBUG such as fips140=on would trip an unrelated refusal in the probe),
// and the build clears any outer GOFLAGS (a GOFLAGS=-race would silently
// instrument the "plain" arm).
func buildAndRunSizeSpecializedProbe(t *testing.T, extraBuildFlags ...string) (string, error) {
	t.Helper()
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(sizeSpecializedProbeSrc), 0o666); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "probe")
	args := append([]string{"build", "-tags", "dst", "-o", exe}, extraBuildFlags...)
	args = append(args, "main.go")
	cmd := testenv.CleanCmdEnv(testenv.Command(t, testenv.GoToolPath(t), args...))
	cmd.Dir = dir
	cmd.Env = append(cmd.Env, "GOEXPERIMENT=sizespecializedmalloc", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build under GOEXPERIMENT=sizespecializedmalloc: %v\n%s", err, out)
	}
	out, err := testenv.CleanCmdEnv(testenv.Command(t, exe)).CombinedOutput()
	return string(out), err
}

// TestDSTSizeSpecializedMallocRefused: a plain (uninstrumented) -tags dst
// build under GOEXPERIMENT=sizespecializedmalloc refuses Run loudly — the
// compiler emits direct size-specialized malloc calls in user packages there,
// bypassing the mallocgc dispatcher that is the DST heap trigger's single
// evaluation point. This builds real std under the experiment, so the refusal
// branch is exercised for the build mode that actually has the bypass.
func TestDSTSizeSpecializedMallocRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips GOEXPERIMENT std rebuild")
	}
	out, err := buildAndRunSizeSpecializedProbe(t)
	if err != nil {
		t.Fatalf("probe run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "unsupported with GOEXPERIMENT=sizespecializedmalloc") {
		t.Fatalf("experiment build did not refuse Run (got %q)", out)
	}
}

// TestDSTSizeSpecializedMallocRaceExempt: under -race the compiler suppresses
// specialized emission for every package it instruments (and runtime-group
// packages never receive it), so there is no dispatcher bypass and the
// refusal must NOT fire — and GC discovery must still be deterministic: two
// same-seed runs agree on a nonzero NumGC delta. -msan/-asan share the same
// compiler gate (ssagen sizeSpecializedMallocEnabled keys on Instrumenting)
// but need sanitizer toolchains, so only the race arm is exercised.
func TestDSTSizeSpecializedMallocRaceExempt(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips GOEXPERIMENT std rebuild")
	}
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skipf("no race detector on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testenv.MustHaveCGO(t) // -race needs cgo
	out, err := buildAndRunSizeSpecializedProbe(t, "-race")
	if err != nil {
		t.Fatalf("probe run: %v\n%s", err, out)
	}
	if strings.Contains(out, "panic:") {
		t.Fatalf("instrumented experiment build refused Run (got %q); the exemption regressed", out)
	}
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 4 || f[0] != "gcs" || f[3] != "true" {
		t.Fatalf("instrumented experiment build lost GC determinism (got %q, want equal nonzero same-seed NumGC deltas)", out)
	}
}

// TestDSTSizeSpecializedMallocPerPackageOptOutFailsLoud: the build-level
// exemption assumes build-uniform instrumentation, and a per-package
// instrumentation opt-out voids that assumption — the opted-out package
// compiles uninstrumented, gets specialized emission, and its allocations
// bypass the dispatcher while race.Enabled still reads true, so Run is
// admitted. The runtime's generated-site backstop is what catches this
// configuration: the first specialized allocation during the active run
// throws instead of silently skewing the trigger stream.
func TestDSTSizeSpecializedMallocPerPackageOptOutFailsLoud(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips GOEXPERIMENT std rebuild")
	}
	if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skipf("no race detector on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	testenv.MustHaveCGO(t) // -race needs cgo
	out, err := buildAndRunSizeSpecializedProbe(t, "-race", "-gcflags=command-line-arguments=-race=false")
	if err == nil {
		t.Fatalf("per-package instrumentation opt-out ran to completion (got %q); the generated-site backstop is gone", out)
	}
	if !strings.Contains(out, "size-specialized malloc during a simulation") {
		t.Fatalf("per-package opt-out died without the backstop diagnostic (err %v, output %q)", err, out)
	}
}
