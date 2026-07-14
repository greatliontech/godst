// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestDSTCrashProcessPidAndGoroutines(t *testing.T) {
	var pid int
	var restartPID int
	var killErr, statErr, restartLiveErr error
	var restarted bool
	var afterCrash, afterYields int32
	Test(t, 1, func(t *testing.T) {
		var progress atomic.Int32
		started := make(chan int, 1)
		go Process("victim", func() {
			started <- os.Getpid()
			for {
				progress.Add(1)
				runtime.Gosched()
			}
		})
		pid = <-started
		for progress.Load() == 0 {
			runtime.Gosched()
		}
		crashProcess("victim")
		killErr = syscall.Kill(pid, 0)
		_, statErr = os.Stat("/proc/" + strconv.Itoa(pid) + "/stat")
		Process("victim", func() {
			restarted = true
			restartPID = os.Getpid()
			restartLiveErr = syscall.Kill(restartPID, 0)
		})
		afterCrash = progress.Load()
		for range 20 {
			runtime.Gosched()
		}
		afterYields = progress.Load()
	})
	if !errors.Is(killErr, syscall.ESRCH) {
		t.Fatalf("Kill(crashed pid %d, 0) = %v, want ESRCH", pid, killErr)
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stat crashed /proc/%d/stat = %v, want not exist", pid, statErr)
	}
	if !restarted || restartPID == pid || restartLiveErr != nil {
		t.Fatalf("restart after crash: ran=%v pid=%d old=%d liveErr=%v, want ran with a fresh live pid", restarted, restartPID, pid, restartLiveErr)
	}
	if afterYields != afterCrash {
		t.Fatalf("crashed goroutine kept running: progress after crash %d, after yields %d", afterCrash, afterYields)
	}
}

func TestDSTCrashProcessSameNameActiveInvocations(t *testing.T) {
	var pids []int
	var killErrs []error
	Test(t, 1, func(t *testing.T) {
		ready := make(chan int, 2)
		for range 2 {
			go Process("dup", func() {
				ready <- os.Getpid()
				select {}
			})
		}
		pids = append(pids, <-ready, <-ready)
		crashProcess("dup")
		for _, pid := range pids {
			killErrs = append(killErrs, syscall.Kill(pid, 0))
		}
	})
	for i, err := range killErrs {
		if !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("Kill(crashed duplicate pid %d, 0) = %v, want ESRCH", pids[i], err)
		}
	}
}

func TestDSTCrashProcessAbandonsBlockedChannelSend(t *testing.T) {
	var received bool
	Test(t, 1, func(t *testing.T) {
		ch := make(chan int)
		ready := make(chan struct{}, 1)
		go Process("sender", func() {
			ready <- struct{}{}
			ch <- 42
		})
		<-ready
		for range 5 {
			runtime.Gosched()
		}
		crashProcess("sender")
		Process("receiver", func() {
			select {
			case <-ch:
				received = true
			default:
			}
		})
	})
	if received {
		t.Fatalf("receive completed with a crashed process's blocked send")
	}
}

func TestDSTCrashProcessReleasesFileResources(t *testing.T) {
	var leaked *os.File
	var leakedWriteErr, lockErr error
	var content string
	Test(t, 1, func(t *testing.T) {
		Host("h", HostConfig{}, func() {
			ready := make(chan *os.File, 1)
			go Process("owner", func() {
				f, err := os.OpenFile("/db", os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					t.Fatalf("owner open: %v", err)
				}
				if _, err := f.WriteAt([]byte("survives"), 0); err != nil {
					t.Fatalf("owner write: %v", err)
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					t.Fatalf("owner flock: %v", err)
				}
				ready <- f
				select {}
			})
			leaked = <-ready
			crashProcess("owner")
			_, leakedWriteErr = leaked.Write([]byte("x"))
			Process("peer", func() {
				f, err := os.OpenFile("/db", os.O_RDWR, 0)
				if err != nil {
					t.Fatalf("peer open: %v", err)
				}
				defer f.Close()
				lockErr = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				b, err := os.ReadFile("/db")
				if err != nil {
					t.Fatalf("peer read: %v", err)
				}
				content = string(b)
			})
		})
	})
	if !errors.Is(leakedWriteErr, os.ErrClosed) {
		t.Fatalf("write through crashed process file = %v, want ErrClosed", leakedWriteErr)
	}
	if lockErr != nil {
		t.Fatalf("peer flock after crash = %v, want nil", lockErr)
	}
	if content != "survives" {
		t.Fatalf("file content after process crash = %q, want %q", content, "survives")
	}
}

