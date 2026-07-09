// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"errors"
	"internal/synctest"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// TestDSTCrashAndRestartOverLiveHostFS: the public Crash applies the full
// process-crash contract — pid dead (Kill → ESRCH, procfs gone), goroutines
// frozen, flocks released, conns RESET at the peer, listener closed — while
// the host filesystem survives byte-for-byte, UNSYNCED writes included (the
// kernel outlives a process crash; only a host crash tears to the durable
// image). A same-name Process restart then reopens the surviving image with a
// fresh pid and clean process-owned resources.
func TestDSTCrashAndRestartOverLiveHostFS(t *testing.T) {
	var victimPID, restartPID int
	var killErr, statErr, restartLockErr error
	var peerReadErr error
	var dialErr error
	var progressAtCrash, progressAfter int32
	var restartData string
	Test(t, 1, func(t *testing.T) {
		Host("h", HostConfig{}, func() {
			started := make(chan int, 1)
			srvErr := make(chan error, 2)
			addrCh := make(chan string, 1)
			var progress atomic.Int32
			go Process("victim", func() {
				started <- os.Getpid()
				f, err := os.OpenFile("/db", os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					srvErr <- err
					return
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					srvErr <- err
					return
				}
				// An UNSYNCED write: it must survive the process crash (page
				// cache is the kernel's), unlike a host crash.
				if _, err := f.Write([]byte("unsynced")); err != nil {
					srvErr <- err
					return
				}
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					srvErr <- err
					return
				}
				addrCh <- l.Addr().String()
				c, err := l.Accept()
				if err != nil {
					srvErr <- err
					return
				}
				_ = c
				for {
					progress.Add(1)
					runtime.Gosched()
				}
			})
			victimPID = <-started
			addr := <-addrCh
			peer, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("peer Dial: %v", err)
			}
			for progress.Load() == 0 {
				runtime.Gosched()
			}

			Crash("victim")

			killErr = syscall.Kill(victimPID, 0)
			_, statErr = os.Stat("/proc/" + strconv.Itoa(victimPID) + "/stat")
			progressAtCrash = progress.Load()
			_, peerReadErr = peer.Read(make([]byte, 1))
			_, dialErr = net.Dial("tcp", addr)
			select {
			case err := <-srvErr:
				t.Fatalf("victim setup: %v", err)
			default:
			}

			Process("victim", func() {
				restartPID = os.Getpid()
				f, err := os.OpenFile("/db", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("restart OpenFile: %v", err)
				}
				defer f.Close()
				// The crash released the predecessor's lock.
				restartLockErr = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				b := make([]byte, 8)
				if _, err := f.Read(b); err != nil {
					t.Fatalf("restart Read: %v", err)
				}
				restartData = string(b)
			})
			for range 20 {
				runtime.Gosched()
			}
			progressAfter = progress.Load()
		})
	})
	if !errors.Is(killErr, syscall.ESRCH) {
		t.Fatalf("Kill(crashed pid, 0) = %v, want ESRCH", killErr)
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stat crashed /proc/<pid>/stat = %v, want not-exist", statErr)
	}
	if progressAfter != progressAtCrash {
		t.Fatalf("crashed goroutine kept running: %d then %d", progressAtCrash, progressAfter)
	}
	if !errors.Is(peerReadErr, syscall.ECONNRESET) {
		t.Fatalf("peer read after crash = %v, want ECONNRESET (crash RSTs)", peerReadErr)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Fatalf("dial after crash = %v, want ECONNREFUSED (listener died)", dialErr)
	}
	if restartPID == victimPID {
		t.Fatalf("restart reused pid %d", victimPID)
	}
	if restartLockErr != nil {
		t.Fatalf("restart Flock = %v, want success (crash released the lock)", restartLockErr)
	}
	// That the write is genuinely UNSYNCED — never committed to the durable
	// image — is pinned by os.TestDSTFSDurabilityMonotonicity (writes move
	// only current state; sync alone advances the image). The complementary
	// leg, that a HOST crash loses exactly these bytes, lands with CrashHost.
	if restartData != "unsynced" {
		t.Fatalf("restart read %q, want %q (process crash leaves the page cache intact)", restartData, "unsynced")
	}
}

