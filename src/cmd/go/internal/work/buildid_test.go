// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package work

import (
	"internal/buildcfg"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseToolID(t *testing.T) {
	t.Parallel()

	// Stock release: whole trimmed line, no buildID required.
	got, err := parseToolID("go tool compile", "compile", "/pkg/tool/compile", "", "compile version go1.27.4\n")
	if err != nil {
		t.Fatalf("stock release failed: %v", err)
	}
	if got != "compile version go1.27.4" {
		t.Fatalf("stock release: got %q, want %q", got, "compile version go1.27.4")
	}

	// Stock release with GOEXPERIMENT: experiment token preserved.
	got, err = parseToolID("go tool compile", "compile", "/pkg/tool/compile", "", "compile version go1.27.4 X:fieldtrack\n")
	if err != nil {
		t.Fatalf("stock experiment failed: %v", err)
	}
	if got != "compile version go1.27.4 X:fieldtrack" {
		t.Fatalf("stock experiment: got %q, want %q", got, "compile version go1.27.4 X:fieldtrack")
	}

	// Upstream devel: extracts bare contentID.
	got, err = parseToolID("go tool compile", "compile", "/pkg/tool/compile", "", "compile version devel go1.28-abc123 buildID=act1/main1/cnt1\n")
	if err != nil {
		t.Fatalf("devel failed: %v", err)
	}
	if got != "cnt1" {
		t.Fatalf("devel: got %q, want %q", got, "cnt1")
	}

	// -dst. stage-2 vs stage-3 bootstrap: distinct actionIDs with the same
	// contentID must produce identical toolIDs so checkNotStale succeeds.
	id2, err := parseToolID("go tool compile", "compile", "/pkg/tool/compile", "", "compile version go1.27.4-dst.16 buildID=act2/main2/cntSame\n")
	if err != nil {
		t.Fatalf("-dst. stage2 failed: %v", err)
	}
	id3, err := parseToolID("go tool compile", "compile", "/pkg/tool/compile", "", "compile version go1.27.4-dst.16 buildID=act3/main3/cntSame\n")
	if err != nil {
		t.Fatalf("-dst. stage3 failed: %v", err)
	}
	if id2 != "compile version go1.27.4-dst.16 buildID=cntSame" || id2 != id3 {
		t.Fatalf("-dst. bootstrap contentID normalization mismatch: id2=%q id3=%q", id2, id3)
	}

	// -dst. rebuilt compiler at the same VERSION: distinct contentIDs must
	// produce distinct toolIDs so GOCACHE invalidates automatically.
	idRebuilt, err := parseToolID("go tool compile", "compile", "/pkg/tool/compile", "", "compile version go1.27.4-dst.16 buildID=act4/main4/cntNew\n")
	if err != nil {
		t.Fatalf("-dst. rebuilt failed: %v", err)
	}
	if idRebuilt == id2 {
		t.Fatalf("-dst. rebuilt compiler did not change toolID: %q", idRebuilt)
	}

	// -dst. stage-1 bootstrap (empty buildID=) must parse cleanly and differ
	// from stage-2's populated contentID.
	id1, err := parseToolID("go tool compile", "compile", "/pkg/tool/compile", "", "compile version go1.27.4-dst.16 buildID=\n")
	if err != nil {
		t.Fatalf("-dst. stage1 empty buildID failed: %v", err)
	}
	if id1 != "compile version go1.27.4-dst.16 buildID=" || id1 == id2 {
		t.Fatalf("-dst. stage1 empty buildID unexpected: %q", id1)
	}

	// -dst. with GOEXPERIMENT preserves X:... alongside normalized contentID.
	idExp, err := parseToolID("go tool compile", "compile", "/pkg/tool/compile", "", "compile version go1.27.4-dst.16 X:fieldtrack buildID=act2/main2/cntSame\n")
	if err != nil {
		t.Fatalf("-dst. experiment failed: %v", err)
	}
	if idExp != "compile version go1.27.4-dst.16 X:fieldtrack buildID=cntSame" {
		t.Fatalf("-dst. experiment: got %q", idExp)
	}

	// -dst. missing buildID= is rejected just like devel missing buildID=.
	if _, err := parseToolID("go tool compile", "compile", "/pkg/tool/compile", "", "compile version go1.27.4-dst.16\n"); err == nil {
		t.Fatal("-dst. without buildID= unexpectedly succeeded")
	}
}

func TestDSTCompilerVFullHasBuildID(t *testing.T) {
	t.Parallel()
	testenv.MustHaveExec(t)

	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	compilePath := filepath.Join(testenv.GOROOT(t), "pkg", "tool", runtime.GOOS+"_"+runtime.GOARCH, "compile"+exe)
	if _, err := os.Stat(compilePath); err != nil {
		t.Skipf("compiler binary not present at %s: %v", compilePath, err)
	}

	out, err := testenv.Command(t, compilePath, "-V=full").CombinedOutput()
	if err != nil {
		t.Fatalf("%s -V=full failed: %v\n%s", compilePath, err, out)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 {
		t.Fatalf("unexpected -V=full output: %q", string(out))
	}
	if strings.Contains(buildcfg.Version, "-dst.") {
		last := fields[len(fields)-1]
		if !strings.HasPrefix(last, "buildID=") || len(last) <= len("buildID=") {
			t.Fatalf("-dst. compiler -V=full missing populated buildID: %q", string(out))
		}
	}
}
