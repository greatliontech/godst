// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareDSTAPI(t *testing.T) {
	if *flagCheck {
		t.Skip("skipping with -check set")
	}

	tests := []struct {
		name        string
		pairs       []dstFeaturePair
		dstRequired []string
		exception   []string
		wantOK      bool
		wantOut     string
	}{
		{
			name: "recorded dst-only export and matching twin pass",
			pairs: []dstFeaturePair{
				{
					ctxName:  "linux-amd64-dst",
					untagged: []string{"pkg testing/simulation, func HostIP(string) string"},
					tagged: []string{
						"pkg testing/simulation, func HostFS(string) fs.FS",
						"pkg testing/simulation, func HostIP(string) string",
					},
				},
				{
					ctxName:  "linux-arm64-dst",
					untagged: []string{"pkg testing/simulation, func HostIP(string) string"},
					tagged: []string{
						"pkg testing/simulation, func HostFS(string) fs.FS",
						"pkg testing/simulation, func HostIP(string) string",
					},
				},
			},
			dstRequired: []string{"pkg testing/simulation (dst), func HostFS(string) fs.FS"},
			wantOK:      true,
			wantOut:     "",
		},
		{
			name: "unlisted dst-only addition fails",
			pairs: []dstFeaturePair{
				{
					ctxName:  "linux-amd64-dst",
					untagged: []string{"pkg testing/simulation, func Run(uint64, func())"},
					tagged: []string{
						"pkg testing/simulation, func NewDSTOnly()",
						"pkg testing/simulation, func Run(uint64, func())",
					},
				},
			},
			wantOK:  false,
			wantOut: "+pkg testing/simulation (dst), func NewDSTOnly()\n",
		},
		{
			name: "removed dst-only export fails",
			pairs: []dstFeaturePair{
				{
					ctxName:  "linux-amd64-dst",
					untagged: []string{"pkg testing/simulation, func Run(uint64, func())"},
					tagged:   []string{"pkg testing/simulation, func Run(uint64, func())"},
				},
			},
			dstRequired: []string{"pkg testing/simulation (dst), func HostFS(string) fs.FS"},
			wantOK:      false,
			wantOut:     "-pkg testing/simulation (dst), func HostFS(string) fs.FS\n",
		},
		{
			name: "tagged twin signature drift fails on both missing old and unlisted new",
			pairs: []dstFeaturePair{
				{
					ctxName:  "linux-amd64-dst",
					untagged: []string{"pkg testing/simulation, func HostIP(string) string"},
					tagged:   []string{"pkg testing/simulation, func HostIP(string) (string, error)"},
				},
			},
			wantOK: false,
			wantOut: "-pkg testing/simulation (dst), func HostIP(string) string\n" +
				"+pkg testing/simulation (dst), func HostIP(string) (string, error)\n",
		},
		{
			name: "arch-partial dst export fails universal (dst) requirement",
			pairs: []dstFeaturePair{
				{
					ctxName:  "linux-amd64-dst",
					untagged: []string{"pkg testing/simulation, func Run(uint64, func())"},
					tagged: []string{
						"pkg testing/simulation, func HostFS(string) fs.FS",
						"pkg testing/simulation, func Run(uint64, func())",
					},
				},
				{
					ctxName:  "linux-arm64-dst",
					untagged: []string{"pkg testing/simulation, func Run(uint64, func())"},
					tagged:   []string{"pkg testing/simulation, func Run(uint64, func())"},
				},
			},
			dstRequired: []string{"pkg testing/simulation (dst), func HostFS(string) fs.FS"},
			wantOK:      false,
			wantOut: "-pkg testing/simulation (dst), func HostFS(string) fs.FS\n" +
				"+pkg testing/simulation (linux-amd64-dst), func HostFS(string) fs.FS\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			gotOK := compareDSTAPI(&buf, tt.pairs, tt.dstRequired, tt.exception)
			if gotOK != tt.wantOK {
				t.Errorf("compareDSTAPI ok = %v, want %v", gotOK, tt.wantOK)
			}
			if got := buf.String(); got != tt.wantOut {
				t.Errorf("compareDSTAPI output:\ngot:\n%s\nwant:\n%s", got, tt.wantOut)
			}
		})
	}
}

// TestDSTWalkerCacheIsolation pins tagKey's BuildTags inclusion: an untagged
// outer package importing a dst-tagged inner package must not reuse the cached
// untagged type-check when walked under BuildTags: ["dst"].
func TestDSTWalkerCacheIsolation(t *testing.T) {
	if *flagCheck {
		t.Skip("skipping with -check set")
	}
	root := t.TempDir()
	mustWrite := func(rel, src string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("inner/off.go", "//go:build !dst\n\npackage inner\n\ntype V int\n")
	mustWrite("inner/on.go", "//go:build dst\n\npackage inner\n\ntype V string\n")
	// outer has NO build tags in its own files, so pkgTags[outer] does not
	// mention "dst"; its exported signature still depends on inner.V.
	mustWrite("outer/outer.go", "package outer\n\nimport \"inner\"\n\nfunc F() inner.V { var z inner.V; return z }\n")

	base := build.Context{GOOS: "linux", GOARCH: "amd64", Compiler: build.Default.Compiler}
	dst := base
	dst.BuildTags = []string{"dst"}

	newTestWalker := func(c build.Context) *Walker {
		return &Walker{
			context:  &c,
			root:     root,
			features: map[string]bool{},
			imported: map[string]*apiPackage{},
		}
	}

	wu := newTestWalker(base)
	pkgu, err := wu.import_("outer")
	if err != nil {
		t.Fatalf("untagged import outer: %v", err)
	}
	wu.export(pkgu)

	wd := newTestWalker(dst)
	pkgd, err := wd.import_("outer")
	if err != nil {
		t.Fatalf("dst import outer: %v", err)
	}
	wd.export(pkgd)

	// Verify inner's underlying type in outer.F's return signature was
	// freshly type-checked against dst inner (string, not int).
	fn := pkgd.Scope().Lookup("F")
	if got := fn.Type().Underlying().String(); !strings.Contains(got, "inner.V") {
		t.Fatalf("unexpected outer.F type: %s", got)
	}
	innerV := pkgd.Imports()[0].Scope().Lookup("V")
	if got := innerV.Type().Underlying().String(); got != "string" {
		t.Fatalf("dst outer resolved inner.V underlying = %q, want \"string\" (cached untagged outer leaked)", got)
	}
}
