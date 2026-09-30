// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types2

import (
	"strings"
	"sync"
	"testing"

	"cmd/compile/internal/syntax"
)

// TestIsCompleteRacesUnpack pins the carried fix for the fromRHS data race
// (godst; the golang/go#79035 family): an instantiated Named writes fromRHS
// in unpack under its mutex, and a checker's cycle detection must read
// the field only once the writer's atomic state bit says the value is
// there, never bare. The race detector is the oracle: run with -race (the
// test:dst-race leg), a bare read reports a write/read race between the
// two goroutines below; the state-guarded read does not.
func TestIsCompleteRacesUnpack(t *testing.T) {
	const src = `package p; type G[P any] struct{ f P }`
	f, err := syntax.Parse(syntax.NewFileBase("p.go"), strings.NewReader(src), nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg := NewPackage("p", "p")
	check := NewChecker(&Config{}, pkg, nil)
	if err := check.Files([]*syntax.File{f}); err != nil {
		t.Fatal(err)
	}
	orig := pkg.Scope().Lookup("G").Type().(*Named)
	inst, err := Instantiate(NewContext(), orig, []Type{Typ[Int]}, false)
	if err != nil {
		t.Fatal(err)
	}
	n := inst.(*Named)
	if n.stateHas(unpacked) {
		t.Fatal("instance already unpacked — the race cannot be exercised")
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); n.unpack() }()
	go func() { defer wg.Done(); check.isComplete(n) }()
	wg.Wait()
}
