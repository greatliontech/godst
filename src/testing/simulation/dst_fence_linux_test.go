// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"os"
	"os/signal"
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

// TestDSTOSProcessFence checks the os-level interception fences (design.md "The
// interception boundary"): from a bubble goroutine, os.StartProcess is refused
// with the error shape (before the pidfd probe and the attr.Files Fd() loop),
// os.Executable is refused (a host path naming nothing in the simulated
// namespace), and os/signal.Notify is refused with a panic (no error channel).
func TestDSTOSProcessFence(t *testing.T) {
	var (
		startErr       error
		startPanicked  bool
		execPath       string
		execErr        error
		notifyPanicked bool
		ignorePanicked bool
		resetPanicked  bool
		stopPanicked   bool
	)

	Run(1, func() {
		// Pass a simulated file in attr.Files. The os-level fence must refuse
		// before the attr.Files f.Fd() loop — a simulated file has no honest
		// descriptor, so f.Fd() panics ("os: Fd on a simulated file"). This gives
		// the os.startProcess fence teeth distinct from syscall.forkExec's (which
		// only refuses after the Fd loop, past the accidental panic).
		sf, err := os.CreateTemp("", "dst-proc-fence")
		if err != nil {
			t.Errorf("CreateTemp in bubble: %v", err)
			return
		}
		defer sf.Close()
		startPanicked = dstDidPanic(func() {
			_, startErr = os.StartProcess("/nonexistent/dst-fence-probe", []string{"probe"},
				&os.ProcAttr{Files: []*os.File{sf}})
		})
		execPath, execErr = os.Executable()
		notifyPanicked = dstDidPanic(func() {
			signal.Notify(make(chan os.Signal, 1), os.Interrupt)
		})
		ignorePanicked = dstDidPanic(func() { signal.Ignore(os.Interrupt) })
		resetPanicked = dstDidPanic(func() { signal.Reset(os.Interrupt) })
		stopPanicked = dstDidPanic(func() { signal.Stop(make(chan os.Signal, 1)) })
	})

	if startPanicked {
		t.Errorf("os.StartProcess in bubble panicked: the os-level fence must refuse before the attr.Files Fd() loop")
	}
	if startErr == nil || !strings.Contains(startErr.Error(), "unsupported under deterministic simulation") {
		t.Errorf("os.StartProcess in bubble = %v, want unsupported-under-simulation refusal", startErr)
	}
	// Exact match: the os.Executable fence returns errDSTUnsupported directly,
	// before the /proc/self/exe readlink. Without the fence, executable() reaches
	// that readlink and the simulated FS refuses it too — but with a *PathError
	// ("readlink /proc/self/exe: filesystem operation unsupported…"), a different
	// string. Asserting the exact fence message keeps this test's teeth on the
	// os.Executable fence rather than the FS refusal that would otherwise mask it.
	if execErr == nil || execErr.Error() != "unsupported under deterministic simulation" {
		t.Errorf("os.Executable in bubble = (%q, %v), want the os.Executable fence error exactly", execPath, execErr)
	}
	if !notifyPanicked {
		t.Errorf("os/signal.Notify in bubble did not panic: the signal fence is inactive")
	}
	if !ignorePanicked {
		t.Errorf("os/signal.Ignore in bubble did not panic: it mutates host signal disposition and must be fenced")
	}
	if !resetPanicked {
		t.Errorf("os/signal.Reset in bubble did not panic: it mutates host signal disposition and must be fenced")
	}
	if !stopPanicked {
		t.Errorf("os/signal.Stop in bubble did not panic: it touches host signal machinery and must be fenced")
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
