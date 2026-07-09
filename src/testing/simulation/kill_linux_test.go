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
	var groupSelfErr, groupAllErr, groupUnknownErr error
	var aliasErr error
	var liveErr, deadErr, restartLiveErr, staleDuringRestartErr, restartDeadErr error
	var panicPid int
	var panicDeadErr error
	var peerPID, restartPID int
	var rawPanicked, signalPanicked bool
	var nonBubbleRootErr, nonBubbleHostErr, nonBubbleSignalErr error
	nonBubbleProceed := make(chan struct{})
	nonBubbleDone := make(chan struct{})
	go func() {
		defer close(nonBubbleDone)
		<-nonBubbleProceed
		nonBubbleRootErr = syscall.Kill(simRootPID, 0)
		nonBubbleHostErr = syscall.Kill(realPID, 0)
		// Non-zero signals are a FENCE, and fences fire only for bubble
		// goroutines: a non-bubble goroutine's Kill(sig != 0) reaches the host
		// kernel even mid-run (the harness keeps host access). simRootPID names
		// no host process, so the kernel answers ESRCH without signalling
		// anything — while the simulated registry holds that pid LIVE (it is
		// the run's root pid, success there) and the fence would panic, so
		// ESRCH is unambiguous proof of host fall-through.
		nonBubbleSignalErr = syscall.Kill(simRootPID, syscall.Signal(-1))
	}()

	RunWith(1, Options{PID: simRootPID}, func() {
		close(nonBubbleProceed)
		<-nonBubbleDone

		rootErr = syscall.Kill(os.Getpid(), 0)
		hostErr = syscall.Kill(realPID, 0)
		unknownErr = syscall.Kill(simRootPID+500000, 0)
		groupSelfErr = syscall.Kill(0, 0)
		groupAllErr = syscall.Kill(-1, 0)
		groupUnknownErr = syscall.Kill(-5, 0)
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
	// kill(0, 0) probes the caller's own process group and kill(-1, 0) "every
	// process the caller may signal" — both always succeed on Linux. Other
	// negative pids name process groups the simulation does not model: unknown,
	// so ESRCH.
	if groupSelfErr != nil {
		t.Fatalf("Kill(0, 0) = %v, want nil (own process group always exists)", groupSelfErr)
	}
	if groupAllErr != nil {
		t.Fatalf("Kill(-1, 0) = %v, want nil (self is always signalable)", groupAllErr)
	}
	if !errors.Is(groupUnknownErr, syscall.ESRCH) {
		t.Fatalf("Kill(-5, 0) = %v, want ESRCH (unmodeled process group)", groupUnknownErr)
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
	if !errors.Is(nonBubbleSignalErr, syscall.ESRCH) {
		t.Fatalf("non-bubble Kill(sim-live pid, Signal(-1)) = %v, want host ESRCH (fences fire only for bubble goroutines; the sim path would answer success and the fence would panic)", nonBubbleSignalErr)
	}
	if !rawPanicked {
		t.Fatalf("raw SYS_KILL did not panic; generic raw syscalls must remain fenced")
	}
}
