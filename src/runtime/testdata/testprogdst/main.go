// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Command testprogdst hosts deterministic-simulation (testing/simulation) test
// programs that pull in heavy imports — crypto/rand (the whole crypto stack) and
// os/user — kept out of the lean runtime "testprog" binary. Those imports shift
// the heap layout enough to perturb testprog's byte-exact per-cycle GC-discovery
// test (the documented ±1-span finalizer-timing fragility), so the simulation
// tests that need them live in their own binary. Same register/dispatch shape as
// testprog.
package main

import "os"

var cmds = map[string]func(){}

func register(name string, f func()) {
	if cmds[name] != nil {
		panic("duplicate registration: " + name)
	}
	cmds[name] = f
}

func main() {
	if len(os.Args) < 2 {
		println("usage: " + os.Args[0] + " name-of-test")
		return
	}
	f := cmds[os.Args[1]]
	if f == nil {
		println("unknown function: " + os.Args[1])
		return
	}
	f()
}
