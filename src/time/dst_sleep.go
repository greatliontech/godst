// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package time

import "unsafe"

//go:linkname newBaseTimer
func newBaseTimer(when, period int64, f func(any, uintptr, int64), arg any, cp unsafe.Pointer) *Timer

// newDSTBaseTimer creates an internal timer whose duration is universe base
// time rather than time perceived by the calling DST host.
//
//go:linkname newDSTBaseTimer
func newDSTBaseTimer(d Duration) *Timer {
	c := make(chan Time, 1)
	t := newBaseTimer(when(d), 0, sendTime, c, syncTimer(c))
	t.C = c
	return t
}
