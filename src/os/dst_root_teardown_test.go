// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"testing/simulation"
)

// TestDSTRootTeardownRegistrationOrder pins process-teardown Root close order
// as REGISTRATION order, the same rule dstCloseOpenFiles enforces for files:
// close order is observable in principle, and a pointer-keyed map iteration
// would order victims by run-varying heap addresses — a silent same-seed
// schedule fork the moment Root.Close gains any observable side effect.
// Mutation: dropping the seq sort (or the registration stamp) in
// dstCloseRoots yields map order here.
func TestDSTRootTeardownRegistrationOrder(t *testing.T) {
	const nRoots = 8
	var got []string
	os.DSTSetRootCloseObserver(func(name string) { got = append(got, name) })
	defer os.DSTSetRootCloseObserver(nil)

	var want []string
	simulation.Run(1, func() {
		simulation.Process("victim", func() {
			for i := 0; i < nRoots; i++ {
				dir := fmt.Sprintf("/r%d", i)
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if _, err := os.OpenRoot(dir); err != nil {
					t.Fatal(err)
				}
				want = append(want, dir)
			}
			// The Process body's return is the process's exit: teardown
			// closes its roots — in registration order.
		})
	})
	if len(got) != nRoots {
		t.Fatalf("teardown closed %d roots (%v), want %d", len(got), got, nRoots)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("teardown close order = %v, want registration order %v", got, want)
	}
}
