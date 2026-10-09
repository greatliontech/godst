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
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"slices"
	"strings"
)

func main() {
	tags := flag.String("tags", "", "comma-separated build tags for source resolution")
	source := flag.String("source", "", "synthetic Go source instead of imported packages")
	flag.Parse()
	if *tags != "" {
		build.Default.BuildTags = strings.Split(*tags, ",")
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "source", nil)
	var lines []string
	if *source != "" {
		file, err := parser.ParseFile(fset, "shape.go", *source, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		pkg, err := new(types.Config).Check("shape", fset, []*ast.File{file}, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		lines = append(lines, inventory(pkg)...)
	}
	for _, path := range flag.Args() {
		pkg, err := imp.Import(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "import %s: %v\n", path, err)
			os.Exit(1)
		}
		lines = append(lines, inventory(pkg)...)
	}
	slices.Sort(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
}

func inventory(pkg *types.Package) []string {
	var lines []string
	path := pkg.Path()
	qual := func(p *types.Package) string { return p.Path() }
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		tn, ok := scope.Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		var params *types.TypeParamList
		switch typ := tn.Type().(type) {
		case *types.Named:
			params = typ.TypeParams()
		case *types.Alias:
			params = typ.TypeParams()
			lines = append(lines, fmt.Sprintf("%s %s alias %s", path, name, types.TypeString(typ.Rhs(), qual)))
		}
		for i := 0; i < params.Len(); i++ {
			p := params.At(i)
			lines = append(lines, fmt.Sprintf("%s %s parameter %d %s %s", path, name, i, p.Obj().Name(), types.TypeString(p.Constraint(), qual)))
		}
		if tn.IsAlias() {
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
			lines = append(lines, fmt.Sprintf("%s %s interface %s", path, name, types.TypeString(u, qual)))
			u.Complete()
			for i := 0; i < u.NumMethods(); i++ {
				m := u.Method(i)
				lines = append(lines, fmt.Sprintf("%s %s method %s %s",
					path, name, m.Id(), types.TypeString(m.Type(), qual)))
			}
		default:
			// A named non-struct type's underlying is its shape.
			lines = append(lines, fmt.Sprintf("%s %s underlying %s",
				path, name, types.TypeString(u, qual)))
		}
		// Concrete method sets are shape too: a fork-added method
		// on an upstream-present type — promoted sets included —
		// is analyzer- and reflect-observable.
		for _, form := range []struct {
			name string
			typ  types.Type
		}{{"value", tn.Type()}, {"pointer", types.NewPointer(tn.Type())}} {
			ms := types.NewMethodSet(form.typ)
			for i := 0; i < ms.Len(); i++ {
				m := ms.At(i).Obj()
				lines = append(lines, fmt.Sprintf("%s %s %s-method %s %s",
					path, name, form.name, m.Id(), types.TypeString(m.Type(), qual)))
			}
		}
	}
	slices.Sort(lines)
	return lines
}
