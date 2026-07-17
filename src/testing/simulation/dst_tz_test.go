// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"testing"
	"time"
)

// The lazy timezone load from a bubble goroutine must neither fence
// (its mini file-reader issues raw openat) nor leak HOST timezone
// bytes into the schedule: under an active fence the read answers a
// deterministic missing-database ENOENT, and the Local LOOKUP answers
// UTC even when host code cached a real zone BEFORE the run — Local
// is UTC inside every bubble regardless of load order, and the host's
// cached zone survives untouched after the run.
func TestDSTTimezoneDeterministicInBubble(t *testing.T) {
	if testing.Short() {
		t.Skip("dst simulation test")
	}
	Run(1, func() {
		name, off := time.Now().Zone()
		if name != "UTC" || off != 0 {
			t.Fatalf("in-bubble zone = (%q, %d), want (UTC, 0): host timezone leaked through the cache", name, off)
		}
		if _, err := time.LoadLocation("America/New_York"); err == nil {
			t.Fatal("LoadLocation found a zone database in-bubble — host file read escaped the fence")
		}
	})
}

// The cache-order leak is pinned in package time_test
// (dst_local_cache_linux_test.go), which can reset the Local cache
// and pre-load a real host zone before the run.
