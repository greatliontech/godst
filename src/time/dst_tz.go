// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package time

import _ "unsafe" // for go:linkname

// The lazy timezone load (initLocal / LoadLocation) reads host files
// (/etc/localtime, the zoneinfo dir or zip) through this package's own
// mini file-reader — raw syscall.Open, bypassing the os interception.
// From a bubble goroutine that is both a fence panic (raw openat) and,
// worse, a determinism escape: HOST timezone bytes entering the bubble
// make schedules host-dependent. Under an active fence the read
// answers ENOENT instead, so zone resolution takes its documented
// missing-database fallback — Local resolves to UTC, LoadLocation
// errors deterministically — identical on every host.
//
// Residual (fork issue index): the package caches zones process-wide,
// so a zone loaded by HOST code before a run is visible inside later
// bubbles without any file read — a recorded determinism caveat until
// zones are bubble-scoped.

//go:linkname dstTZFenceActive runtime.dstFenceActive
func dstTZFenceActive() bool
