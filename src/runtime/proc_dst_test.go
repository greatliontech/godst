// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package runtime_test

import (
	"runtime"
	"testing"
	"time"
)

func TestUpdateMaxProcsHelperSurvivesCustomBail(t *testing.T) {
	if !runtime.UpdateMaxProcsGStarted() {
		t.Skip("GOMAXPROCS auto-update helper disabled (GODEBUG=updatemaxprocs=0)")
	}
	wasCustom := runtime.CustomGOMAXPROCS()
	old := runtime.GOMAXPROCS(0)
	// Force manual mode without changing the count, so a pushed update observes
	// customGOMAXPROCS under STW — the bail path.
	runtime.GOMAXPROCS(old)
	defer func() {
		if wasCustom {
			runtime.GOMAXPROCS(old)
		} else {
			runtime.SetDefaultGOMAXPROCS()
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	waitIdle := func(what string) {
		for !runtime.UpdateMaxProcsGIdle() {
			if time.Now().After(deadline) {
				t.Fatalf("GOMAXPROCS auto-update helper did not re-park (%s)", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	push := func(what string) {
		// WakeUpdateMaxProcsG fails only when the helper is not idle (e.g. a
		// real sysmon push raced in); wait and retry.
		for !runtime.WakeUpdateMaxProcsG() {
			waitIdle(what)
		}
	}
	waitIdle("initial")

	// Push an update the helper will observe under STW with the custom flag
	// set: it must drop the update and re-park, not exit. An exited helper
	// leaves idle false forever, permanently disabling automatic updates even
	// after SetDefaultGOMAXPROCS restores automatic mode.
	push("before custom-mode bail")
	waitIdle("after custom-mode bail")

	// The helper must still service updates after automatic mode is restored.
	runtime.SetDefaultGOMAXPROCS()
	push("before post-restore update")
	waitIdle("after post-restore update")
}
