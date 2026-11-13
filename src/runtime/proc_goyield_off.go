// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package runtime

import _ "unsafe"

// goyield is like Gosched, but it does not emit a GoSched trace event nor does it
// put the current goroutine on the global runqueue.
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname goyield
func goyield() {
	checkTimeouts()
	mcall(goyield_m)
}
