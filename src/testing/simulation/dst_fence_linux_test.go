// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// TestDSTSyscallFence checks the interception-boundary fence (design.md "The
// interception boundary"): from inside a run's bubble, a raw syscall that mints
// a host resource is refused loudly, while an I/O syscall on the inherited-
// handle allowlist is left alone. The refusal is deterministic — same seed,
// same panic — instead of a silent real socket the seed never controlled.
func TestDSTSyscallFence(t *testing.T) {
	var (
		socketPanicked  bool
		allowedPanicked bool
		forkPanicked    bool
		execPanicked    bool
		forkErr         error
		execErr         error
	)

	Run(1, func() {
		// Minting a socket is a simulation escape: fenced (panic).
		socketPanicked = dstDidPanic(func() {
			syscall.RawSyscall(syscall.SYS_SOCKET, uintptr(syscall.AF_INET), uintptr(syscall.SOCK_STREAM), 0)
		})

		// close() is on the allowlist: an fd argument can only name a real host
		// handle (a simulated file never exposes one), so I/O on it is the
		// sanctioned inherited-handle stance, not fenced. A bogus fd returns
		// EBADF; the point is that it does not panic.
		allowedPanicked = dstDidPanic(func() {
			syscall.RawSyscall(syscall.SYS_CLOSE, 0x7fffffff, 0, 0)
		})

		// Process spawn is refused via the ERROR shape, at the exec entry point
		// (not by the trampoline panic on the eventual clone/execve). That entry
		// fence is load-bearing beyond the message shape: it stops a bubble
		// BEFORE forkExec forks and BEFORE Exec calls runtime_BeforeExec (which
		// holds execLock across the exec) — reaching either and then panicking
		// at the trampoline would strand process-global runtime state. So assert
		// both that it does NOT panic and that the error is the refusal. A
		// nonexistent path makes a broken fence surface a distinguishable ENOENT
		// (not a real child) rather than the "unsupported" message.
		forkPanicked = dstDidPanic(func() {
			_, forkErr = syscall.ForkExec("/nonexistent/dst-fence-probe", []string{"probe"}, &syscall.ProcAttr{})
		})
		execPanicked = dstDidPanic(func() {
			execErr = syscall.Exec("/nonexistent/dst-fence-probe", []string{"probe"}, nil)
		})
	})

	if !socketPanicked {
		t.Errorf("raw SYS_SOCKET in bubble did not panic: the resource-minting fence is inactive")
	}
	if allowedPanicked {
		t.Errorf("raw SYS_CLOSE in bubble panicked: the inherited-handle allowlist is broken")
	}
	if forkPanicked {
		t.Errorf("ForkExec in bubble panicked: it must be refused via the error shape at the entry fence, before forking")
	}
	if execPanicked {
		t.Errorf("Exec in bubble panicked: it must be refused via the error shape at the entry fence, before runtime_BeforeExec")
	}
	if forkErr == nil || !strings.Contains(forkErr.Error(), "unsupported under deterministic simulation") {
		t.Errorf("ForkExec in bubble = %v, want unsupported-under-simulation refusal", forkErr)
	}
	if execErr == nil || !strings.Contains(execErr.Error(), "unsupported under deterministic simulation") {
		t.Errorf("Exec in bubble = %v, want unsupported-under-simulation refusal", execErr)
	}
}

// TestDSTFenceIsBubbleScoped checks the other half of the contract: the fence
// fires ONLY for bubble goroutines. A non-bubble goroutine (the harness around
// the run) keeps full host access even while a run is active. The regression
// this catches is dropping the bubble check from dstFenceActive (fencing on
// dstActive() alone), which would refuse the harness's own syscalls mid-run.
func TestDSTFenceIsBubbleScoped(t *testing.T) {
	var nonBubblePanicked atomic.Bool
	proceed := make(chan struct{}) // bubble -> non-bubble: a run is now active
	finished := make(chan struct{})

	go func() {
		defer close(finished)
		<-proceed // do the syscall only while dstActive is true
		defer func() {
			if recover() != nil {
				nonBubblePanicked.Store(true)
			}
		}()
		// A resource-minting syscall from this NON-bubble goroutine must not be
		// fenced. Create then close a real UDP socket; a wrongly-firing fence
		// would panic instead. (A sandbox that blocks socket() returns an errno,
		// which is fine — the assertion is only about the absence of a panic.)
		fd, _, errno := syscall.RawSyscall(syscall.SYS_SOCKET, uintptr(syscall.AF_INET), uintptr(syscall.SOCK_DGRAM), 0)
		if errno == 0 {
			syscall.RawSyscall(syscall.SYS_CLOSE, fd, 0, 0)
		}
	}()

	Run(1, func() {
		// proceed/finished are created outside the bubble, so blocking on them is
		// not durable blocking (they can be signalled by the outside world) — the
		// bubble root may wait on <-finished without synctest declaring a
		// deadlock. This pins the non-bubble syscall strictly between Run entry
		// and Run return, i.e. while dstActive is true.
		close(proceed) // the run is active; release the non-bubble goroutine
		<-finished     // hold the run open until its syscall has completed
	})

	if nonBubblePanicked.Load() {
		t.Errorf("non-bubble goroutine's raw SYS_SOCKET was fenced during an active run; the fence must be bubble-scoped, not run-scoped")
	}
}

func dstDidPanic(f func()) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	f()
	return
}
