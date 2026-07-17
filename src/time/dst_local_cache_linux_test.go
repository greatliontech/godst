// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package time_test

import (
	"os"
	"testing"
	"testing/simulation"
	"time"
)

// A zone the HOST cached BEFORE a simulation run must not leak into a
// bubble: (*Location).get answers UTC for the Local sentinel while
// the fence is active, load order be damned — and the host's cached
// zone survives the run untouched. This is the cache-order
// counterpart of the load-time ENOENT fence.
func TestDSTLocalCacheDoesNotLeakIntoBubble(t *testing.T) {
	if testing.Short() {
		t.Skip("dst simulation test")
	}
	t.Setenv("TZ", "America/New_York")
	time.ResetLocalOnceForTest()
	// The time test binary forces US/Pacific as Local at startup; put
	// that world back for the tests that run after this one.
	defer time.ForceUSPacificForTesting()
	if _, err := os.Stat("/usr/share/zoneinfo/America/New_York"); err != nil {
		t.Skip("no host zoneinfo database")
	}
	hostName, _ := time.Now().Zone() // loads and caches the REAL zone on the host side
	if hostName == "UTC" {
		t.Fatalf("host cache did not load the test zone (got UTC) — the leak pin would be vacuous")
	}
	simulation.Run(1, func() {
		if name, off := time.Now().Zone(); name != "UTC" || off != 0 {
			t.Fatalf("in-bubble zone = (%q, %d), want (UTC, 0): the host's cached zone leaked", name, off)
		}
	})
	// The REASSIGNED-var path: host sets the mutable time.Local to a
	// loaded zone; the bubble must still see UTC, and the host's
	// assignment must survive the run untouched.
	loaded, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("host zone database unreadable")
	}
	prev := time.Local
	time.Local = loaded
	defer func() { time.Local = prev }()
	simulation.Run(2, func() {
		if name, off := time.Now().Zone(); name != "UTC" || off != 0 {
			t.Fatalf("in-bubble zone with reassigned Local = (%q, %d), want (UTC, 0)", name, off)
		}
	})
	if time.Local != loaded {
		t.Fatal("the run mutated the host's reassigned time.Local")
	}
	if name, _ := time.Now().Zone(); name != hostName {
		t.Fatalf("host zone changed across the run: %q -> %q (the bubble answer must not mutate the cache)", hostName, name)
	}
}
