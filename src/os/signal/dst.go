// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package signal

import _ "unsafe" // for go:linkname

const dstSimEnabled = true

// dstFenceActive reports whether the caller is a bubble goroutine of the active
// simulation (see runtime.dstFenceActive). os/signal uses it to fence Notify/
// NotifyContext from a bubble goroutine: a real OS signal is an outside-bubble
// event on a wall clock that no seed controls, so subscribing to one is refused
// with the standard unsupported shape. See design.md "The interception boundary".
//
//go:linkname dstFenceActive runtime.dstFenceActive
func dstFenceActive() bool
