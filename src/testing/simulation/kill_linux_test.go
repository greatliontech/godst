// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"errors"
	"os"
	"strconv"
	"syscall"
	"testing"
)

func TestDSTKillPidZeroLiveness(t *testing.T) {
	realPID := os.Getpid()
	simRootPID := realPID + 100000
	var rootErr, hostErr, unknownErr error
	var aliasErr error
	var liveErr, deadErr, restartLiveErr, staleDuringRestartErr, restartDeadErr error
	var panicPid int
	var panicDeadErr error
	var peerPID, restartPID int
	var rawPanicked, signalPanicked bool
	var nonBubbleRootErr, nonBubbleHostErr error
	var nonBubbleSignalPanicked bool
	nonBubbleProceed := make(chan struct{})
	nonBubbleDone := make(chan struct{})
	go func() {
		defer close(nonBubbleDone)
		<-nonBubbleProceed
		nonBubbleRootErr = syscall.Kill(simRootPID, 0)
		nonBubbleHostErr = syscall.Kill(realPID, 0)
		nonBubbleSignalPanicked = dstDidPanic(func() {
			_ = syscall.Kill(simRootPID, syscall.Signal(-1))
		})
	}()

	RunWith(1, Options{PID: simRootPID}, func() {
		close(nonBubbleProceed)
		<-nonBubbleDone

		rootErr = syscall.Kill(os.Getpid(), 0)
		hostErr = syscall.Kill(realPID, 0)
		unknownErr = syscall.Kill(simRootPID+500000, 0)
		if strconv.IntSize == 64 {
			aliasErr = syscall.Kill(int(int64(simRootPID)+(1<<32)), 0)
		}

		ready := make(chan int)
		release := make(chan struct{})
		exited := make(chan struct{})
		go func() {
			Process("peer", func() {
				ready <- os.Getpid()
				<-release
			})
			close(exited)
		}()
		peerPID = <-ready
		liveErr = syscall.Kill(peerPID, 0)
		close(release)
		<-exited
		deadErr = syscall.Kill(peerPID, 0)

		Process("peer", func() {
			restartPID = os.Getpid()
			restartLiveErr = syscall.Kill(restartPID, 0)
			staleDuringRestartErr = syscall.Kill(peerPID, 0)
		})
		restartDeadErr = syscall.Kill(restartPID, 0)

		func() {
			defer func() { _ = recover() }()
			Process("panic-peer", func() {
				panicPid = os.Getpid()
				panic("boom")
			})
		}()
		panicDeadErr = syscall.Kill(panicPid, 0)

		signalPanicked = dstDidPanic(func() {
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		})
		rawPanicked = dstDidPanic(func() {
			syscall.Syscall(syscall.SYS_KILL, uintptr(os.Getpid()), 0, 0)
		})
	})

	if rootErr != nil {
		t.Fatalf("Kill(root pid, 0) = %v, want nil", rootErr)
	}
	if nonBubbleRootErr != nil {
		t.Fatalf("non-bubble Kill(root pid, 0) = %v, want nil", nonBubbleRootErr)
	}
	for name, err := range map[string]error{
		"host real pid":              hostErr,
		"non-bubble host real pid":   nonBubbleHostErr,
		"unknown pid":                unknownErr,
		"out-of-range alias pid":     aliasErr,
		"dead pid":                   deadErr,
		"stale pid during restart":   staleDuringRestartErr,
		"restarted pid after return": restartDeadErr,
		"panicked process pid":       panicDeadErr,
	} {
		if name == "out-of-range alias pid" && strconv.IntSize != 64 {
			continue
		}
		if !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("Kill(%s, 0) = %v, want ESRCH", name, err)
		}
	}
	if liveErr != nil {
		t.Fatalf("Kill(live peer pid %d, 0) = %v, want nil", peerPID, liveErr)
	}
	if restartPID == peerPID {
		t.Fatalf("restart reused pid %d", peerPID)
	}
	if restartLiveErr != nil {
		t.Fatalf("Kill(restarted live pid %d, 0) = %v, want nil", restartPID, restartLiveErr)
	}
	if !signalPanicked {
		t.Fatalf("Kill(pid, SIGTERM) did not panic; non-zero signals must remain fenced")
	}
	if !nonBubbleSignalPanicked {
		t.Fatalf("non-bubble Kill(pid, non-zero) did not panic; non-zero signals must remain fenced during a run")
	}
	if !rawPanicked {
		t.Fatalf("raw SYS_KILL did not panic; generic raw syscalls must remain fenced")
	}
}
