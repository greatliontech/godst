// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// typeShapePackages is the type-shape gate's package set: the
// user-importable std packages the dst delta modifies, plus the
// internal packages whose types embed into their exported shapes
// (internal/poll.FD sits inside os.file; internal/sync inside sync).
// The runtime is deliberately outside the clause: its internals appear
// in no user signature and are unreachable by reflect, so their
// recorded DATA deviations (design.md) stay in-record.
var typeShapePackages = []string{
	"internal/poll", "internal/sync", "net", "os", "os/exec",
	"os/signal", "os/user", "sync", "syscall", "testing", "time",
}

// typeShapeExceptions records the deliberate per-structure shape
// deviations the type-shape clause admits, each judged in design.md's
// untagged footprint contract. An entry admits exactly one inventory
// line the fork adds over stock; an entry that admits nothing fails
// (the stale-entry discipline of the text gate). Placeholder lines are
// resolved to exact inventory text by running the gate; a mismatch
// fails as BOTH an unadmitted addition and a stale exception.
var typeShapeExceptions = map[string]string{
	// The framework stream's raw descriptor, captured at construction:
	// harness-internal state in a struct no user signature carries,
	// judged in-record (design.md, "Untagged footprint (contract)").
	"testing chattyPrinter field 4 hostFD int embedded=false tag=\"\"": "chatty printer host-stream slot",
	// The chatty printer's simulation-bubble output methods: the same
	// harness-internal judgment as its hostFD slot (design.md).
	"testing chattyPrinter cmethod dstBubbleBenchWrite func(strErrBegin string, indent string, b []byte, strErrEnd string, c []byte) bool": "chatty printer bubble output",
	"testing chattyPrinter cmethod dstBubbleFramework func() bool":                                                                         "chatty printer bubble output (dst-tagged build only)",
	"testing chattyPrinter cmethod dstBubblePrintf func(testName string, format string, args ...any) bool":                                 "chatty printer bubble output",
	"testing chattyPrinter cmethod dstBubbleUpdatef func(testName string, format string, args ...any) bool":                                "chatty printer bubble output",
}

// TestUntaggedTypeShapesIdenticalToStock is the type-shape half of the
// untagged-footprint contract: every type declared in an
// upstream-present, user-importable std package keeps upstream's exact
// field and method-set shape — in BOTH build modes, because analyzers
// and reflect observe declared shapes, not built text — with DST
// per-object state attached out of line (dst_filestate.go,
// dst_root.go). Fork-only type declarations are outside the clause;
// a field added to or removed from an upstream-present type fails
// unless a recorded exception admits it (design.md, "Untagged
// footprint (contract)", the type-shape clause).
func TestUntaggedTypeShapesIdenticalToStock(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: skips the differential resolution")
	}
	stock := os.Getenv("DST_STOCK_GOROOT")
	if stock == "" {
		t.Skip("DST_STOCK_GOROOT not set; run via `task test:inert-diff`, which locates or installs the upstream base toolchain")
	}
	fork := runtime.GOROOT()
	corpus, err := filepath.Abs("testdata/typeshapecorpus")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "typeshapecorpus")
	build := exec.Command(filepath.Join(fork, "bin", "go"), "build", "-o", bin, ".")
	build.Dir = corpus
	build.Env = append(os.Environ(),
		"GOROOT="+fork, "GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOFLAGS=", "GO111MODULE=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build corpus: %v\n%s", err, out)
	}
	inventory := func(goroot, tags string) map[string]bool {
		t.Helper()
		args := []string{}
		if tags != "" {
			args = append(args, "-tags", tags)
		}
		args = append(args, typeShapePackages...)
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "GOROOT="+goroot)
		out, err := cmd.Output()
		if err != nil {
			var stderr []byte
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = ee.Stderr
			}
			t.Fatalf("inventory GOROOT=%s tags=%q: %v\n%s", goroot, tags, err, stderr)
		}
		lines := map[string]bool{}
		for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			lines[l] = true
		}
		return lines
	}
	stockInv := inventory(stock, "")
	// Anti-vacuity: an empty or truncated corpus output would pass
	// every comparison below vacuously.
	anchored := false
	for l := range stockInv {
		if strings.HasPrefix(l, "os file field ") {
			anchored = true
			break
		}
	}
	if len(stockInv) < 500 || !anchored {
		t.Fatalf("stock inventory is implausibly small (%d lines) or missing the os.file anchor — the corpus emitted nothing comparable", len(stockInv))
	}
	// The package list must not drift below the clause: every
	// user-importable upstream-present package the dst delta touches
	// must be in typeShapePackages (the text gate's live-delta
	// discipline; advisory without git, required in CI via
	// DST_REQUIRE_LIVE_DELTA).
	typeShapeCoverageCheck(t, fork)
	// The clause binds both build modes: declared shapes are what
	// analyzers and reflect see, and both modes must show stock's.
	// Staleness is judged over the union of both passes — a
	// tagged-build-only deviation legitimately fires in one.
	stockTypes := map[string]bool{}
	for l := range stockInv {
		stockTypes[typeKey(l)] = true
	}
	fired := map[string]bool{}
	var failures []string
	for _, tags := range []string{"", "dst"} {
		forkInv := inventory(fork, tags)
		for l := range stockInv {
			if !forkInv[l] {
				failures = append(failures, fmt.Sprintf("tags=%q: stock shape line missing from fork: %s", tags, l))
			}
		}
		for l := range forkInv {
			if !stockInv[l] && stockTypes[typeKey(l)] {
				if _, ok := typeShapeExceptions[l]; ok {
					fired[l] = true
					continue
				}
				failures = append(failures, fmt.Sprintf("tags=%q: fork adds to an upstream-present type: %s", tags, l))
			}
		}
	}
	for l := range typeShapeExceptions {
		if !fired[l] {
			failures = append(failures, fmt.Sprintf("stale type-shape exception %q: fired on no line in either build mode — the deviation it admits no longer exists; remove it", l))
		}
	}
	if len(failures) > 0 {
		t.Fatalf("declared type shapes diverge from the upstream base beyond the recorded exceptions:\n  %s", strings.Join(failures, "\n  "))
	}
}

