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
// deterministic missing-database ENOENT, so Local resolves to UTC and
// LoadLocation errors identically on every host. Caveat: process-wide
// zone caching means this pin only bites when no HOST code loaded a
// zone first — true in this binary, recorded in the issue index.
func TestDSTTimezoneDeterministicInBubble(t *testing.T) {
	if testing.Short() {
		t.Skip("dst simulation test")
	}
	Run(1, func() {
		name, off := time.Now().Zone()
		if name != "UTC" || off != 0 {
			t.Fatalf("in-bubble zone = (%q, %d), want (UTC, 0): host timezone leaked", name, off)
		}
		if _, err := time.LoadLocation("America/New_York"); err == nil {
			t.Fatal("LoadLocation found a zone database in-bubble — host file read escaped the fence")
		}
	})
}
