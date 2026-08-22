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
	buf  [56]byte
}

var (
	ring  [64]*[64]byte
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
				// All three specialized families — small noscan, small
				// pointerful, and tiny (< 16 bytes, noscan) — at sizes
				// below ssagen's specializedMallocMax (80 bytes), above
				// which the compiler emits the generic call regardless.
				ring[i%len(ring)] = new([64]byte)
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

const arenaProbeSrc = `package main

import (
	"arena"
	"fmt"
	"testing/simulation"
)

func attempt() (panicValue any, entered bool) {
	defer func() { panicValue = recover() }()
	simulation.RunWith(12345, simulation.Options{MemoryLimit: 1}, func() {
		entered = true
		a := arena.NewArena()
		defer a.Free()
		for i := 0; i < 32; i++ {
			_ = arena.New[[1 << 20]byte](a)
		}
	})
	return nil, entered
}

func main() {
	for i := 1; i <= 2; i++ {
		panicValue, entered := attempt()
		fmt.Printf("attempt=%d panic=%v entered=%v\n", i, panicValue, entered)
	}
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

func buildAndRunArenaProbe(t *testing.T) (string, error) {
	t.Helper()
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(arenaProbeSrc), 0o666); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "probe")
	cmd := testenv.CleanCmdEnv(testenv.Command(t, testenv.GoToolPath(t), "build", "-tags", "dst", "-o", exe, "main.go"))
	cmd.Dir = dir
	cmd.Env = append(cmd.Env, "GOEXPERIMENT=arenas", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build under GOEXPERIMENT=arenas: %v\n%s", err, out)
	}
	out, err := testenv.CleanCmdEnv(testenv.Command(t, exe)).CombinedOutput()
	return string(out), err
}

func TestDSTArenasRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips GOEXPERIMENT std rebuild")
	}
	out, err := buildAndRunArenaProbe(t)
	if err != nil {
		t.Fatalf("probe run: %v\n%s", err, out)
	}
	const refusal = "testing/simulation: Run is unsupported with GOEXPERIMENT=arenas (arena allocations bypass deterministic heap and process accounting)"
	want := "attempt=1 panic=" + refusal + " entered=false\n" +
		"attempt=2 panic=" + refusal + " entered=false\n"
	if out != want {
		t.Fatalf("arena entry refusal transcript = %q, want %q", out, want)
	}
}

// TestDSTSizeSpecializedMallocAdmitted: under GOEXPERIMENT=sizespecializedmalloc
// (the default since Go 1.27) a -tags dst build is admitted in every build mode
// and GC discovery stays deterministic — two same-seed runs agree on a nonzero
// NumGC delta. Plain: cmd/go passes -d=dstbuild=1 and the compiler suppresses
// the direct size-specialized malloc calls it would otherwise emit in user
// packages, so every allocation still reaches the mallocgc dispatcher, the DST
// heap trigger's single evaluation point. Race: the compiler suppresses
// emission for every package it instruments (runtime-group packages never
// receive it); -msan/-asan share the same gate but need sanitizer toolchains,
// so only the race arm is exercised. Both arms build real std under the
// experiment, so the suppression is exercised for the build modes that would
// otherwise have the bypass. Mutation: with the compiler gate dropped the probe
// dies on the runtime's generated-site backstop; with both dropped it observes
// the bypass — zero same-seed GC deltas.
func TestDSTSizeSpecializedMallocAdmitted(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips GOEXPERIMENT std rebuild")
	}
	for _, arm := range []struct {
		name  string
		flags []string
		race  bool
	}{
		{"plain", nil, false},
		{"race", []string{"-race"}, true},
	} {
		t.Run(arm.name, func(t *testing.T) {
			if arm.race {
				if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
					t.Skipf("no race detector on %s/%s", runtime.GOOS, runtime.GOARCH)
				}
				testenv.MustHaveCGO(t) // -race needs cgo
			}
			out, err := buildAndRunSizeSpecializedProbe(t, arm.flags...)
			if err != nil {
				t.Fatalf("probe run: %v\n%s", err, out)
			}
			if strings.Contains(out, "panic:") {
				t.Fatalf("experiment build refused Run (got %q); the compiler-side suppression regressed", out)
			}
			f := strings.Fields(strings.TrimSpace(out))
			if len(f) != 4 || f[0] != "gcs" || f[3] != "true" {
				t.Fatalf("experiment build lost GC determinism (got %q, want equal nonzero same-seed NumGC deltas)", out)
			}
		})
	}
}

// TestDSTSizeSpecializedMallocPerPackageOptOutFailsLoud: the build-level
// mechanism assumes -d=dstbuild=1 reached every package, and an explicit
// per-package -gcflags override voids that assumption — cmd/go places the
// dst flags first, so the user's later -d=dstbuild=0 wins for that package,
// which then compiles with specialized emission and its allocations bypass
// the dispatcher while Run is admitted. The runtime's generated-site
// backstop is what catches this configuration: the first specialized
// allocation during the active run throws instead of silently skewing the
// trigger stream.
func TestDSTSizeSpecializedMallocPerPackageOptOutFailsLoud(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips GOEXPERIMENT std rebuild")
	}
	out, err := buildAndRunSizeSpecializedProbe(t, "-gcflags=command-line-arguments=-d=dstbuild=0")
	if err == nil {
		t.Fatalf("per-package dst-flag opt-out ran to completion (got %q); the generated-site backstop is gone", out)
	}
	if !strings.Contains(out, "size-specialized malloc during a simulation") {
		t.Fatalf("per-package opt-out died without the backstop diagnostic (err %v, output %q)", err, out)
	}
}