// typeKey is the "pkg TypeName" prefix of one inventory line.
func typeKey(line string) string {
	f := strings.Fields(line)
	if len(f) < 2 {
		return line
	}
	return f[0] + " " + f[1]
}

// typeShapeCoverageCheck asserts typeShapePackages covers every
// user-importable, upstream-present std package the dst delta touches
// (mirroring the text gate's live-delta consultation): a fork edit to a
// twelfth package must not slide under the gate silently.
func typeShapeCoverageCheck(t *testing.T, fork string) {
	t.Helper()
	base, err := os.ReadFile(filepath.Join(fork, "VERSION"))
	if err != nil {
		t.Logf("type-shape coverage: VERSION unreadable (%v); package list not cross-checked", err)
		return
	}
	tag := strings.TrimSpace(strings.Split(string(base), "\n")[0])
	if i := strings.Index(tag, "-dst."); i >= 0 {
		tag = tag[:i]
	}
	git := exec.Command("git", "-C", fork, "diff", "--name-only", tag, "--", "src")
	out, err := git.Output()
	if err != nil {
		if os.Getenv("DST_REQUIRE_LIVE_DELTA") != "" {
			t.Errorf("type-shape coverage: live delta REQUIRED but unavailable (git diff %s: %v)", tag, err)
		} else {
			t.Logf("type-shape coverage: live delta unavailable (git diff %s: %v); package list not cross-checked", tag, err)
		}
		return
	}
	listed := map[string]bool{}
	for _, p := range typeShapePackages {
		listed[p] = true
	}
	for _, f := range strings.Fields(string(out)) {
		if !strings.HasSuffix(f, ".go") || strings.HasSuffix(f, "_test.go") || strings.Contains(f, "/testdata/") {
			continue
		}
		pkg := strings.TrimPrefix(filepath.Dir(f), "src/")
		switch {
		case listed[pkg]:
		case pkg == "runtime" || strings.HasPrefix(pkg, "runtime/"):
			// Outside the clause: runtime internals appear in no user
			// signature and are unreachable by reflect.
		case strings.HasPrefix(pkg, "cmd/"), strings.HasPrefix(pkg, "testing/simulation"):
			// Toolchain internals and fork-only trees.
		case strings.HasPrefix(pkg, "internal/") || strings.Contains(pkg, "/internal/"):
			// Internal packages are outside "user-importable"; the two
			// whose types embed into exported shapes (internal/poll,
			// internal/sync) are listed explicitly above.
		case strings.HasPrefix(pkg, "crypto/internal"):
			// Internal crypto plumbing: outside "user-importable".
		default:
			t.Errorf("type-shape coverage: dst delta touches %s (via %s) but typeShapePackages does not list it — the gate would not see a shape change there", pkg, f)
		}
	}
}
