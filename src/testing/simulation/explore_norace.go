// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !race

package simulation

// dstRaceErrors returns 0 in a non-race build: the data-race oracle (D5) is only
// available under -race. Explore still enumerates interleavings and reports SUT
// assertion failures; it just records no data-race failures. (runtime.RaceErrors
// exists only in race builds, so it cannot be referenced here.)
func dstRaceErrors() int { return 0 }
