// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build race

package simulation

import "runtime"

// dstRaceErrors returns the process-global count of data races the detector has
// reported so far. Under -race this is runtime.RaceErrors(); the Explore loop reads
// it before/after each scheduled Run and attributes a NEW race to that run's
// schedule (D5: the happens-before detector is the deterministic oracle). The race
// detector dedups by signature, so the delta is nonzero only on the FIRST schedule
// exhibiting each distinct race — which yields exactly one reproducing schedule per
// race.
func dstRaceErrors() int { return runtime.RaceErrors() }