func TestDSTRootClosesOnProcessExitAndCrash(t *testing.T) {
	for _, crash := range []bool{false, true} {
		name := "exit"
		if crash {
			name = "crash"
		}
		t.Run(name, func(t *testing.T) {
			var leaked *os.Root
			Test(t, 1, func(t *testing.T) {
				Host("h", HostConfig{}, func() {
					ready := make(chan *os.Root, 1)
					go Process("owner", func() {
						os.Mkdir("/d", 0o755)
						r, err := os.OpenRoot("/d")
						if err != nil {
							t.Fatal(err)
						}
						ready <- r
						if crash {
							select {}
						}
					})
					leaked = <-ready
					if crash {
						crashProcess("owner")
					}
					if _, err := leaked.Stat("."); !errors.Is(err, os.ErrClosed) {
						t.Fatalf("Root.Stat after process %s = %v, want ErrClosed", name, err)
					}
				})
			})
		})
	}
}

func TestDSTCrashProcessClosesListeners(t *testing.T) {
	var restartListenErr error
	Test(t, 1, func(t *testing.T) {
		port := make(chan string, 1)
		go Process("server", func() {
			ln, err := net.Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, p, _ := net.SplitHostPort(ln.Addr().String())
			port <- p
			select {}
		})
		p := <-port
		crashProcess("server")
		Process("server", func() {
			ln, err := net.Listen("tcp", ":"+p)
			restartListenErr = err
			if err == nil {
				ln.Close()
			}
		})
	})
	if restartListenErr != nil {
		t.Fatalf("restart listen on crashed process port = %v, want nil", restartListenErr)
	}
}

func TestDSTCrashProcessMmapPreservesSharedContents(t *testing.T) {
	var content []byte
	Test(t, 1, func(t *testing.T) {
		Host("h", HostConfig{}, func() {
			ready := make(chan struct{}, 1)
			go Process("mapper", func() {
				f, err := os.OpenFile("/mapped", os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					t.Fatalf("mapper open: %v", err)
				}
				if err := f.Truncate(int64(syscall.Getpagesize())); err != nil {
					t.Fatalf("mapper truncate: %v", err)
				}
				data, err := syscall.Mmap(int(f.Fd()), 0, syscall.Getpagesize(), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					t.Fatalf("mapper mmap: %v", err)
				}
				data[0] = 'Z'
				ready <- struct{}{}
				select {}
			})
			<-ready
			crashProcess("mapper")
			Process("reader", func() {
				var err error
				content, err = os.ReadFile("/mapped")
				if err != nil {
					t.Fatalf("reader read: %v", err)
				}
			})
		})
	})
	if len(content) == 0 || content[0] != 'Z' {
		t.Fatalf("shared mmap content after process crash starts %q, want Z", content[:min(len(content), 1)])
	}
}

func TestDSTCrashProcessResetsConnections(t *testing.T) {
	var readErr error
	Test(t, 1, func(t *testing.T) {
		port := make(chan string, 1)
		accepted := make(chan net.Conn, 1)
		go Process("server", func() {
			ln, err := net.Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, p, _ := net.SplitHostPort(ln.Addr().String())
			port <- p
			c, err := ln.Accept()
			if err != nil {
				t.Fatalf("accept: %v", err)
			}
			accepted <- c
			select {}
		})
		Process("client", func() {
			p := <-port
			c, err := net.Dial("tcp", HostIP("server")+":"+p)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			<-accepted
			crashProcess("server")
			_ = c.SetReadDeadline(time.Now())
			_, readErr = c.Read(make([]byte, 1))
		})
	})
	if !errors.Is(readErr, syscall.ECONNRESET) {
		t.Fatalf("client read after server crash = %v, want ECONNRESET", readErr)
	}
}

