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
// The cache side is closed at the LOOKUP: (*Location).get answers
// &utcLoc for the Local sentinel — the &localLoc cache AND a
// *Location the host ASSIGNED to the mutable time.Local var — while
// the fence is active, so neither load order nor reassignment leaks a
// host zone into a bubble. Host code keeps its zone untouched (the
// cache is never mutated). Boundary: a host-loaded *Location VALUE
// handed into the bubble (captured in the closure, not via
// time.Local) is irreducible data flow no cache policing can catch —
// program discipline, like pointer-carried host state generally —
// as is reassigning time.Local from a HOST goroutine mid-run (the
// sentinel reads the var per lookup; a mid-run swap is a data race
// on a documented-mutable global in any Go program).

// dstTZBuild is the bare-constant guard for the zoneinfo hooks (see
// dst_tz_off.go for the stock-build false).
const dstTZBuild = true

//go:linkname dstTZFenceActive runtime.dstFenceActive
func dstTZFenceActive() bool
