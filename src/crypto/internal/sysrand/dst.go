// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package sysrand

import _ "unsafe" // for go:linkname

const dstReadRandomEnabled = true

// dstReadRandom is the runtime's deterministic-simulation entropy source. Under
// deterministic simulation testing (testing/simulation), it fills b from the
// run's per-goroutine deterministic RNG and reports true, so crypto/rand becomes
// a reproducible function of the seed; outside a run it returns false and Read
// falls through to the operating system. See runtime.dstReadRandom.
//
// go:noescape is sound: the runtime writes into b but never retains it.
//
//go:noescape
//go:linkname dstReadRandom runtime.dstReadRandom
func dstReadRandom(b []byte) bool