// TestDSTCrashProcessAbandonsCondWait: a crashed process's goroutine parked
// in sync.Cond.Wait never runs again, and a survivor's Signal or Broadcast
// is not lost to it — the runtime's notify-list dequeues skip crashed
// waiters (dstPid < 0) exactly as the chan and sema dequeues do, passing a
// Signal's consumed ticket on to the next live waiter.
func TestDSTCrashProcessAbandonsCondWait(t *testing.T) {
	var crashedRan atomic.Bool
	var signalWoke, broadcastWoke bool
	Test(t, 1, func(t *testing.T) {
		var mu sync.Mutex
		cond := sync.NewCond(&mu)
		parked := make(chan struct{}, 1)

		// Signal arm: the crashed waiter holds the oldest ticket; Signal
		// must skip it and wake the live waiter queued behind it.
		go Process("victim", func() {
			mu.Lock()
			parked <- struct{}{}
			cond.Wait()
			crashedRan.Store(true)
			mu.Unlock()
		})
		<-parked
		// Fake time advances only at quiescence: after the sleep the victim
		// is durably parked inside Wait, holding the oldest ticket.
		time.Sleep(time.Millisecond)
		crashProcess("victim")

		signalDone := make(chan struct{})
		go Process("waiter", func() {
			mu.Lock()
			parked <- struct{}{}
			cond.Wait()
			signalWoke = true
			mu.Unlock()
			close(signalDone)
		})
		<-parked
		time.Sleep(time.Millisecond)
		Process("signaler", func() {
			cond.Signal()
		})
		<-signalDone

		// Broadcast arm: a second crashed waiter shares the list with a
		// live one; Broadcast must wake only the live waiter.
		go Process("victim2", func() {
			mu.Lock()
			parked <- struct{}{}
			cond.Wait()
			crashedRan.Store(true)
			mu.Unlock()
		})
		<-parked
		time.Sleep(time.Millisecond)
		crashProcess("victim2")

		broadcastDone := make(chan struct{})
		go Process("waiter2", func() {
			mu.Lock()
			parked <- struct{}{}
			cond.Wait()
			broadcastWoke = true
			mu.Unlock()
			close(broadcastDone)
		})
		<-parked
		time.Sleep(time.Millisecond)
		Process("broadcaster", func() {
			cond.Broadcast()
		})
		<-broadcastDone
		// One more quiescence: a wrongly-woken crashed goroutine gets every
		// chance to run (and trip the flag) before the run ends.
		time.Sleep(time.Millisecond)
	})
	if crashedRan.Load() {
		t.Fatal("a crashed process's Cond.Wait goroutine ran after Signal/Broadcast")
	}
	if !signalWoke {
		t.Fatal("Signal was lost to the crashed waiter; the live waiter never woke")
	}
	if !broadcastWoke {
		t.Fatal("Broadcast did not wake the live waiter")
	}
}

// TestDSTCondAbandonedRunWaitersSkippedByLaterRun: an aborted run's parked
// Cond waiters are retired at deactivation (negative pid, still pointing at
// the dead bubble), and a LATER run signaling or broadcasting the shared
// Cond must skip them — the notify-list dequeues key on dstPid < 0 before
// the cross-bubble wake check, which would otherwise be a runtime fatal
// against the dead bubble — and deliver the wakeup to its own live waiter.
func TestDSTCondAbandonedRunWaitersSkippedByLaterRun(t *testing.T) {
	var muOne, muAll sync.Mutex
	condOne := sync.NewCond(&muOne)
	condAll := sync.NewCond(&muAll)
	var leftoverRan atomic.Bool

	// Run 1: park one waiter on each cond and abandon them — returning with
	// members durably parked aborts the run with the bubble-deadlock panic.
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("run abandoning parked Cond waiters completed; want the bubble-deadlock abort")
			}
			if !strings.Contains(fmt.Sprint(r), "deadlock") {
				panic(r) // not the bubble-deadlock diagnostic: repanic
			}
		}()
		Run(1, func() {
			for _, c := range []*sync.Cond{condOne, condAll} {
				go func(c *sync.Cond) {
					c.L.Lock()
					c.Wait()
					leftoverRan.Store(true)
					c.L.Unlock()
				}(c)
			}
			time.Sleep(time.Millisecond) // both waiters durably parked in Wait
		})
	}()

	// Run 2: a fresh bubble over the same Cond objects, one live waiter each.
	Run(2, func() {
		signalWoke := make(chan struct{})
		go func() {
			muOne.Lock()
			condOne.Wait()
			muOne.Unlock()
			close(signalWoke)
		}()
		time.Sleep(time.Millisecond)
		condOne.Signal() // the oldest ticket is the leftover's: skip it, wake the live waiter
		<-signalWoke

		bcastWoke := make(chan struct{})
		go func() {
			muAll.Lock()
			condAll.Wait()
			muAll.Unlock()
			close(bcastWoke)
		}()
		time.Sleep(time.Millisecond)
		condAll.Broadcast() // the leftover rides the pulled list: skip it
		<-bcastWoke
	})
	if leftoverRan.Load() {
		t.Fatal("an abandoned run's Cond waiter ran in a later run")
	}
}
