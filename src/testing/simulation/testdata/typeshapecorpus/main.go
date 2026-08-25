// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Typeshapecorpus prints a deterministic type-shape inventory of the
// standard packages named on the command line, resolved from the
// RUNNING toolchain's GOROOT source under the build tags given by
// -tags: one line per struct field ("pkg TypeName field fieldType") and
// one per interface method ("pkg TypeName method name signature"),
// sorted. The type-shape parity gate builds and runs this program under
// the fork and the upstream base toolchain and compares the inventories
// (design.md, "Untagged footprint (contract)", the type-shape clause).
package main

import (
	"flag"
	"fmt"
	"go/build"
	"go/importer"
	"go/token"
	"go/types"
	"os"
	"slices"
	"strings"
)

func main() {
	tags := flag.String("tags", "", "comma-separated build tags for source resolution")
	flag.Parse()
	if *tags != "" {
		build.Default.BuildTags = strings.Split(*tags, ",")
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "source", nil)
	var lines []string
	qual := func(p *types.Package) string { return p.Path() }
	for _, path := range flag.Args() {
		pkg, err := imp.Import(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "import %s: %v\n", path, err)
			os.Exit(1)
		}
		scope := pkg.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			switch u := tn.Type().Underlying().(type) {
			case *types.Struct:
				// Field ORDER, embeddedness, and tags are shape:
				// analyzers walk field lists positionally and reflect
				// exposes all three.
				for i := 0; i < u.NumFields(); i++ {
					f := u.Field(i)
					lines = append(lines, fmt.Sprintf("%s %s field %d %s %s embedded=%v tag=%q",
						path, name, i, f.Name(), types.TypeString(f.Type(), qual), f.Anonymous(), u.Tag(i)))
				}
				if u.NumFields() == 0 {
					lines = append(lines, fmt.Sprintf("%s %s struct empty", path, name))
				}
			case *types.Interface:
				for i := 0; i < u.NumExplicitMethods(); i++ {
					m := u.ExplicitMethod(i)
					lines = append(lines, fmt.Sprintf("%s %s method %s %s",
						path, name, m.Name(), types.TypeString(m.Type(), qual)))
				}
			default:
				// A named non-struct type's underlying is its shape.
				lines = append(lines, fmt.Sprintf("%s %s underlying %s",
					path, name, types.TypeString(u, qual)))
			}
			// Concrete method sets are shape too: a fork-added method
			// on an upstream-present type — promoted sets included —
			// is analyzer- and reflect-observable.
			ms := types.NewMethodSet(types.NewPointer(tn.Type()))
			for i := 0; i < ms.Len(); i++ {
				m := ms.At(i).Obj()
				lines = append(lines, fmt.Sprintf("%s %s cmethod %s %s",
					path, name, m.Name(), types.TypeString(m.Type(), qual)))
			}
		}
	}
	slices.Sort(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
}
