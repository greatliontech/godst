// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package runtime

import _ "unsafe"

// goyield is like Gosched, but it does not emit a GoSched trace event nor does it
// put the current goroutine on the global runqueue.
//
// Under -tags dst, goyield is //go:nosplit because the dst-race compiler emits
// hook calls at nosplit positions that funnel through dstYieldPoint -> goyield.
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname goyield
//go:nosplit
func goyield() {
	checkTimeouts()
	mcall(goyield_m)
}
