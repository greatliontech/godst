// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"testing/simulation"
)

func init() {
	register("DSTRunNoTag", DSTRunNoTag)
	register("DSTFaultAPINoTag", DSTFaultAPINoTag)
}

// DSTRunNoTag exercises the documented build-constraint panic: in a binary
// built WITHOUT -tags dst, simulation.Run must refuse to start (the map hash
// key cannot be made deterministic at runtime). Prints the recovered panic.
func DSTRunNoTag() {
	defer func() {
		if v := recover(); v != nil {
			if s, ok := v.(string); ok {
				os.Stdout.WriteString("panic: " + s + "\n")
				return
			}
			os.Stdout.WriteString("panic: non-string\n")
			return
		}
		os.Stdout.WriteString("no panic\n")
	}()
	simulation.Run(1, func() {})
}

// DSTFaultAPINoTag exercises the tag boundary of the fault API in an UNTAGGED
// binary. The simulated filesystem's and runtime's DST symbols exist only
// under -tags dst, so an untagged caller of testing/simulation must still link
// cleanly and outside a run these calls must be no-ops.
func DSTFaultAPINoTag() {
	simulation.CrashHost("nohost")
	simulation.CrashProcess("nohost", "noproc")
	simulation.Partition("a", "b")
	os.Stdout.WriteString("fault api no-op\n")
}