// TestDSTCrashProcessBlockedInSynctestWait: a victim blocked in synctest.Wait is
// the bubble's registered waiter. The crash must clear that registration —
// otherwise the bubble's next quiescence hands the wake to a goroutine the
// scheduler will never select (crashed goroutines are dropped), leaking the
// active count and hanging the run forever.
func TestDSTCrashProcessBlockedInSynctestWait(t *testing.T) {
	var completed bool
	Test(t, 1, func(t *testing.T) {
		waiting := make(chan struct{})
		go Process("waiter", func() {
			go func() {
				// Keep the bubble non-idle until Wait registers, then block
				// durably so Wait can proceed to park.
				close(waiting)
				select {}
			}()
			<-waiting
			synctest.Wait()
		})
		<-waiting
		for range 5 {
			runtime.Gosched()
		}
		Crash("waiter")
		// The run must still reach quiescence and complete.
		completed = true
	})
	if !completed {
		t.Fatalf("run did not complete after crashing a goroutine blocked in synctest.Wait")
	}
}

// TestDSTCrashSelf: a goroutine of the victim crashing its own process (the
// OOM shape) never returns from Crash — code after the call is dead, defers
// are forfeited (a killed process does not unwind) — and the run continues:
// the supervisor observes the death and restarts.
func TestDSTCrashSelf(t *testing.T) {
	var afterCrashRan, deferRan atomic.Bool
	var pid int
	var killErr error
	var restarted bool
	Test(t, 1, func(t *testing.T) {
		started := make(chan int, 1)
		go Process("victim", func() {
			defer deferRan.Store(true)
			started <- os.Getpid()
			Crash("victim")
			afterCrashRan.Store(true)
		})
		pid = <-started
		for range 10 {
			runtime.Gosched()
		}
		killErr = syscall.Kill(pid, 0)
		Process("victim", func() { restarted = true })
	})
	if afterCrashRan.Load() {
		t.Fatalf("code after a self-crash ran")
	}
	if deferRan.Load() {
		t.Fatalf("a self-crashed goroutine's defer ran (a killed process does not unwind)")
	}
	if !errors.Is(killErr, syscall.ESRCH) {
		t.Fatalf("Kill(self-crashed pid, 0) = %v, want ESRCH", killErr)
	}
	if !restarted {
		t.Fatalf("restart after self-crash did not run")
	}
}

// TestDSTCrashKillingTheRunMainPanics: a Process declared directly on the run's
// own goroutine has the bubble main in its goroutine set; crashing it would
// leave the universe with no driver — the body's remaining statements (a
// test's assertions among them) would never run and the bubble would never
// complete. Refused loudly, BEFORE any goroutine is marked, naming the fix
// (declare the crashable process on its own goroutine). The alternative is a
// whole-binary "all goroutines are asleep" fatal with no diagnostic.
func TestDSTCrashKillingTheRunMainPanics(t *testing.T) {
	var got any
	Run(1, func() {
		func() {
			defer func() { got = recover() }()
			Process("inline", func() {
				Crash("inline")
			})
		}()
	})
	if got == nil {
		t.Fatalf("crashing a process that owns the run's main goroutine did not panic")
	}
	msg, _ := got.(string)
	if !strings.Contains(msg, "would kill the run's main goroutine") {
		t.Fatalf("panic = %v, want the run-main refusal naming the fix", got)
	}
}

// TestDSTCrashNestedInvocationParked: a victim goroutine that is INSIDE a
// nested Process body when its enclosing invocation dies carries the inner
// pid at mark time and escapes the kill — the pid restore at the nested
// body's return must park it forever instead of resuming a dead invocation's
// goroutine (a thread cannot outlive its process). Both the crash and the
// normal-exit form of the enclosing death are pinned.
func TestDSTCrashNestedInvocationParked(t *testing.T) {
	for _, mode := range []string{"crash", "exit"} {
		t.Run(mode, func(t *testing.T) {
			var resumed atomic.Bool
			Test(t, 1, func(t *testing.T) {
				entered := make(chan struct{})
				release := make(chan struct{})
				outerDead := make(chan struct{})
				body := func() {
					go func() {
						Process("inner-"+mode, func() {
							close(entered)
							<-release
						})
						// Only reachable if the dead outer invocation's
						// goroutine resumed past the nested body.
						resumed.Store(true)
					}()
					if mode == "crash" {
						select {} // stay alive until crashed
					}
					<-entered // exit mode: return once the nested goroutine is inside
				}
				go func() {
					Process("outer-"+mode, body)
					close(outerDead) // exit mode reaches this; crash mode never does
				}()
				<-entered
				if mode == "crash" {
					Crash("outer-" + mode)
				} else {
					<-outerDead // the outer invocation has fully exited
				}
				close(release) // the nested goroutine leaves the inner body now
				for range 20 {
					runtime.Gosched()
				}
			})
			if resumed.Load() {
				t.Fatalf("a goroutine of a dead invocation resumed past its nested Process body (%s mode)", mode)
			}
		})
	}
}
