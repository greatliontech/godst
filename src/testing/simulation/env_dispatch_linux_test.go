// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"os"
	"testing"
)

func TestDSTEnvironmentDispatchSerializesRunEdges(t *testing.T) {
	t.Setenv("DST_ENV_EDGE", "host-before")
	activateReached := make(chan struct{})
	deactivateReached := make(chan struct{})
	dstEnvRunEdgeHook = func(activating bool) {
		if activating {
			close(activateReached)
		}
	}
	defer func() { dstEnvRunEdgeHook = nil }()
	dstEnvDispatchLock()
	started := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		Run(1, func() { started <- os.Getenv("DST_ENV_EDGE") })
		close(done)
	}()
	<-activateReached
	select {
	case <-started:
		dstEnvDispatchUnlock()
		t.Fatal("run activation crossed environment dispatch lock")
	default:
	}
	dstEnvDispatchUnlock()
	if got := <-started; got != "host-before" {
		t.Fatalf("run-entry environment = %q, want host-before", got)
	}
	<-done
	dstEnvRunEdgeHook = func(activating bool) {
		if !activating {
			close(deactivateReached)
		}
	}

	inside := make(chan struct{})
	release := make(chan struct{})
	done = make(chan struct{})
	go func() {
		Run(2, func() {
			close(inside)
			<-release
		})
		close(done)
	}()
	<-inside
	dstEnvDispatchLock()
	close(release)
	<-deactivateReached
	select {
	case <-done:
		dstEnvDispatchUnlock()
		t.Fatal("run deactivation crossed environment dispatch lock")
	default:
	}
	dstEnvDispatchUnlock()
	<-done
	if err := os.Setenv("DST_ENV_EDGE", "host-after"); err != nil || os.Getenv("DST_ENV_EDGE") != "host-after" {
		t.Fatalf("post-run host environment = %q, %v", os.Getenv("DST_ENV_EDGE"), err)
	}
}
