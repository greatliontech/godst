// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	isync "internal/sync"
	"internal/synctest"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

//go:linkname dstSemacquireFP runtime.dstSemacquireFP
func dstSemacquireFP(addr *uint32)

//go:linkname dstSemreleaseHandoffFP runtime.dstSemreleaseHandoffFP
func dstSemreleaseHandoffFP(addr *uint32)

//go:linkname dstSemreleaseDSTHandoffFP runtime.dstSemreleaseDSTHandoffFP
func dstSemreleaseDSTHandoffFP(addr *uint32)

//go:linkname dstClockOffsetNowFP runtime.dstClockOffsetNow
func dstClockOffsetNowFP() int64

func TestDSTKernelMutexUninterruptibleUnderCrashAndExit(t *testing.T) {
	if !dstRaceEnabledFP() {
		t.Skip("requires -tags dst -race")
	}
	res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 64, MaxSteps: 256}, func() error {
		hostDone := make(chan struct{})
		crashReady := make(chan struct{})
		go Host("h1", func() {
			defer close(hostDone)
			// 1. Process that exits normally while a child goroutine is touching
			// modeled OS/env kernel locks.
			exitDone := make(chan struct{})
			go Process("exit_proc", func() {
				defer close(exitDone)
				go func() {
					_ = os.Setenv("K", "V")
					_ = os.WriteFile("/tmp_exit", []byte("abc"), 0o644)
				}()
			})
			<-exitDone

			// 2. Process that stays live while background goroutines touch OS/net/env
			// locks so Crash("crash_proc") and CrashHost("h1") hit it mid-flight.
			go Process("crash_proc", func() {
				go func() {
					_ = os.Setenv("K2", "V2")
					_ = os.WriteFile("/tmp_crash", []byte("xyz"), 0o644)
					ln, err := net.Listen("tcp", "127.0.0.1:9100")
					if err == nil {
						_ = ln.Close()
					}
				}()
				close(crashReady)
				select {}
			})
		})
		<-crashReady
		CrashProcess("h1", "crash_proc")
		<-hostDone
		CrashHost("h1")

		// Surviving root host operations must never wedge on any kernel lock
		// held when exit_proc exited, crash_proc crashed, or h1 powered off.
		if err := os.WriteFile("/root_ok", []byte("ok"), 0o644); err != nil {
			panic(err)
		}
		_ = os.Setenv("ROOT_K", "1")
		if os.Getenv("ROOT_K") != "1" {
			return errors.New("sut failed")
		}
		return nil
	})
	if len(res.Failures) != 0 {
		t.Fatalf("unexpected failures (stranded kernel lock or race): %+v", res.Failures)
	}
}

func growStackAndTouchKernel(depth int, idx int) {
	var pad [256]byte
	pad[0] = byte(depth)
	if depth > 0 {
		growStackAndTouchKernel(depth-1, idx)
		if pad[0] == 255 {
			panic("unreachable")
		}
		return
	}
	name := fmt.Sprintf("/gc_stack_%d", idx)
	_ = os.WriteFile(name, []byte("payload"), 0o644)
	_, _ = os.ReadFile(name)
	_ = os.Setenv("STACK_K", "V")
	_ = os.Getenv("STACK_K")
}

func TestDSTKernelMutexUninterruptibleUnderGCScanstack(t *testing.T) {
	Run(7, func() {
		started := make(chan struct{})
		go Process("worker", func() {
			close(started)
			for i := 0; ; i++ {
				growStackAndTouchKernel(24, i)
				runtime.Gosched()
			}
		})
		<-started
		for range 8 {
			runtime.GC()
			runtime.Gosched()
		}
		CrashProcess("", "worker")
		if err := os.WriteFile("/post_crash", []byte("alive"), 0o644); err != nil {
			t.Fatalf("WriteFile after GC + Crash failed: %v", err)
		}
	})
}

func TestDSTSemaCrashWaitersMutexNormal(t *testing.T) {
	Run(11, func() {
		// Case 1: two crashed waiters only (previously left two phantom sema
		// tokens and mutexWoken, triggering "sync: inconsistent mutex state"
		// when two subsequent contenders raced onto m).
		var mu sync.Mutex
		mu.Lock()
		queued := make(chan struct{}, 2)
		go Process("crashed_all", func() {
			for range 2 {
				go func() {
					queued <- struct{}{}
					mu.Lock()
					mu.Unlock()
				}()
			}
			select {}
		})
		<-queued
		<-queued
		for range 30 {
			runtime.Gosched()
		}
		CrashProcess("", "crashed_all")
		mu.Unlock()

		// Two fresh contenders must acquire and release cleanly without throwing
		// "sync: inconsistent mutex state".
		mu.Lock()
		done := make(chan struct{}, 2)
		for range 2 {
			go func() {
				mu.Lock()
				mu.Unlock()
				done <- struct{}{}
			}()
		}
		for range 20 {
			runtime.Gosched()
		}
		mu.Unlock()
		<-done
		<-done

		// Case 2: mixed crashed waiter ahead of a live waiter.
		mu.Lock()
		crashedQueued := make(chan struct{})
		go Process("crashed_head", func() {
			close(crashedQueued)
			mu.Lock()
			mu.Unlock()
		})
		<-crashedQueued
		for range 20 {
			runtime.Gosched()
		}
		liveDone := make(chan struct{})
		go func() {
			mu.Lock()
			mu.Unlock()
			close(liveDone)
		}()
		for range 20 {
			runtime.Gosched()
		}
		CrashProcess("", "crashed_head")
		mu.Unlock()
		<-liveDone
	})
}

func TestDSTSemaCrashWaitersMutexStarving(t *testing.T) {
	for _, allCrashed := range []bool{true, false} {
		Run(23, func() {
			var smu sync.Mutex
			smu.Lock()
			victimReady := make(chan struct{})
			go Process("starve_crash", func() {
				close(victimReady)
				smu.Lock()
				smu.Unlock()
				select {}
			})
			<-victimReady
			for range 20 {
				runtime.Gosched()
			}
			// Each Unlock + immediate TryLock on this goroutine steals smu back in
			// normal mode while waking starve_crash; after 65 lost wakes,
			// starve_crash sets mutexStarving and re-parks on smu.sema.
			for range 65 {
				smu.Unlock()
				if !smu.TryLock() {
					t.Fatalf("unexpected handoff before starvation threshold")
				}
				for range 10 {
					runtime.Gosched()
				}
			}
			var liveDone chan struct{}
			if !allCrashed {
				liveDone = make(chan struct{})
				go func() {
					smu.Lock()
					smu.Unlock()
					close(liveDone)
				}()
				for range 20 {
					runtime.Gosched()
				}
			}
			CrashProcess("", "starve_crash")
			smu.Unlock()
			if liveDone != nil {
				<-liveDone
			}
			// Fresh Lock/Unlock must not deadlock or throw inconsistent mutex state.
			smu.Lock()
			smu.Unlock()
		})
	}
}

func TestDSTSemaCrashWaitersRWMutexReaders(t *testing.T) {
	for _, withLiveReader := range []bool{false, true} {
		Run(31, func() {
			var rw sync.RWMutex
			rw.Lock()

			crashedQueued := make(chan struct{}, 2)
			go Process("rw_readers", func() {
				for range 2 {
					go func() {
						crashedQueued <- struct{}{}
						rw.RLock()
						rw.RUnlock()
					}()
				}
				select {}
			})
			<-crashedQueued
			<-crashedQueued
			for range 20 {
				runtime.Gosched()
			}

			var liveDone chan struct{}
			if withLiveReader {
				liveDone = make(chan struct{})
				go func() {
					rw.RLock()
					rw.RUnlock()
					close(liveDone)
				}()
				for range 20 {
					runtime.Gosched()
				}
			}

			CrashProcess("", "rw_readers")
			rw.Unlock()
			if liveDone != nil {
				<-liveDone
			}

			// Subsequent exclusive Lock and shared RLock must succeed without
			// hanging on leaked readerCount or consuming orphan readerSem tokens.
			rw.Lock()
			rw.Unlock()
			rw.RLock()
			rw.RUnlock()
		})
	}
}

func TestDSTSemaCrashWaitersWaitGroup(t *testing.T) {
	// Subcase 1: crashed waiter queued ahead of live waiter (exercises
	// woke && crashed > 0 accounting in dstWaitGroupWake).
	Run(37, func() {
		var wg sync.WaitGroup
		wg.Add(1)

		crashedQueued := make(chan struct{})
		go Process("wg_head_crashed", func() {
			close(crashedQueued)
			wg.Wait()
		})
		<-crashedQueued
		for range 20 {
			runtime.Gosched()
		}

		liveDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(liveDone)
		}()
		for range 20 {
			runtime.Gosched()
		}

		CrashProcess("", "wg_head_crashed")
		wg.Done()
		<-liveDone
	})

	// Subcase 2: live waiter queued first (owns the w==0 TSan race.Write
	// annotation and returns cleanly before reuse) followed by a crashed
	// waiter, then WaitGroup reuse verifying zero orphan semaphore token.
	Run(38, func() {
		var wg sync.WaitGroup
		wg.Add(1)

		liveDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(liveDone)
		}()
		for range 20 {
			runtime.Gosched()
		}

		crashedQueued := make(chan struct{})
		go Process("wg_tail_crashed", func() {
			close(crashedQueued)
			wg.Wait()
		})
		<-crashedQueued
		for range 20 {
			runtime.Gosched()
		}

		CrashProcess("", "wg_tail_crashed")
		wg.Done()
		<-liveDone

		// Reuse wg: if an orphan token remained on wg.sema, the second Wait()
		// would return early before Done()!
		wg.Add(1)
		secondDone := make(chan struct{})
		var premature atomic.Bool
		releaseSecond := make(chan struct{})
		go func() {
			wg.Wait()
			select {
			case <-releaseSecond:
			default:
				premature.Store(true)
			}
			close(secondDone)
		}()
		for range 20 {
			runtime.Gosched()
		}
		close(releaseSecond)
		wg.Done()
		<-secondDone
		if premature.Load() {
			t.Fatalf("reused WaitGroup.Wait returned before Done (orphan semaphore token)")
		}
	})
}

func TestDSTExploreReplayPreservesBudgets(t *testing.T) {
	if !dstRaceEnabledFP() {
		t.Skip("requires -tags dst -race")
	}
	const addrCount = 72_000 // > default 65_536 maxAccesses
	cells := make([]int, addrCount)
	sut := func() error {
		var ready, flag atomic.Int32
		done := make(chan struct{})
		go func() {
			if ready.Load() == 1 {
				flag.Store(1)
			}
			close(done)
		}()
		for i := range cells {
			cells[i] = i
		}
		ready.Store(1)
		seen := flag.Load()
		<-done
		if seen == 1 {
			return errors.New("sut failed")
		}
		return nil
	}

	const customMaxAccesses = 1 << 18
	res := ExploreWith(1, ExploreOptions{
		Mode:        DPOR,
		MaxAccesses: customMaxAccesses,
	}, sut)
	if len(res.Failures) == 0 {
		t.Fatalf("expected failure in ExploreWith, got %+v", res)
	}
	f := res.Failures[0]
	if f.MaxAccesses != customMaxAccesses {
		t.Fatalf("Failure.MaxAccesses = %d, want %d", f.MaxAccesses, customMaxAccesses)
	}
	msgs, _ := formatExploreReport(1, res)
	if len(msgs) == 0 || !strings.Contains(msgs[0], fmt.Sprintf("MaxAccesses: %d", customMaxAccesses)) {
		t.Fatalf("formatExploreReport missing MaxAccesses budget: %v", msgs)
	}
	if Replay(1, f, sut).Err == nil {
		t.Fatalf("Replay did not reproduce failure under custom MaxAccesses budget")
	}
}

func TestDSTExploreWaitGroupMisuseAndHB(t *testing.T) {
	if !dstRaceEnabledFP() {
		t.Skip("requires -tags dst -race")
	}
	if os.Getenv("DST_WG_MISUSE_HELPER") == "1" {
		misuse := Explore(1, DPOR, func() error {
			var wg sync.WaitGroup
			wg.Add(1)
			done1 := make(chan struct{})
			done2 := make(chan struct{})
			go func() {
				wg.Done()
				wg.Add(1)
				wg.Done()
				close(done1)
			}()
			go func() {
				wg.Wait()
				close(done2)
			}()
			<-done1
			<-done2
			return nil
		})
		fmt.Printf("WG_MISUSE_CAUGHT=%v\n", len(misuse.Failures) > 0)
		return
	}

	// 1. Completeness: concurrent Done+Add reuse racing Wait must be caught by
	// DPOR (run in a child process because the intentional WaitGroup misuse also
	// triggers TSan's race.Write/race.Read annotation, which marks testing.T failed).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDSTExploreWaitGroupMisuseAndHB$")
	cmd.Env = append(os.Environ(), "DST_WG_MISUSE_HELPER=1")
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "WG_MISUSE_CAUGHT=true") {
		t.Fatalf("DPOR missed WaitGroup Add-vs-Wait reuse failure:\n%s", out)
	}

	// 2. Happens-before parity: non-waking and waking Done() calls synchronize
	// worker writes before Wait() returns, producing zero false conflicts/races.
	hb := Explore(1, DPOR, func() error {
		var wg sync.WaitGroup
		var a, b int
		wg.Add(2)
		go func() {
			a = 10
			wg.Done()
		}()
		go func() {
			b = 20
			wg.Done()
		}()
		wg.Wait()
		if a+b != 30 {
			return errors.New("sut failed")
		}
		return nil
	})
	if len(hb.Failures) != 0 || !hb.Exhausted {
		t.Fatalf("WaitGroup HB SUT failed or truncated: %+v", hb)
	}
}

func TestDSTExploreCondLostWakeupAndBlindDowngrade(t *testing.T) {
	if dstRaceEnabledFP() {
		res := Explore(1, DPOR, func() error {
			var mu sync.Mutex
			cond := sync.NewCond(&mu)
			var ready atomic.Bool
			done := make(chan struct{})
			go func() {
				if !ready.Load() {
					mu.Lock()
					cond.Wait()
					mu.Unlock()
				}
				close(done)
			}()
			go func() {
				ready.Store(true)
				cond.Signal()
			}()
			<-done
			return nil
		})
		foundDeadlock := false
		for _, f := range res.Failures {
			if f.Deadlock != "" {
				foundDeadlock = true
				break
			}
		}
		if !foundDeadlock {
			t.Fatalf("DPOR missed sync.Cond lost-wakeup deadlock: %+v", res)
		}
		return
	}

	// Non-race build: parking in Cond.Wait must trip SyncBlind -> Uninstrumented.
	res := Explore(1, DPOR, func() error {
		var mu sync.Mutex
		cond := sync.NewCond(&mu)
		parked := make(chan struct{})
		done := make(chan struct{})
		mu.Lock()
		go func() {
			mu.Lock()
			close(parked)
			cond.Wait()
			mu.Unlock()
			close(done)
		}()
		mu.Unlock()
		<-parked
		for range 10 {
			runtime.Gosched()
		}
		mu.Lock()
		cond.Signal()
		mu.Unlock()
		<-done
		return nil
	})
	if !res.Uninstrumented || res.Exhausted {
		t.Fatalf("norace Cond.Wait SUT: Uninstrumented=%v Exhausted=%v, want true/false", res.Uninstrumented, res.Exhausted)
	}
}

func TestDSTForeignSynctestBubbleClockAndTimerIsolation(t *testing.T) {
	// checkNonDurableMutex verifies upstream testing/synctest semantics on a
	// foreign bubble:
	//  1. sync.Mutex.Lock is non-durably blocking (synctest.Wait waits for an
	//     external holder rather than returning early).
	//  2. Mutex starvation uses real runtime nanotime (>1ms), not DST's
	//     64-wake virtual threshold.
	checkNonDurableMutex := func(checkStarvation bool) error {
		var mu sync.Mutex
		var childQueued, acquired, releaseChild atomic.Bool
		mu.Lock()
		releaseDone := make(chan struct{})
		var stealErr error
		go func() {
			defer close(releaseDone)
			for !childQueued.Load() {
				runtime.Gosched()
			}
			// Give the child >1ms of real wait time parked on mu.sema.
			time.Sleep(3 * time.Millisecond)
			if checkStarvation {
				// Wake #1 in normal mode (handoff=false): under Run's GOMAXPROCS=1,
				// immediate TryLock steals mu back, forcing the woken child through
				// lockSlow's starvation check with wait time >= 3ms > 1ms.
				mu.Unlock()
				if mu.TryLock() {
					// Let the woken child observe the steal, set mutexStarving, and re-park.
					time.Sleep(2 * time.Millisecond)
					// Wake #2: mutexStarving must now hand ownership directly to the child,
					// forbidding another TryLock steal.
					mu.Unlock()
					if mu.TryLock() {
						stealErr = fmt.Errorf("TryLock stole mutex after >1ms wait; virtual starvation leaked into foreign synctest bubble")
						releaseChild.Store(true)
						mu.Unlock()
						return
					}
					releaseChild.Store(true)
					return
				}
			} else {
				mu.Unlock()
			}
			releaseChild.Store(true)
		}()
		var err error
		synctest.Run(func() {
			go func() {
				childQueued.Store(true)
				mu.Lock()
				acquired.Store(true)
				for !releaseChild.Load() {
					runtime.Gosched()
				}
				mu.Unlock()
			}()
			synctest.Wait()
			if !acquired.Load() {
				err = fmt.Errorf("synctest.Wait returned while child was still blocked in sync.Mutex.Lock")
			}
		})
		<-releaseDone
		if err != nil {
			return err
		}
		return stealErr
	}

	// 1. Standalone synctest.Run outside any simulation run must not route
	// sync.Mutex contention or timers through DST simulation hooks.
	if err := checkNonDurableMutex(false); err != nil {
		t.Fatalf("standalone synctest: %v", err)
	}

	// 2. Foreign synctest.Run executing concurrently with an active simulation
	// run whose host 0 and host 1 ("h0") both carry +5h skew and -20% drift.
	var simLive, foreignChecked atomic.Bool
	foreignErr := make(chan error, 1)

	go func() {
		for !simLive.Load() {
			runtime.Gosched()
		}
		oldHost, oldProc := dstSetNode(1, 0)
		defer dstSetNode(oldHost, oldProc)
		runErr := checkNonDurableMutex(true)
		if runErr == nil {
			synctest.Run(func() {
				t0 := time.Now().UTC()
				if t0.Year() != 2000 || t0.Hour() != 0 {
					runErr = fmt.Errorf("foreign synctest Now().UTC() = %v, leaked sim host skew", t0)
					return
				}
				if off := dstClockOffsetNowFP(); off != 0 {
					runErr = fmt.Errorf("foreign synctest dstClockOffsetNow() = %v, want 0", time.Duration(off))
					return
				}
				tm := time.NewTimer(10 * time.Millisecond)
				<-tm.C
				if got := time.Since(t0); got != 10*time.Millisecond {
					runErr = fmt.Errorf("foreign synctest timer elapsed = %v, want 10ms (drift leaked)", got)
				}
			})
		}
		foreignChecked.Store(true)
		foreignErr <- runErr
	}()

	Run(1, func() {
		dstStepHostClock(0, int64(5*time.Hour))
		dstDriftHostClock(0, -200_000_000)
		HostWith("h0", HostConfig{
			Clock: Skew(5 * time.Hour),
		}, func() {
			DriftClock("h0", -200_000_000)
			simLive.Store(true)
			for !foreignChecked.Load() {
				runtime.Gosched()
			}
			if got := dstFakeTimersCountFP(); got != 0 {
				t.Fatalf("foreign synctest timer leaked into dstFakeTimers: count = %d, want 0", got)
			}
			tm := time.NewTimer(15 * time.Millisecond)
			<-tm.C
		})
		if got := dstFakeTimersCountFP(); got == 0 {
			t.Fatalf("simulation fake timer was not registered in dstFakeTimers")
		}
	})
	if err := <-foreignErr; err != nil {
		t.Fatal(err)
	}
}

func TestDSTKernelSectionSuppressesHandoffSemreleaseYield(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release func(*uint32)
	}{
		{"semrelease1", dstSemreleaseHandoffFP},
		{"dstSemrelease1", dstSemreleaseDSTHandoffFP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Explore(1, Exhaustive, func() error {
				var km kernelMutex
				var sema uint32
				waiterDone := make(chan struct{})
				victimReady := make(chan struct{})
				go func() {
					dstSemacquireFP(&sema)
					close(waiterDone)
				}()
				synctest.Wait()
				go Process("handoff_victim", func() {
					close(victimReady)
					km.Lock()
					tc.release(&sema)
					km.Unlock()
					select {}
				})
				<-victimReady
				CrashProcess("", "handoff_victim")
				km.Lock()
				km.Unlock()
				// If victim died before releasing sema, unblock waiter so bubble exits.
				dstSemreleaseHandoffFP(&sema)
				<-waiterDone
				return nil
			})
			if len(res.Failures) != 0 || !res.Exhausted {
				t.Fatalf("%s: Exhausted=%v Failures=%+v, want true/none", tc.name, res.Exhausted, res.Failures)
			}
		})
	}

	// Sanity-check kernel allocations crossing Options.MemoryLimit under Crash
	// (gcStart disassociates via dstGCInternal, which firstSystemG resumes
	// transparently ahead of other simulation goroutines).
	for seed := uint64(1); seed <= 8; seed++ {
		RunWith(seed, Options{
			MemoryLimit: 4 << 10,
			Strategy:    Random,
		}, func() {
			ready := make(chan struct{})
			go Process("alloc_victim", func() {
				for i := range 320 {
					_ = os.Setenv(fmt.Sprintf("MEM_K_%d", i), "val")
				}
				go func() {
					close(ready)
					for range 8 {
						_ = os.Environ()
					}
				}()
				select {}
			})
			<-ready
			for range int(seed % 4) {
				runtime.Gosched()
			}
			CrashProcess("", "alloc_victim")
			_ = os.Environ()
			if err := os.Setenv("SURVIVOR_ENV", "ok"); err != nil {
				t.Fatalf("seed %d: survivor Setenv failed: %v", seed, err)
			}
		})
	}
}

func TestDSTKernelMutexDoesNotForgeHBOrConflicts(t *testing.T) {
	if os.Getenv("DST_KERNEL_RACE_HELPER") == "1" {
		res := Explore(1, DPOR, func() error {
			var shared int
			done1 := make(chan struct{})
			done2 := make(chan struct{})
			go Process("p1", func() {
				shared = 42
				_, _ = os.Stat("/unrelated_a")
				_, _ = os.LookupEnv("DST_UNRELATED")
				close(done1)
			})
			go Process("p2", func() {
				_, _ = os.LookupEnv("DST_UNRELATED")
				_, _ = os.Stat("/unrelated_b")
				_ = shared
				close(done2)
			})
			<-done1
			<-done2
			return nil
		})
		fmt.Printf("KERNEL_NO_FALSE_HB_CAUGHT=%v\n", len(res.Failures) > 0)
		return
	}
	if os.Getenv("DST_USER_BUF_RACE_HELPER") == "1" {
		res := Explore(1, DPOR, func() error {
			if err := os.WriteFile("/shared_file", []byte("hello world"), 0o644); err != nil {
				panic(err)
			}
			f, err := os.Open("/shared_file")
			if err != nil {
				panic(err)
			}
			defer f.Close()
			buf := make([]byte, 16)
			done1 := make(chan struct{})
			done2 := make(chan struct{})
			go func() {
				_, _ = f.Read(buf)
				close(done1)
			}()
			go func() {
				buf[0] = 0x7f
				close(done2)
			}()
			<-done1
			<-done2
			return nil
		})
		fmt.Printf("USER_BUF_RACE_CAUGHT=%v\n", len(res.Failures) > 0)
		return
	}

	// 1a. Cross-host independent kernel operations must not create universe-global
	// DPOR conflicts (exact 1 schedule when each host only touches its own FS/env).
	res := Explore(1, DPOR, func() error {
		go Host("h1", func() {
			Process("p1", func() {
				_ = os.Setenv("ISOLATED_KEY", "v1")
				if os.Getenv("ISOLATED_KEY") != "v1" {
					panic("unexpected env in p1")
				}
				if err := os.WriteFile("/local", []byte("h1"), 0o644); err != nil {
					panic(err)
				}
			})
		})
		go Host("h2", func() {
			Process("p2", func() {
				_ = os.Setenv("ISOLATED_KEY", "v2")
				if os.Getenv("ISOLATED_KEY") != "v2" {
					panic("unexpected env in p2")
				}
				if err := os.WriteFile("/local", []byte("h2"), 0o644); err != nil {
					panic(err)
				}
			})
		})
		synctest.Wait()
		return nil
	})
	if len(res.Failures) != 0 || !res.Exhausted || res.Schedules != 1 {
		t.Fatalf("cross-host isolated env/FS SUT: Schedules=%d Exhausted=%v Failures=%v, want 1/true/none",
			res.Schedules, res.Exhausted, res.Failures)
	}

	// 1b. Within a single process, concurrent Setenv and Getenv share one
	// environment table and must explore both outcomes ("" and "set").
	seenEmpty, seenSet := false, false
	sameProcRes := Explore(1, DPOR, func() error {
		Process("shared_env_proc", func() {
			go func() {
				_ = os.Setenv("SAME_PROC_KEY", "set")
			}()
			go func() {
				if os.Getenv("SAME_PROC_KEY") == "set" {
					seenSet = true
				} else {
					seenEmpty = true
				}
			}()
			synctest.Wait()
		})
		return nil
	})
	if len(sameProcRes.Failures) != 0 || !seenEmpty || !seenSet {
		t.Fatalf("same-process Setenv/Getenv exploration: seenEmpty=%v seenSet=%v Schedules=%d Failures=%v",
			seenEmpty, seenSet, sameProcRes.Schedules, sameProcRes.Failures)
	}

	if !dstRaceEnabledFP() {
		return
	}

	// 2. Under -race, unrelated OS/env calls touching KernelMutex must NOT forge
	// TSan happens-before edges that hide an unsynchronized user data race.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDSTKernelMutexDoesNotForgeHBOrConflicts$")
	cmd.Env = append(os.Environ(), "DST_KERNEL_RACE_HELPER=1")
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "KERNEL_NO_FALSE_HB_CAUGHT=true") {
		t.Fatalf("KernelMutex forged TSan HB and masked user data race:\n%s", out)
	}

	// 3. Under -race, racing a caller []byte buffer passed to modeled File.Read
	// must still be caught even though the internal copy runs inside KernelMutex.
	cmdBuf := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDSTKernelMutexDoesNotForgeHBOrConflicts$")
	cmdBuf.Env = append(os.Environ(), "DST_USER_BUF_RACE_HELPER=1")
	outBuf, _ := cmdBuf.CombinedOutput()
	if !strings.Contains(string(outBuf), "USER_BUF_RACE_CAUGHT=true") {
		t.Fatalf("modeled File.Read failed to report race on caller buffer:\n%s", outBuf)
	}
}

func TestDSTWorldRestartAndEndIdempotentOnDownHost(t *testing.T) {
	RunWith(43, Options{CrashTear: true}, func() {
		bootCount := 0
		var bootedPlatter []byte
		ctl := StartWorld([]HostDecl{{
			Name: "h1",
			Boot: func() {
				bootCount++
				ready := make(chan struct{})
				go Process("writer", func() {
					defer close(ready)
					if bootCount == 1 {
						f, err := os.OpenFile("/data", os.O_CREATE|os.O_RDWR, 0o644)
						if err != nil {
							panic(err)
						}
						zeros := make([]byte, 32*4096)
						if _, err := f.Write(zeros); err != nil {
							panic(err)
						}
						if err := f.Sync(); err != nil {
							panic(err)
						}
						if d, err := os.Open("/"); err == nil {
							_ = d.Sync()
							_ = d.Close()
						}
						// Dirty 32 pages without fsync so CrashTear produces a
						// non-trivial torn platter image on the first power loss.
						ones := bytes.Repeat([]byte{0xAB}, 32*4096)
						if _, err := f.WriteAt(ones, 0); err != nil {
							panic(err)
						}
						_ = f.Close()
						return
					}
					b, err := os.ReadFile("/data")
					if err != nil {
						panic(err)
					}
					bootedPlatter = b
				})
				<-ready
			},
		}})

		CrashHost("h1")
		afterFirstCrash, err := fs.ReadFile(HostFS("h1"), "data")
		if err != nil {
			t.Fatalf("HostFS ReadFile after first CrashHost: %v", err)
		}

		// RestartHost calls CrashHost("h1") internally; on an already-down host
		// that must be a strict no-op (must NOT re-tear the platter a second time).
		ctl.RestartHost("h1")
		if !bytes.Equal(bootedPlatter, afterFirstCrash) {
			t.Fatalf("RestartHost re-tore already-down host platter (got %d surviving 0xAB bytes, want %d)",
				bytes.Count(bootedPlatter, []byte{0xAB}),
				bytes.Count(afterFirstCrash, []byte{0xAB}))
		}

		// Power off again, snapshot platter, and verify ctl.End() is also a no-op
		// on an already-down host.
		Host("h1", func() {
			done := make(chan struct{})
			go Process("dirty2", func() {
				defer close(done)
				if err := os.WriteFile("/data", bytes.Repeat([]byte{0xCD}, 32*4096), 0o644); err != nil {
					panic(err)
				}
			})
			<-done
		})
		CrashHost("h1")
		beforeEnd, err := fs.ReadFile(HostFS("h1"), "data")
		if err != nil {
			t.Fatalf("HostFS ReadFile before End: %v", err)
		}
		ctl.End()
		afterEnd, err := fs.ReadFile(HostFS("h1"), "data")
		if err != nil {
			t.Fatalf("HostFS ReadFile after End: %v", err)
		}
		if !bytes.Equal(afterEnd, beforeEnd) {
			t.Fatalf("Ctl.End re-tore already-down host platter")
		}
	})
}

func TestDSTUserBufCopyOutsideKernelLocks(t *testing.T) {
	if helper := os.Getenv("DST_MULTICHUNK_RACE_HELPER"); helper != "" {
		switch helper {
		case "pipe":
			res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 64}, func() error {
				r, w, err := os.Pipe()
				if err != nil {
					panic(err)
				}
				defer r.Close()
				defer w.Close()
				buf := bytes.Repeat([]byte{'A'}, 128*1024)
				done := make(chan struct{})
				go func() {
					_, _ = w.Write(buf)
					close(done)
				}()
				tmp := make([]byte, 64*1024)
				if _, err := r.Read(tmp); err != nil {
					panic(err)
				}
				// Mutate second chunk concurrently while writer wakes to copy buf[64KiB:].
				buf[64*1024] = 'X'
				_, _ = r.Read(tmp)
				<-done
				return nil
			})
			fmt.Printf("PIPE_MULTICHUNK_RACE_CAUGHT=%v\n", len(res.Failures) > 0)
		case "wire":
			res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 8}, func() error {
				dstSetNetSendBuffer(4096)
				ln, err := net.Listen("tcp", "127.0.0.1:9201")
				if err != nil {
					panic(err)
				}
				defer ln.Close()
				srvCh := make(chan net.Conn, 1)
				go func() {
					c, err := ln.Accept()
					if err != nil {
						panic(err)
					}
					srvCh <- c
				}()
				cli, err := net.Dial("tcp", "127.0.0.1:9201")
				if err != nil {
					panic(err)
				}
				defer cli.Close()
				srv := <-srvCh
				defer srv.Close()

				buf := bytes.Repeat([]byte{'B'}, 3*4096)
				done := make(chan struct{})
				go func() {
					_, _ = cli.Write(buf)
					close(done)
				}()
				tmp := make([]byte, 4096)
				if _, err := srv.Read(tmp); err != nil {
					panic(err)
				}
				// Mutate third chunk BEFORE draining second chunk: srv.Read must
				// not publish a reverse happens-before edge (srv.Read -> cli.Write)
				// through send-buffer space wakeups.
				buf[2*4096] = 'Y'
				if _, err := srv.Read(tmp); err != nil {
					panic(err)
				}
				_, _ = srv.Read(tmp)
				<-done
				return nil
			})
			fmt.Printf("WIRE_MULTICHUNK_RACE_CAUGHT=%v\n", len(res.Failures) > 0)
		case "dialer":
			res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 64}, func() error {
				ln, err := net.Listen("tcp", "127.0.0.1:9202")
				if err != nil {
					panic(err)
				}
				defer ln.Close()
				var d net.Dialer
				done1 := make(chan struct{})
				done2 := make(chan struct{})
				go func() {
					d.KeepAlive = 5 * time.Second
					close(done1)
				}()
				go func() {
					if c, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:9202"); err == nil {
						_ = c.Close()
					}
					close(done2)
				}()
				<-done1
				<-done2
				return nil
			})
			fmt.Printf("DIALER_KEEPALIVE_RACE_CAUGHT=%v\n", len(res.Failures) > 0)
		case "read_l2":
			seenReadOverwrote, seenMutateOverwrote := false, false
			_ = ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 64}, func() error {
				ln, err := net.Listen("tcp", "127.0.0.1:9204")
				if err != nil {
					panic(err)
				}
				defer ln.Close()
				cli, err := net.Dial("tcp", "127.0.0.1:9204")
				if err != nil {
					panic(err)
				}
				defer cli.Close()
				srv, err := ln.Accept()
				if err != nil {
					panic(err)
				}
				if _, err := srv.Write([]byte("hello")); err != nil {
					panic(err)
				}
				_ = srv.Close()
				buf := make([]byte, 5)
				done1 := make(chan struct{})
				done2 := make(chan struct{})
				go func() {
					_, _ = cli.Read(buf)
					close(done1)
				}()
				go func() {
					buf[0] = 0x7f
					close(done2)
				}()
				<-done1
				<-done2
				switch buf[0] {
				case 'h':
					seenReadOverwrote = true
				case 0x7f:
					seenMutateOverwrote = true
				}
				return nil
			})
			fmt.Printf("READ_L2_BOTH_ORDERS=%v\n", seenReadOverwrote && seenMutateOverwrote)
		}
		return
	}

	// 1. MAP_SHARED synchronous mapping-fault teardown during Read/Write on
	// os.File, procfs, os.Pipe, and net.Conn must never self-deadlock on held
	// KernelMutex locks, and surviving operations must remain functional.
	Run(53, func() {
		Host("h1", func() {
			if err := os.WriteFile("/backing", bytes.Repeat([]byte("m"), 4096), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile("/src", []byte("hello world"), 0o644); err != nil {
				t.Fatal(err)
			}
			runTrapProc := func(name string, fn func(roSlice, noneSlice []byte)) {
				go Process(name, func() {
					mf, err := os.OpenFile("/backing", os.O_RDWR, 0)
					if err != nil {
						panic(err)
					}
					ro, err := syscall.Mmap(int(mf.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
					if err != nil {
						panic(err)
					}
					none, err := syscall.Mmap(int(mf.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
					if err != nil {
						panic(err)
					}
					if err := syscall.Mprotect(none, syscall.PROT_NONE); err != nil {
						panic(err)
					}
					fn(ro[:8], none[:8])
				})
				synctest.Wait()
			}

			runTrapProc("trap_file_read", func(ro, _ []byte) {
				f, _ := os.Open("/src")
				_, _ = f.Read(ro)
			})
			runTrapProc("trap_file_write", func(_, none []byte) {
				f, _ := os.OpenFile("/src", os.O_RDWR, 0)
				_, _ = f.Write(none)
			})
			runTrapProc("trap_proc_read", func(ro, _ []byte) {
				pf, _ := os.Open("/proc/sys/kernel/random/boot_id")
				_, _ = pf.Read(ro)
			})
			runTrapProc("trap_pipe_read", func(ro, _ []byte) {
				pr, pw, _ := os.Pipe()
				_, _ = pw.Write([]byte("abc"))
				_, _ = pr.Read(ro)
			})
			runTrapProc("trap_pipe_write", func(_, none []byte) {
				pr, pw, _ := os.Pipe()
				defer pr.Close()
				_, _ = pw.Write(none)
			})

			ln, err := net.Listen("tcp", "127.0.0.1:9200")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					_, _ = c.Write([]byte("wiredata"))
					_ = c.Close()
				}
			}()
			runTrapProc("trap_wire_read", func(ro, _ []byte) {
				c, err := net.Dial("tcp", "127.0.0.1:9200")
				if err != nil {
					panic(err)
				}
				_, _ = c.Read(ro)
			})
			runTrapProc("trap_wire_write", func(_, none []byte) {
				c, err := net.Dial("tcp", "127.0.0.1:9200")
				if err != nil {
					panic(err)
				}
				_, _ = c.Write(none)
			})

			// Verify all kernel locks (dstFS.mu, proc, pipe, net) remain unheld and operable.
			if got, err := os.ReadFile("/src"); err != nil || string(got) != "hello world" {
				t.Fatalf("post-trap ReadFile = (%q, %v), want hello world", got, err)
			}
			pr, pw, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			_, _ = pw.Write([]byte("ok"))
			_ = pw.Close()
			pbuf := make([]byte, 2)
			if _, err := pr.Read(pbuf); err != nil || string(pbuf) != "ok" {
				t.Fatalf("post-trap pipe Read = (%q, %v), want ok", pbuf, err)
			}
			_ = pr.Close()
		})
	})

	// 2. Recovered nil-backed slice panics must never strand KernelMutex or leak
	// dstKernelDepth / TSan ignore state.
	Run(59, func() {
		raw := struct {
			p *byte
			n int
			c int
		}{nil, 8, 8}
		bad := *(*[]byte)(unsafe.Pointer(&raw))
		mustRecover := func(name string, fn func()) {
			panicked := false
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				fn()
			}()
			if !panicked {
				t.Fatalf("%s: expected panic on nil-backed slice", name)
			}
		}
		if err := os.WriteFile("/panic_f", []byte("abcdefgh"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, _ := os.OpenFile("/panic_f", os.O_RDWR, 0)
		defer f.Close()
		mustRecover("file.Read", func() { _, _ = f.Read(bad) })
		mustRecover("file.Write", func() { _, _ = f.Write(bad) })

		pf, _ := os.Open("/proc/sys/kernel/random/boot_id")
		defer pf.Close()
		mustRecover("proc.Read", func() { _, _ = pf.Read(bad) })

		pr, pw, _ := os.Pipe()
		defer pr.Close()
		defer pw.Close()
		_, _ = pw.Write([]byte("12345678"))
		mustRecover("pipe.Read", func() { _, _ = pr.Read(bad) })
		mustRecover("pipe.Write", func() { _, _ = pw.Write(bad) })

		ln, err := net.Listen("tcp", "127.0.0.1:9203")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		srvHold := make(chan struct{})
		defer close(srvHold)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("12345678"))
			<-srvHold
			_ = c.Close()
		}()
		cli, err := net.Dial("tcp", "127.0.0.1:9203")
		if err != nil {
			t.Fatal(err)
		}
		defer cli.Close()
		mustRecover("conn.Read", func() { _, _ = cli.Read(bad) })
		mustRecover("conn.Write", func() { _, _ = cli.Write(bad) })

		// Subsequent normal operations on the same goroutine must succeed cleanly.
		if _, err := os.ReadFile("/panic_f"); err != nil {
			t.Fatalf("ReadFile after recovered slice panics failed: %v", err)
		}
	})

	if !dstRaceEnabledFP() {
		return
	}

	// 3. Spawning a goroutine inside a KernelMutex critical section must preserve
	// the parent->child `go`-spawn TSan happens-before vector clock, and racing
	// host *os.File Close against Read outside a simulation must not race on
	// internal/poll.FD.Sysfd.
	var parentState [1]int
	parentState[0] = 1
	parentState[0] = 42
	var kmu isync.KernelMutex
	spawnDone := make(chan struct{})
	kmu.Lock()
	go func(p *[1]int) {
		if p[0] != 42 {
			panic("unexpected parentState")
		}
		close(spawnDone)
	}(&parentState)
	kmu.Unlock()
	<-spawnDone

	for range 64 {
		hr, hw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			var b [1]byte
			_, _ = hr.Read(b[:])
		}()
		go func() {
			defer wg.Done()
			_ = hr.Close()
		}()
		_ = hr.Close()
		_ = hw.Close()
		wg.Wait()
	}

	// 4. Level-2 DPOR pre-access yield completeness on Read buffers (read_l2) and
	// TSan detection of multi-chunk pipe.Write / conn.Write and Dialer.KeepAlive
	// races under -race.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, tc := range []struct {
		helper string
		want   string
	}{
		{"read_l2", "READ_L2_BOTH_ORDERS=true"},
		{"pipe", "PIPE_MULTICHUNK_RACE_CAUGHT=true"},
		{"wire", "WIRE_MULTICHUNK_RACE_CAUGHT=true"},
		{"dialer", "DIALER_KEEPALIVE_RACE_CAUGHT=true"},
	} {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDSTUserBufCopyOutsideKernelLocks$")
		cmd.Env = append(os.Environ(), "DST_MULTICHUNK_RACE_HELPER="+tc.helper)
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), tc.want) {
			t.Fatalf("helper %s failed to detect caller race (want %s):\n%s", tc.helper, tc.want, out)
		}
	}
}

func TestDSTKernelSyncAtomicity(t *testing.T) {
	// 1. Concurrent Write and Close on the same net.Conn must serialize FIN vs.
	// segment commit atomically under the stream lock so peer Read never observes
	// data after io.EOF.
	eofOrderRes := Explore(1, DPOR, func() error {
		ln, err := net.Listen("tcp", "127.0.0.1:9205")
		if err != nil {
			panic(err)
		}
		defer ln.Close()
		cli, err := net.Dial("tcp", "127.0.0.1:9205")
		if err != nil {
			panic(err)
		}
		srv, err := ln.Accept()
		if err != nil {
			panic(err)
		}
		defer srv.Close()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = cli.Write([]byte("payload"))
		}()
		go func() {
			defer wg.Done()
			_ = cli.Close()
		}()
		var buf [16]byte
		sawEOF := false
		for range 4 {
			n, rerr := srv.Read(buf[:])
			if sawEOF && (n > 0 || rerr != io.EOF) {
				return errors.New("sut failed")
			}
			if rerr == io.EOF {
				sawEOF = true
			}
		}
		wg.Wait()
		return nil
	})
	if len(eofOrderRes.Failures) != 0 {
		t.Fatalf("concurrent Write+Close violated monotonic EOF on peer Read: %+v", eofOrderRes.Failures)
	}

	// 2. Non-race and -race coarse DPOR exploration on net.Conn concurrent writes
	// must explore both segment orders ("AB" and "BA") via dstCoarseDep.
	sawAB, sawBA := false, false
	wireCoarseRes := Explore(1, DPOR, func() error {
		ln, err := net.Listen("tcp", "127.0.0.1:9208")
		if err != nil {
			panic(err)
		}
		defer ln.Close()
		cli, err := net.Dial("tcp", "127.0.0.1:9208")
		if err != nil {
			panic(err)
		}
		srv, err := ln.Accept()
		if err != nil {
			panic(err)
		}
		defer srv.Close()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = cli.Write([]byte("A"))
		}()
		go func() {
			defer wg.Done()
			_, _ = cli.Write([]byte("B"))
		}()
		wg.Wait()
		_ = cli.Close()
		got, err := io.ReadAll(srv)
		if err != nil {
			panic(err)
		}
		switch string(got) {
		case "AB":
			sawAB = true
		case "BA":
			sawBA = true
		}
		return nil
	})
	if len(wireCoarseRes.Failures) != 0 || !wireCoarseRes.Exhausted || !sawAB || !sawBA {
		t.Fatalf("net.Conn coarse DPOR write ordering: sawAB=%v sawBA=%v Exhausted=%v Failures=%+v",
			sawAB, sawBA, wireCoarseRes.Exhausted, wireCoarseRes.Failures)
	}

	// 3. Kernel broadcast wakeups (e.g. SetReadDeadline bumping a pipe's
	// generation channel inside KernelMutex) must NOT record a spurious
	// readier->readied DPOR happens-before edge that suppresses reordering
	// against an independent real waker.
	sawPipeUnset := false
	pipeDeadlineRes := Explore(1, DPOR, func() error {
		_ = os.Unsetenv("PIPE_DEADLINE_K")
		pr, pw, err := os.Pipe()
		if err != nil {
			panic(err)
		}
		defer pr.Close()
		defer pw.Close()

		readDone := make(chan struct{})
		go func() {
			var b [1]byte
			_, rerr := pr.Read(b[:])
			if rerr == nil && os.Getenv("PIPE_DEADLINE_K") == "" {
				sawPipeUnset = true
			}
			close(readDone)
		}()
		synctest.Wait()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = os.Setenv("PIPE_DEADLINE_K", "1")
			_ = pr.SetReadDeadline(time.Now().Add(time.Hour))
		}()
		go func() {
			defer wg.Done()
			_, _ = pw.Write([]byte{1})
		}()
		<-readDone
		wg.Wait()
		return nil
	})
	if len(pipeDeadlineRes.Failures) != 0 || !pipeDeadlineRes.Exhausted || !sawPipeUnset {
		t.Fatalf("pipe SetReadDeadline kernel broadcast leaked DPOR HB: sawPipeUnset=%v Schedules=%d Exhausted=%v Failures=%+v",
			sawPipeUnset, pipeDeadlineRes.Schedules, pipeDeadlineRes.Exhausted, pipeDeadlineRes.Failures)
	}

	// 4. Shared MAP_SHARED futex wakes must transfer DPOR happens-before per
	// woken waiter (&w.raceSync), never through a shared futex-key object clock
	// where a second waker waking W2 would pollute W1's clock.
	sawFutexUnset := false
	futexPerWaiterRes := Explore(1, DPOR, func() error {
		_ = os.Unsetenv("FUTEX_WAKER2_K")
		if err := os.WriteFile("/futex_hb", make([]byte, 4096), 0o644); err != nil {
			panic(err)
		}
		f, err := os.OpenFile("/futex_hb", os.O_RDWR, 0)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		mem, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			panic(err)
		}
		defer syscall.Munmap(mem)
		word := (*uint32)(unsafe.Pointer(&mem[0]))

		startWaker2 := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(3)
		// Spawn Waker2 first so it has a lower dstSeq than W1, making Schedule 1
		// execute Waker2 before W1 after G0 wakes both.
		go func() {
			defer wg.Done()
			<-startWaker2
			_ = os.Setenv("FUTEX_WAKER2_K", "1")
			_, _, _ = syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(word)), 1, 1, 0, 0, 0)
		}()
		synctest.Wait()

		go func() {
			defer wg.Done()
			_, _, errno := syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(word)), 0, 0, 0, 0, 0)
			if errno == 0 && os.Getenv("FUTEX_WAKER2_K") == "" {
				sawFutexUnset = true
			}
		}()
		synctest.Wait()

		go func() {
			defer wg.Done()
			_, _, _ = syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(word)), 0, 0, 0, 0, 0)
		}()
		synctest.Wait()

		close(startWaker2)
		atomic.StoreUint32(word, 1)
		_, _, _ = syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(word)), 1, 1, 0, 0, 0)
		wg.Wait()
		return nil
	})
	if len(futexPerWaiterRes.Failures) != 0 || !futexPerWaiterRes.Exhausted || !sawFutexUnset {
		t.Fatalf("futex shared key leaked DPOR HB from Waker2 into W1: sawFutexUnset=%v Schedules=%d Exhausted=%v Failures=%+v",
			sawFutexUnset, futexPerWaiterRes.Schedules, futexPerWaiterRes.Exhausted, futexPerWaiterRes.Failures)
	}

	if !dstRaceEnabledFP() {
		return
	}

	// 2. Intentional user-level synchronization across modeled net.Conn,
	// os.Pipe, and os.File must update TSan vector clocks atomically inside
	// KernelMutex so valid Write->Read message passing produces zero false
	// data-race reports under Level-2 exploration.
	hbMsgRes := Explore(1, DPOR, func() error {
		var wireMsg, pipeMsg, fileMsg int

		ln, err := net.Listen("tcp", "127.0.0.1:9206")
		if err != nil {
			panic(err)
		}
		defer ln.Close()
		cli, err := net.Dial("tcp", "127.0.0.1:9206")
		if err != nil {
			panic(err)
		}
		defer cli.Close()
		srv, err := ln.Accept()
		if err != nil {
			panic(err)
		}
		defer srv.Close()

		pr, pw, err := os.Pipe()
		if err != nil {
			panic(err)
		}
		defer pr.Close()
		defer pw.Close()

		wf, err := os.OpenFile("/hb_msg", os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			panic(err)
		}
		defer wf.Close()
		rf, err := os.Open("/hb_msg")
		if err != nil {
			panic(err)
		}
		defer rf.Close()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			wireMsg = 11
			_, _ = cli.Write([]byte{1})
			pipeMsg = 22
			_, _ = pw.Write([]byte{1})
			fileMsg = 33
			_, _ = wf.Write([]byte{1})
		}()
		go func() {
			defer wg.Done()
			var b [1]byte
			if n, _ := srv.Read(b[:]); n == 1 && wireMsg != 11 {
				panic("bad wireMsg")
			}
			if n, _ := pr.Read(b[:]); n == 1 && pipeMsg != 22 {
				panic("bad pipeMsg")
			}
			if n, _ := rf.ReadAt(b[:], 0); n == 1 && fileMsg != 33 {
				panic("bad fileMsg")
			}
		}()
		wg.Wait()
		return nil
	})
	if len(hbMsgRes.Failures) != 0 || !hbMsgRes.Exhausted {
		t.Fatalf("atomic in-kernel HB message-passing SUT failed: Exhausted=%v Failures=%+v", hbMsgRes.Exhausted, hbMsgRes.Failures)
	}

	defer dstFilterForceConservativeFP(false)
	fileAtomicityRes := ExploreWith(1, ExploreOptions{Mode: Exhaustive, MaxSchedules: 128}, func() error {
		wf, err := os.OpenFile("/hb_atomic", os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			panic(err)
		}
		defer wf.Close()
		rf, err := os.Open("/hb_atomic")
		if err != nil {
			panic(err)
		}
		defer rf.Close()

		dstFilterForceConservativeFP(true)
		var fileMsg int
		done := make(chan struct{})
		go func() {
			dstFilterForceConservativeFP(false)
			var b [1]byte
			if n, _ := rf.ReadAt(b[:], 0); n == 1 && fileMsg != 33 {
				panic("bad fileMsg")
			}
			close(done)
		}()
		fileMsg = 33
		_, _ = wf.Write([]byte{1})
		<-done
		return nil
	})
	if len(fileAtomicityRes.Failures) != 0 {
		t.Fatalf("conservative-filter File.Write->ReadAt HB atomicity failed: %+v", fileAtomicityRes.Failures)
	}

	wireEOFAtomicityRes := ExploreWith(1, ExploreOptions{Mode: Exhaustive, MaxSchedules: 128}, func() error {
		ln, err := net.Listen("tcp", "127.0.0.1:9207")
		if err != nil {
			panic(err)
		}
		defer ln.Close()
		cli, err := net.Dial("tcp", "127.0.0.1:9207")
		if err != nil {
			panic(err)
		}
		defer cli.Close()
		srv, err := ln.Accept()
		if err != nil {
			panic(err)
		}
		defer srv.Close()

		payload := []byte("payload")
		dstFilterForceConservativeFP(true)
		done := make(chan struct{})
		sawEOF := false
		go func() {
			dstFilterForceConservativeFP(false)
			_ = cli.Close()
			var buf [16]byte
			if _, rerr := srv.Read(buf[:]); rerr == io.EOF {
				sawEOF = true
			}
			close(done)
		}()
		_, _ = cli.Write(payload)
		<-done
		if sawEOF {
			var buf [16]byte
			if n, rerr := srv.Read(buf[:]); n > 0 || rerr != io.EOF {
				return errors.New("sut failed")
			}
		}
		return nil
	})
	if len(wireEOFAtomicityRes.Failures) != 0 {
		t.Fatalf("conservative-filter net.Conn Write+Close monotonic EOF failed: %+v", wireEOFAtomicityRes.Failures)
	}
}

func TestDSTKernelTeardownFlockDPORReversal(t *testing.T) {
	var sawBusy, sawGranted atomic.Bool
	res := Explore(1, DPOR, func() error {
		Host("h1", func() {
			if err := os.WriteFile("/exit_flock", []byte("x"), 0o644); err != nil {
				panic(err)
			}
			locked := make(chan struct{})
			p1Done := make(chan struct{})
			go Process("holder", func() {
				defer close(p1Done)
				f, err := os.OpenFile("/exit_flock", os.O_RDWR, 0)
				if err != nil {
					panic(err)
				}
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
					panic(err)
				}
				close(locked)
				// Return without unlocking or closing f so process-exit teardown
				// releases the flock inside its kernel critical section.
			})
			<-locked

			p2Done := make(chan struct{})
			go Process("prober", func() {
				defer close(p2Done)
				f, err := os.OpenFile("/exit_flock", os.O_RDWR, 0)
				if err != nil {
					panic(err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					sawBusy.Store(true)
				} else {
					sawGranted.Store(true)
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				}
			})
			<-p1Done
			<-p2Done
		})
		return nil
	})
	if len(res.Failures) != 0 || !res.Exhausted || !sawBusy.Load() || !sawGranted.Load() {
		t.Fatalf("process-exit flock teardown DPOR reversal: sawBusy=%v sawGranted=%v Schedules=%d Exhausted=%v Failures=%+v",
			sawBusy.Load(), sawGranted.Load(), res.Schedules, res.Exhausted, res.Failures)
	}
}

func TestDSTStreamPreBufferedReadDoesNotAcquireCloseHB(t *testing.T) {
	// First verify in both non-race and -race builds that DPOR treats a peer
	// Close after pre-buffering data as independent of pre-buffered data reads
	// while still synchronizing drained EOF reads.
	if os.Getenv("DST_PREBUF_CLOSE_RACE_HELPER") != "" {
		// MaxSchedules: 1 runs only the base sticky schedule, in which the
		// lower-dstSeq closer finishes unsynced=X + Close() completely before
		// the reader starts Read(b[:1]) + _=unsynced. If Close() falsely
		// released the data stream's raceSync word, this single schedule would
		// report zero races; with eofSync separated from raceSync, TSan catches
		// the unsynchronized access on schedule 1.
		switch os.Getenv("DST_PREBUF_CLOSE_RACE_HELPER") {
		case "wire":
			res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 1}, func() error {
				ln, err := net.Listen("tcp", "127.0.0.1:9401")
				if err != nil {
					panic(err)
				}
				defer ln.Close()
				cli, err := net.Dial("tcp", "127.0.0.1:9401")
				if err != nil {
					panic(err)
				}
				srv, err := ln.Accept()
				if err != nil {
					panic(err)
				}
				defer srv.Close()
				if _, err := cli.Write([]byte{'A'}); err != nil {
					panic(err)
				}
				var unsynced int
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					unsynced = 42
					_ = cli.Close()
				}()
				go func() {
					defer wg.Done()
					var b [1]byte
					if n, _ := srv.Read(b[:]); n == 1 {
						_ = unsynced
					}
				}()
				wg.Wait()
				return nil
			})
			fmt.Printf("PREBUF_WIRE_RACE=%v\n", len(res.Failures) > 0)
		case "pipe":
			res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 1}, func() error {
				pr, pw, err := os.Pipe()
				if err != nil {
					panic(err)
				}
				defer pr.Close()
				if _, err := pw.Write([]byte{'A'}); err != nil {
					panic(err)
				}
				var unsynced int
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					unsynced = 99
					_ = pw.Close()
				}()
				go func() {
					defer wg.Done()
					var b [1]byte
					if n, _ := pr.Read(b[:]); n == 1 {
						_ = unsynced
					}
				}()
				wg.Wait()
				return nil
			})
			fmt.Printf("PREBUF_PIPE_RACE=%v\n", len(res.Failures) > 0)
		}
		return
	}

	if !dstRaceEnabledFP() {
		return
	}

	// Subprocess probes: reading pre-buffered bytes after peer Close must NOT
	// acquire Close's happens-before clock and mask a user data race.
	for _, tc := range []struct {
		mode string
		want string
	}{
		{mode: "wire", want: "PREBUF_WIRE_RACE=true"},
		{mode: "pipe", want: "PREBUF_PIPE_RACE=true"},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDSTStreamPreBufferedReadDoesNotAcquireCloseHB$")
		cmd.Env = append(os.Environ(), "DST_PREBUF_CLOSE_RACE_HELPER="+tc.mode)
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), tc.want) {
			t.Fatalf("pre-buffered %s read falsely acquired Close HB (want %s):\n%s", tc.mode, tc.want, out)
		}
	}

	// Conversely, draining all the way to io.EOF DOES synchronize with the
	// writer's Close on both net.Conn and os.Pipe.
	eofSyncRes := Explore(1, DPOR, func() error {
		ln, err := net.Listen("tcp", "127.0.0.1:9402")
		if err != nil {
			panic(err)
		}
		defer ln.Close()
		cli, err := net.Dial("tcp", "127.0.0.1:9402")
		if err != nil {
			panic(err)
		}
		srv, err := ln.Accept()
		if err != nil {
			panic(err)
		}
		defer srv.Close()
		if _, err := cli.Write([]byte{'A'}); err != nil {
			panic(err)
		}

		pr, pw, err := os.Pipe()
		if err != nil {
			panic(err)
		}
		defer pr.Close()
		if _, err := pw.Write([]byte{'B'}); err != nil {
			panic(err)
		}

		var wireEOFMsg, pipeEOFMsg int
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			wireEOFMsg = 7
			_ = cli.Close()
			pipeEOFMsg = 8
			_ = pw.Close()
		}()
		go func() {
			defer wg.Done()
			var b [4]byte
			for {
				_, rerr := srv.Read(b[:])
				if rerr == io.EOF {
					if wireEOFMsg != 7 {
						panic("missing wire EOF HB")
					}
					break
				}
			}
			for {
				_, rerr := pr.Read(b[:])
				if rerr == io.EOF {
					if pipeEOFMsg != 8 {
						panic("missing pipe EOF HB")
					}
					break
				}
			}
		}()
		wg.Wait()
		return nil
	})
	if len(eofSyncRes.Failures) != 0 || !eofSyncRes.Exhausted {
		t.Fatalf("drained EOF failed to acquire Close HB: Exhausted=%v Failures=%+v", eofSyncRes.Exhausted, eofSyncRes.Failures)
	}
}

func TestDSTExploreCoarseDeadlinesNetHandlesCwdSockopt(t *testing.T) {
	// Keep handles open past wg.Wait() (the per-run epoch resets them) so a
	// post-Wait Close() write on closeSync cannot mask a missing conflict
	// between the two racing operations themselves in non-race builds.

	// 1a. net.Conn SetReadDeadline(past) vs Read on pre-buffered data.
	var sawWireReadOK, sawWireDeadline atomic.Bool
	wireDeadRes := Explore(1, DPOR, func() error {
		ln, err := net.Listen("tcp", "127.0.0.1:9410")
		if err != nil {
			panic(err)
		}
		cli, err := net.Dial("tcp", "127.0.0.1:9410")
		if err != nil {
			panic(err)
		}
		srv, err := ln.Accept()
		if err != nil {
			panic(err)
		}
		if _, err := cli.Write([]byte{'x'}); err != nil {
			panic(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = srv.SetReadDeadline(time.Unix(0, 1))
		}()
		go func() {
			defer wg.Done()
			var b [1]byte
			if n, rerr := srv.Read(b[:]); n == 1 && rerr == nil {
				sawWireReadOK.Store(true)
			} else if rerr != nil {
				sawWireDeadline.Store(true)
			}
		}()
		wg.Wait()
		return nil
	})
	if len(wireDeadRes.Failures) != 0 || !wireDeadRes.Exhausted || !sawWireReadOK.Load() || !sawWireDeadline.Load() {
		t.Fatalf("net.Conn SetReadDeadline vs Read coarse DPOR: ok=%v deadline=%v Exhausted=%v Failures=%+v",
			sawWireReadOK.Load(), sawWireDeadline.Load(), wireDeadRes.Exhausted, wireDeadRes.Failures)
	}

	// 1b. os.Pipe SetReadDeadline(past) vs Read on pre-buffered data.
	var sawPipeReadOK, sawPipeDeadline atomic.Bool
	pipeDeadRes := Explore(1, DPOR, func() error {
		pr, pw, err := os.Pipe()
		if err != nil {
			panic(err)
		}
		if _, err := pw.Write([]byte{'x'}); err != nil {
			panic(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = pr.SetReadDeadline(time.Unix(0, 1))
		}()
		go func() {
			defer wg.Done()
			var b [1]byte
			if n, rerr := pr.Read(b[:]); n == 1 && rerr == nil {
				sawPipeReadOK.Store(true)
			} else if rerr != nil {
				sawPipeDeadline.Store(true)
			}
		}()
		wg.Wait()
		return nil
	})
	if len(pipeDeadRes.Failures) != 0 || !pipeDeadRes.Exhausted || !sawPipeReadOK.Load() || !sawPipeDeadline.Load() {
		t.Fatalf("os.Pipe SetReadDeadline vs Read coarse DPOR: ok=%v deadline=%v Exhausted=%v Failures=%+v",
			sawPipeReadOK.Load(), sawPipeDeadline.Load(), pipeDeadRes.Exhausted, pipeDeadRes.Failures)
	}

	// 2. net.Listen vs net.Dial on the same host/port.
	var sawDialOK, sawDialRefused atomic.Bool
	listenDialRes := Explore(1, DPOR, func() error {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := net.Listen("tcp", "127.0.0.1:9411"); err != nil {
				panic(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := net.Dial("tcp", "127.0.0.1:9411"); err == nil {
				sawDialOK.Store(true)
			} else {
				sawDialRefused.Store(true)
			}
		}()
		wg.Wait()
		return nil
	})
	if len(listenDialRes.Failures) != 0 || !listenDialRes.Exhausted || !sawDialOK.Load() || !sawDialRefused.Load() {
		t.Fatalf("net.Listen vs net.Dial coarse DPOR: ok=%v refused=%v Exhausted=%v Failures=%+v",
			sawDialOK.Load(), sawDialRefused.Load(), listenDialRes.Exhausted, listenDialRes.Failures)
	}

	// 3a. Same-handle *os.File Seek vs Read cursor conflict.
	var sawByteA, sawByteB atomic.Bool
	seekReadRes := Explore(1, DPOR, func() error {
		if err := os.WriteFile("/seek_race", []byte("ab"), 0o644); err != nil {
			panic(err)
		}
		f, err := os.Open("/seek_race")
		if err != nil {
			panic(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = f.Seek(1, io.SeekStart)
		}()
		go func() {
			defer wg.Done()
			var b [1]byte
			if n, _ := f.Read(b[:]); n == 1 {
				switch b[0] {
				case 'a':
					sawByteA.Store(true)
				case 'b':
					sawByteB.Store(true)
				}
			}
		}()
		wg.Wait()
		return nil
	})
	if len(seekReadRes.Failures) != 0 || !seekReadRes.Exhausted || !sawByteA.Load() || !sawByteB.Load() {
		t.Fatalf("same-handle File.Seek vs Read coarse DPOR: sawA=%v sawB=%v Exhausted=%v Failures=%+v",
			sawByteA.Load(), sawByteB.Load(), seekReadRes.Exhausted, seekReadRes.Failures)
	}

	// 3b. Same-process os.Chdir vs relative os.ReadFile working-directory conflict.
	var sawDirOne, sawDirTwo atomic.Bool
	chdirRes := Explore(1, DPOR, func() error {
		if err := os.MkdirAll("/d1", 0o755); err != nil {
			panic(err)
		}
		if err := os.MkdirAll("/d2", 0o755); err != nil {
			panic(err)
		}
		if err := os.WriteFile("/d1/v", []byte("1"), 0o644); err != nil {
			panic(err)
		}
		if err := os.WriteFile("/d2/v", []byte("2"), 0o644); err != nil {
			panic(err)
		}
		if err := os.Chdir("/d1"); err != nil {
			panic(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = os.Chdir("/d2")
		}()
		go func() {
			defer wg.Done()
			data, err := os.ReadFile("v")
			if err != nil {
				panic(err)
			}
			switch string(data) {
			case "1":
				sawDirOne.Store(true)
			case "2":
				sawDirTwo.Store(true)
			}
		}()
		wg.Wait()
		return nil
	})
	if len(chdirRes.Failures) != 0 || !chdirRes.Exhausted || !sawDirOne.Load() || !sawDirTwo.Load() {
		t.Fatalf("os.Chdir vs relative ReadFile coarse DPOR: saw1=%v saw2=%v Exhausted=%v Failures=%+v",
			sawDirOne.Load(), sawDirTwo.Load(), chdirRes.Exhausted, chdirRes.Failures)
	}

	// 3c. syscall.SetsockoptInt vs syscall.GetsockoptInt on the same socket.
	var sawOptOff, sawOptOn atomic.Bool
	sockoptRes := Explore(1, DPOR, func() error {
		ln, err := net.Listen("tcp", "127.0.0.1:9412")
		if err != nil {
			panic(err)
		}
		cli, err := net.Dial("tcp", "127.0.0.1:9412")
		if err != nil {
			panic(err)
		}
		if _, err := ln.Accept(); err != nil {
			panic(err)
		}
		rc, err := cli.(syscall.Conn).SyscallConn()
		if err != nil {
			panic(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = rc.Control(func(fd uintptr) {
				// net.Dial enables SO_KEEPALIVE by default; clear it so the racing
				// GetsockoptInt observes 1 (before) vs 0 (after).
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE, 0)
			})
		}()
		go func() {
			defer wg.Done()
			_ = rc.Control(func(fd uintptr) {
				v, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
				if err != nil {
					panic(err)
				}
				if v == 0 {
					sawOptOff.Store(true)
				} else {
					sawOptOn.Store(true)
				}
			})
		}()
		wg.Wait()
		return nil
	})
	if len(sockoptRes.Failures) != 0 || !sockoptRes.Exhausted || !sawOptOff.Load() || !sawOptOn.Load() {
		t.Fatalf("SetsockoptInt vs GetsockoptInt coarse DPOR: off=%v on=%v Exhausted=%v Failures=%+v",
			sawOptOff.Load(), sawOptOn.Load(), sockoptRes.Exhausted, sockoptRes.Failures)
	}

	// 4. Send-buffer space wakeups (bumpSpaceLocked inside KernelMutex) must not
	// leak a readier->readied DPOR happens-before edge that suppresses reversal
	// against an independent deadline waker.
	var sawWireBPUnset atomic.Bool
	wireBPRes := Explore(1, DPOR, func() error {
		dstSetNetSendBuffer(4)
		_ = os.Unsetenv("WIRE_BP_K")
		ln, err := net.Listen("tcp", "127.0.0.1:9413")
		if err != nil {
			panic(err)
		}
		cli, err := net.Dial("tcp", "127.0.0.1:9413")
		if err != nil {
			panic(err)
		}
		srv, err := ln.Accept()
		if err != nil {
			panic(err)
		}
		if _, err := cli.Write([]byte("1234")); err != nil {
			panic(err)
		}

		writeDone := make(chan struct{})
		go func() {
			_, _ = cli.Write([]byte("56"))
			if os.Getenv("WIRE_BP_K") == "" {
				sawWireBPUnset.Store(true)
			}
			close(writeDone)
		}()
		synctest.Wait()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = os.Setenv("WIRE_BP_K", "1")
			var b [1]byte
			_, _ = srv.Read(b[:])
		}()
		go func() {
			defer wg.Done()
			_ = cli.SetWriteDeadline(time.Unix(0, 1))
		}()
		<-writeDone
		wg.Wait()
		return nil
	})
	if len(wireBPRes.Failures) != 0 || !wireBPRes.Exhausted || !sawWireBPUnset.Load() {
		t.Fatalf("net.Conn send-buffer backpressure wakeup leaked DPOR HB: sawUnset=%v Schedules=%d Exhausted=%v Failures=%+v",
			sawWireBPUnset.Load(), wireBPRes.Schedules, wireBPRes.Exhausted, wireBPRes.Failures)
	}
}

func TestDSTKernelFutexTeardownDPORReversal(t *testing.T) {
	// 1. Crash("waiter") racing FUTEX_WAKE on a waiter that has closed its fd and
	// unmapped its VMA after parking in FUTEX_WAIT must still run futex teardown
	// via dstFutexHasWaitersForProc and record the futex coarse key so DPOR
	// explores both woke=0 and woke=1 regardless of initial goroutine order.
	for _, crashFirst := range []bool{true, false} {
		var sawZero, sawOne bool
		res := Explore(1, DPOR, func() error {
			Host("h1", func() {
				if err := os.WriteFile("/futex_td_crash", make([]byte, 4096), 0o644); err != nil {
					panic(err)
				}
				f, err := os.OpenFile("/futex_td_crash", os.O_RDWR, 0)
				if err != nil {
					panic(err)
				}
				wakeMem, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					panic(err)
				}
				_ = f.Close()
				defer syscall.Munmap(wakeMem)

				waiterParked := make(chan struct{})
				go Process("waiter", func() {
					wf, err := os.OpenFile("/futex_td_crash", os.O_RDWR, 0)
					if err != nil {
						panic(err)
					}
					mem, err := syscall.Mmap(int(wf.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
					if err != nil {
						panic(err)
					}
					_ = wf.Close()
					w := (*uint32)(unsafe.Pointer(&mem[0]))
					childStarted := make(chan struct{})
					go func() {
						close(childStarted)
						_, _, errno := syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(w)), 0, 0, 0, 0, 0)
						if errno != 0 {
							panic(fmt.Sprintf("waiter FUTEX_WAIT errno = %v", errno))
						}
						select {}
					}()
					<-childStarted
					synctest.Wait()
					if err := syscall.Munmap(mem); err != nil {
						panic(err)
					}
					close(waiterParked)
					select {}
				})
				<-waiterParked

				word := (*uint32)(unsafe.Pointer(&wakeMem[0]))
				var wakeN uintptr
				var wg sync.WaitGroup
				wg.Add(2)
				runCrash := func() {
					defer wg.Done()
					CrashProcess("h1", "waiter")
				}
				runWake := func() {
					defer wg.Done()
					n, _, errno := syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(word)), 1, 1, 0, 0, 0)
					if errno != 0 {
						panic(fmt.Sprintf("waker FUTEX_WAKE errno = %v", errno))
					}
					wakeN = n
				}
				if crashFirst {
					go runCrash()
					go runWake()
				} else {
					go runWake()
					go runCrash()
				}
				wg.Wait()
				if wakeN == 0 {
					sawZero = true
				} else if wakeN == 1 {
					sawOne = true
				}
				synctest.Wait()
			})
			return nil
		})
		if len(res.Failures) != 0 || !res.Exhausted || !sawZero || !sawOne {
			t.Fatalf("crashFirst=%v: sawZero=%v sawOne=%v Schedules=%d Exhausted=%v Failures=%+v",
				crashFirst, sawZero, sawOne, res.Schedules, res.Exhausted, res.Failures)
		}
	}

	// 2. Normal Process("waiter_exit_only") return while a background child
	// goroutine of that process is parked in FUTEX_WAIT (after closing its fd and
	// unmapping its VMA so zero other OS resources remain) must scrub the orphaned
	// futex queue entry so a subsequent single-waiter FUTEX_WAKE wakes a live peer.
	Run(1, func() {
		Host("h1", func() {
			if err := os.WriteFile("/futex_td_exit", make([]byte, 4096), 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile("/futex_td_exit", os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			mem, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
			if err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			defer syscall.Munmap(mem)
			word := (*uint32)(unsafe.Pointer(&mem[0]))

			waiterReady := make(chan struct{})
			exitTrigger := make(chan struct{})
			go Process("waiter_exit_only", func() {
				wf, err := os.OpenFile("/futex_td_exit", os.O_RDWR, 0)
				if err != nil {
					panic(err)
				}
				wmem, err := syscall.Mmap(int(wf.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					panic(err)
				}
				_ = wf.Close()
				ww := (*uint32)(unsafe.Pointer(&wmem[0]))
				started := make(chan struct{})
				go func() {
					close(started)
					_, _, _ = syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(ww)), 0, 0, 0, 0, 0)
				}()
				<-started
				synctest.Wait()
				if err := syscall.Munmap(wmem); err != nil {
					panic(err)
				}
				close(waiterReady)
				<-exitTrigger
			})
			<-waiterReady

			close(exitTrigger)
			synctest.Wait()

			liveWoke := make(chan struct{})
			go Process("live_waiter", func() {
				lf, err := os.OpenFile("/futex_td_exit", os.O_RDWR, 0)
				if err != nil {
					panic(err)
				}
				lmem, err := syscall.Mmap(int(lf.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				if err != nil {
					panic(err)
				}
				_ = lf.Close()
				defer syscall.Munmap(lmem)
				lw := (*uint32)(unsafe.Pointer(&lmem[0]))
				_, _, errno := syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(lw)), 0, 0, 0, 0, 0)
				if errno != 0 {
					panic(errno)
				}
				close(liveWoke)
			})
			synctest.Wait()

			n, _, errno := syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(word)), 1, 1, 0, 0, 0)
			if errno != 0 || n != 1 {
				t.Fatalf("FUTEX_WAKE after waiter_exit_only returned (%d, %v), want (1, 0)", n, errno)
			}
			synctest.Wait()
			select {
			case <-liveWoke:
			default:
				t.Fatalf("orphaned futex waiter from exited process stole single-waiter FUTEX_WAKE")
			}
		})
	})
}

func TestDSTKernelNetAbnormalTerminationRaceHB(t *testing.T) {
	if os.Getenv("DST_NET_PREBUF_NO_HB_HELPER") == "1" {
		res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 16}, func() error {
			var resetState int
			var ln net.Listener
			var addr string
			Host("h1", func() {
				var err error
				ln, err = net.Listen("tcp", ":9500")
				if err != nil {
					panic(err)
				}
				addr = net.JoinHostPort(HostIP("h1"), "9500")
			})
			defer ln.Close()
			var cli net.Conn
			Host("h2", func() {
				var err error
				cli, err = net.Dial("tcp", addr)
				if err != nil {
					panic(err)
				}
			})
			defer cli.Close()
			var srv net.Conn
			Host("h1", func() {
				var err error
				srv, err = ln.Accept()
				if err != nil {
					panic(err)
				}
				if _, err := srv.Write([]byte("ok")); err != nil {
					panic(err)
				}
			})
			defer srv.Close()

			done1 := make(chan struct{})
			done2 := make(chan struct{})
			go func() {
				resetState = 77
				ResetLinkConns("h1", "h2")
				close(done1)
			}()
			go func() {
				var b [2]byte
				n, err := cli.Read(b[:])
				if err != nil || n != 2 {
					panic(fmt.Sprintf("pre-buffered Read = (%d, %v), want (2, nil)", n, err))
				}
				// Draining pre-buffered bytes before observing ECONNRESET must NOT
				// forge happens-before from Reset.
				_ = resetState
				close(done2)
			}()
			<-done1
			<-done2
			return nil
		})
		fmt.Printf("PREBUF_NO_RESET_HB_RACE_CAUGHT=%v\n", len(res.Failures) > 0)
		return
	}

	// 1. Injected Reset after pre-buffered data: draining pre-buffered bytes
	// succeeds even after Reset, and the post-drain ECONNRESET observation carries
	// TSan/DPOR HB from the Reset caller without any channel edge between them.
	resetRes := Explore(1, DPOR, func() error {
		var resetState int
		var ln net.Listener
		var addr string
		Host("h1", func() {
			var err error
			ln, err = net.Listen("tcp", ":9501")
			if err != nil {
				panic(err)
			}
			addr = net.JoinHostPort(HostIP("h1"), "9501")
		})
		defer ln.Close()
		var cli net.Conn
		Host("h2", func() {
			var err error
			cli, err = net.Dial("tcp", addr)
			if err != nil {
				panic(err)
			}
		})
		defer cli.Close()
		var srv net.Conn
		Host("h1", func() {
			var err error
			srv, err = ln.Accept()
			if err != nil {
				panic(err)
			}
			if _, err := srv.Write([]byte("ok")); err != nil {
				panic(err)
			}
		})
		defer srv.Close()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			resetState = 77
			ResetLinkConns("h1", "h2")
		}()
		go func() {
			defer wg.Done()
			var b [2]byte
			n, err := cli.Read(b[:])
			if err != nil || n != 2 {
				panic(fmt.Sprintf("pre-buffered Read = (%d, %v), want (2, nil)", n, err))
			}
			for {
				_, err = cli.Read(b[:])
				if err != nil {
					if !errors.Is(err, syscall.ECONNRESET) {
						panic(fmt.Sprintf("post-drain Read err = %v, want ECONNRESET", err))
					}
					break
				}
			}
			if resetState != 77 {
				panic(fmt.Sprintf("resetState = %d, want 77", resetState))
			}
		}()
		wg.Wait()
		return nil
	})
	if len(resetRes.Failures) != 0 || !resetRes.Exhausted {
		t.Fatalf("Reset ECONNRESET HB: Schedules=%d Exhausted=%v Failures=%+v",
			resetRes.Schedules, resetRes.Exhausted, resetRes.Failures)
	}

	// 2. Partition + RetransmitTimeout ETIMEDOUT must carry transitive TSan/DPOR
	// HB from the Partition caller through horizonDie to the ETIMEDOUT observer
	// with zero channel synchronization between the two goroutines.
	horizonRes := Explore(1, DPOR, func() error {
		dstSetNetRetransmitTimeout(int64(20 * time.Millisecond))
		var cutState int
		var ln net.Listener
		var addr string
		Host("h1", func() {
			var err error
			ln, err = net.Listen("tcp", ":9502")
			if err != nil {
				panic(err)
			}
			addr = net.JoinHostPort(HostIP("h1"), "9502")
		})
		defer ln.Close()
		var cli net.Conn
		Host("h2", func() {
			var err error
			cli, err = net.Dial("tcp", addr)
			if err != nil {
				panic(err)
			}
		})
		defer cli.Close()
		var srv net.Conn
		Host("h1", func() {
			var err error
			srv, err = ln.Accept()
			if err != nil {
				panic(err)
			}
		})
		defer srv.Close()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			cutState = 91
			Partition("h2", "h1")
		}()
		go func() {
			defer wg.Done()
			for {
				time.Sleep(5 * time.Millisecond)
				if _, err := cli.Write([]byte("x")); err != nil {
					if !errors.Is(err, syscall.ETIMEDOUT) {
						panic(fmt.Sprintf("Write under partition = %v, want ETIMEDOUT", err))
					}
					break
				}
			}
			if cutState != 91 {
				panic(fmt.Sprintf("cutState = %d, want 91", cutState))
			}
		}()
		wg.Wait()
		HealLink("h2", "h1")
		return nil
	})
	if len(horizonRes.Failures) != 0 || !horizonRes.Exhausted {
		t.Fatalf("Partition ETIMEDOUT HB: Schedules=%d Exhausted=%v Failures=%+v",
			horizonRes.Schedules, horizonRes.Exhausted, horizonRes.Failures)
	}

	// 3. Listener Close racing net.Dial must carry TSan/DPOR HB to the
	// ECONNREFUSED observer without leaking HB onto successful dials.
	dialRefusedRes := Explore(1, DPOR, func() error {
		var closedState int
		ln, err := net.Listen("tcp", "127.0.0.1:9503")
		if err != nil {
			panic(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			closedState = 53
			_ = ln.Close()
		}()
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", "127.0.0.1:9503")
			if err != nil {
				if !errors.Is(err, syscall.ECONNREFUSED) {
					panic(fmt.Sprintf("Dial err = %v, want ECONNREFUSED", err))
				}
				if closedState != 53 {
					panic(fmt.Sprintf("closedState = %d, want 53", closedState))
				}
				return
			}
			_ = c.Close()
		}()
		wg.Wait()
		return nil
	})
	if len(dialRefusedRes.Failures) != 0 || !dialRefusedRes.Exhausted {
		t.Fatalf("Listener Close vs Dial ECONNREFUSED HB: Schedules=%d Exhausted=%v Failures=%+v",
			dialRefusedRes.Schedules, dialRefusedRes.Exhausted, dialRefusedRes.Failures)
	}

	// 4. Write racing Reset across the outside-lock caller-slice copy window must
	// never self-deadlock on e.out.mu and must carry TSan/DPOR HB on error.
	var sawWriteOK, sawWriteErr bool
	writeResetRes := Explore(1, DPOR, func() error {
		var writeResetState int
		sharedBuf := []byte("hello")
		var ln net.Listener
		var addr string
		Host("h1", func() {
			var err error
			ln, err = net.Listen("tcp", ":9504")
			if err != nil {
				panic(err)
			}
			addr = net.JoinHostPort(HostIP("h1"), "9504")
		})
		defer ln.Close()
		var cli net.Conn
		Host("h2", func() {
			var err error
			cli, err = net.Dial("tcp", addr)
			if err != nil {
				panic(err)
			}
		})
		defer cli.Close()
		var srv net.Conn
		Host("h1", func() {
			var err error
			srv, err = ln.Accept()
			if err != nil {
				panic(err)
			}
		})
		defer srv.Close()

		var wroteOK bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			writeResetState = 88
			ResetLinkConns("h1", "h2")
		}()
		go func() {
			defer wg.Done()
			n, err := cli.Write(sharedBuf)
			if err != nil {
				if !errors.Is(err, syscall.ECONNRESET) && !errors.Is(err, syscall.EPIPE) {
					panic(fmt.Sprintf("Write racing Reset err = %v, want ECONNRESET/EPIPE", err))
				}
				if writeResetState != 88 {
					panic(fmt.Sprintf("writeResetState = %d, want 88", writeResetState))
				}
				return
			}
			if n != len(sharedBuf) {
				panic(fmt.Sprintf("Write n = %d, want %d", n, len(sharedBuf)))
			}
			wroteOK = true
		}()
		wg.Wait()
		if wroteOK {
			sawWriteOK = true
		} else {
			sawWriteErr = true
		}
		return nil
	})
	if len(writeResetRes.Failures) != 0 || !writeResetRes.Exhausted || !sawWriteOK || !sawWriteErr {
		t.Fatalf("Write racing Reset HB / leaf-lock: ok=%v err=%v Schedules=%d Exhausted=%v Failures=%+v",
			sawWriteOK, sawWriteErr, writeResetRes.Schedules, writeResetRes.Exhausted, writeResetRes.Failures)
	}

	if dstRaceEnabledFP() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDSTKernelNetAbnormalTerminationRaceHB$")
		cmd.Env = append(os.Environ(), "DST_NET_PREBUF_NO_HB_HELPER=1")
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), "PREBUF_NO_RESET_HB_RACE_CAUGHT=true") && !strings.Contains(string(out), "DATA RACE") {
			t.Fatalf("pre-buffered Read unexpectedly forged Reset HB:\n%s", out)
		}
	}
}

func TestDSTKernelSlowDiskCoarseDPOR(t *testing.T) {
	for _, opFirst := range []bool{true, false} {
		var sawFastHandle, sawSlowHandle bool
		var sawFastPath, sawSlowPath bool
		var sawFastRoot, sawSlowRoot bool

		res := Explore(1, DPOR, func() error {
			Host("h1", func() {
				Process("reader", func() {
					if err := os.WriteFile("/slow_target", []byte("abcd"), 0o644); err != nil {
						panic(err)
					}
					rf, err := os.Open("/slow_target")
					if err != nil {
						panic(err)
					}
					defer rf.Close()
					root, err := os.OpenRoot("/")
					if err != nil {
						panic(err)
					}
					defer root.Close()

					var slowH, slowP, slowR bool
					var wg sync.WaitGroup
					wg.Add(2)
					runSlow := func() {
						defer wg.Done()
						SlowDisk("h1", 50*time.Millisecond)
					}
					runOps := func() {
						defer wg.Done()
						var b [4]byte
						t0 := time.Now()
						if _, err := rf.ReadAt(b[:], 0); err != nil {
							panic(err)
						}
						slowH = time.Since(t0) >= 50*time.Millisecond

						t1 := time.Now()
						if _, err := os.Stat("/slow_target"); err != nil {
							panic(err)
						}
						slowP = time.Since(t1) >= 50*time.Millisecond

						t2 := time.Now()
						if _, err := root.Stat("slow_target"); err != nil {
							panic(err)
						}
						slowR = time.Since(t2) >= 50*time.Millisecond
					}
					if opFirst {
						go runOps()
						go runSlow()
					} else {
						go runSlow()
						go runOps()
					}
					wg.Wait()
					if slowH {
						sawSlowHandle = true
					} else {
						sawFastHandle = true
					}
					if slowP {
						sawSlowPath = true
					} else {
						sawFastPath = true
					}
					if slowR {
						sawSlowRoot = true
					} else {
						sawFastRoot = true
					}
				})
			})
			return nil
		})
		if len(res.Failures) != 0 || !res.Exhausted ||
			!sawFastHandle || !sawSlowHandle ||
			!sawFastPath || !sawSlowPath ||
			!sawFastRoot || !sawSlowRoot {
			t.Fatalf("opFirst=%v: handle=(%v,%v) path=(%v,%v) root=(%v,%v) Schedules=%d Exhausted=%v Failures=%+v",
				opFirst,
				sawFastHandle, sawSlowHandle,
				sawFastPath, sawSlowPath,
				sawFastRoot, sawSlowRoot,
				res.Schedules, res.Exhausted, res.Failures)
		}
	}
}

func TestDSTKernelMMapReleaseOrderDeterministic(t *testing.T) {
	runOnce := func() [2]int {
		var mu sync.Mutex
		var order [2]int
		var idx int
		Run(1, func() {
			Host("h1", func() {
				if err := os.WriteFile("/lock1", make([]byte, 4096), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile("/lock2", make([]byte, 4096), 0o644); err != nil {
					t.Fatal(err)
				}
				holderReady := make(chan struct{})
				go Process("holder", func() {
					f1, err := os.OpenFile("/lock1", os.O_RDWR, 0)
					if err != nil {
						panic(err)
					}
					f2, err := os.OpenFile("/lock2", os.O_RDWR, 0)
					if err != nil {
						panic(err)
					}
					if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_EX); err != nil {
						panic(err)
					}
					if err := syscall.Flock(int(f2.Fd()), syscall.LOCK_EX); err != nil {
						panic(err)
					}
					_, err = syscall.Mmap(int(f1.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
					if err != nil {
						panic(err)
					}
					_, err = syscall.Mmap(int(f2.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
					if err != nil {
						panic(err)
					}
					// Close both fds so flockPending retains both locks strictly
					// through the two live mappings; Crash("holder") then releases
					// them via dstMMapReleaseWhere.
					_ = f1.Close()
					_ = f2.Close()
					close(holderReady)
					select {}
				})
				<-holderReady

				w1Done := make(chan struct{})
				w2Done := make(chan struct{})
				go Process("w1", func() {
					f, err := os.OpenFile("/lock1", os.O_RDWR, 0)
					if err != nil {
						panic(err)
					}
					defer f.Close()
					if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
						panic(err)
					}
					mu.Lock()
					order[idx] = 1
					idx++
					mu.Unlock()
					close(w1Done)
				})
				synctest.Wait()
				go Process("w2", func() {
					f, err := os.OpenFile("/lock2", os.O_RDWR, 0)
					if err != nil {
						panic(err)
					}
					defer f.Close()
					if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
						panic(err)
					}
					mu.Lock()
					order[idx] = 2
					idx++
					mu.Unlock()
					close(w2Done)
				})
				synctest.Wait()

				CrashProcess("h1", "holder")
				<-w1Done
				<-w2Done
			})
		})
		return order
	}

	for iter := range 16 {
		if got := runOnce(); got != [2]int{1, 2} {
			t.Fatalf("iter %d: mmap-retained flock release order = %v, want [1 2]", iter, got)
		}
	}
}

func TestDSTKernelConfigureLinkCoarseDPOR(t *testing.T) {
	if os.Getenv("DST_CONFIGURE_LINK_NO_HB_HELPER") == "1" {
		res := ExploreWith(1, ExploreOptions{Mode: DPOR, MaxSchedules: 16}, func() error {
			var unsyncState int
			var ln net.Listener
			var addr string
			Host("h1", func() {
				var err error
				ln, err = net.Listen("tcp", ":9510")
				if err != nil {
					panic(err)
				}
				addr = net.JoinHostPort(HostIP("h1"), "9510")
			})
			defer ln.Close()
			var cli net.Conn
			Host("h2", func() {
				var err error
				cli, err = net.Dial("tcp", addr)
				if err != nil {
					panic(err)
				}
			})
			defer cli.Close()
			var srv net.Conn
			Host("h1", func() {
				var err error
				srv, err = ln.Accept()
				if err != nil {
					panic(err)
				}
			})
			defer srv.Close()

			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				unsyncState = 42
				ConfigureLink("h1", "h2", LinkConfig{Latency: 50 * time.Millisecond})
			}()
			go func() {
				defer wg.Done()
				if _, err := cli.Write([]byte("x")); err != nil {
					panic(err)
				}
				var b [1]byte
				if _, err := srv.Read(b[:]); err != nil {
					panic(err)
				}
				// ConfigureLink is pure timing policy and must NOT forge a TSan
				// happens-before edge across goroutines.
				_ = unsyncState
			}()
			wg.Wait()
			return nil
		})
		fmt.Printf("CONFIGURE_LINK_NO_HB_RACE_CAUGHT=%v\n", len(res.Failures) > 0)
		return
	}

	for _, opFirst := range []bool{true, false} {
		var sawFastWrite, sawSlowWrite bool
		var sawFastClose, sawSlowClose bool

		writeRes := Explore(1, DPOR, func() error {
			var ln net.Listener
			var addr string
			Host("h1", func() {
				var err error
				ln, err = net.Listen("tcp", ":9511")
				if err != nil {
					panic(err)
				}
				addr = net.JoinHostPort(HostIP("h1"), "9511")
			})
			defer ln.Close()
			var cli net.Conn
			Host("h2", func() {
				var err error
				cli, err = net.Dial("tcp", addr)
				if err != nil {
					panic(err)
				}
			})
			defer cli.Close()
			var srv net.Conn
			Host("h1", func() {
				var err error
				srv, err = ln.Accept()
				if err != nil {
					panic(err)
				}
			})
			defer srv.Close()

			t0 := time.Now()
			doneCfg := make(chan struct{})
			doneWrite := make(chan struct{})
			runCfg := func() {
				defer close(doneCfg)
				ConfigureLink("h1", "h2", LinkConfig{Latency: 50 * time.Millisecond})
			}
			runWrite := func() {
				defer close(doneWrite)
				if _, err := cli.Write([]byte("abcd")); err != nil {
					panic(err)
				}
			}
			if opFirst {
				go runWrite()
				go runCfg()
			} else {
				go runCfg()
				go runWrite()
			}
			<-doneCfg
			<-doneWrite
			var buf [4]byte
			if _, err := io.ReadFull(srv, buf[:]); err != nil {
				panic(err)
			}
			if time.Since(t0) >= 50*time.Millisecond {
				sawSlowWrite = true
			} else {
				sawFastWrite = true
			}
			return nil
		})
		if len(writeRes.Failures) != 0 || !writeRes.Exhausted || !sawFastWrite || !sawSlowWrite {
			t.Fatalf("Write vs ConfigureLink opFirst=%v: fast=%v slow=%v Schedules=%d Exhausted=%v Failures=%+v",
				opFirst, sawFastWrite, sawSlowWrite, writeRes.Schedules, writeRes.Exhausted, writeRes.Failures)
		}

		closeRes := Explore(1, DPOR, func() error {
			var ln net.Listener
			var addr string
			Host("h1", func() {
				var err error
				ln, err = net.Listen("tcp", ":9512")
				if err != nil {
					panic(err)
				}
				addr = net.JoinHostPort(HostIP("h1"), "9512")
			})
			defer ln.Close()
			var cli net.Conn
			Host("h2", func() {
				var err error
				cli, err = net.Dial("tcp", addr)
				if err != nil {
					panic(err)
				}
			})
			var srv net.Conn
			Host("h1", func() {
				var err error
				srv, err = ln.Accept()
				if err != nil {
					panic(err)
				}
			})
			defer srv.Close()

			t0 := time.Now()
			doneCfg := make(chan struct{})
			doneClose := make(chan struct{})
			runCfg := func() {
				defer close(doneCfg)
				ConfigureLinkOneWay("h2", "h1", LinkConfig{Latency: 50 * time.Millisecond})
			}
			runClose := func() {
				defer close(doneClose)
				if err := cli.Close(); err != nil {
					panic(err)
				}
			}
			if opFirst {
				go runClose()
				go runCfg()
			} else {
				go runCfg()
				go runClose()
			}
			<-doneCfg
			<-doneClose
			var buf [1]byte
			if n, err := srv.Read(buf[:]); n != 0 || err != io.EOF {
				panic(fmt.Sprintf("srv Read after graceful Close = (%d, %v), want (0, io.EOF)", n, err))
			}
			if time.Since(t0) >= 50*time.Millisecond {
				sawSlowClose = true
			} else {
				sawFastClose = true
			}
			return nil
		})
		if len(closeRes.Failures) != 0 || !closeRes.Exhausted || !sawFastClose || !sawSlowClose {
			t.Fatalf("Close vs ConfigureLinkOneWay opFirst=%v: fast=%v slow=%v Schedules=%d Exhausted=%v Failures=%+v",
				opFirst, sawFastClose, sawSlowClose, closeRes.Schedules, closeRes.Exhausted, closeRes.Failures)
		}

		// Close on h2 with unread inbound bytes racing IsolateHost("h1") (which
		// records only peerHost h1, not localHost h2) must conflict via coarseClose's
		// peerHost tracking and explore both ECONNRESET and blackholed timeout.
		var sawCloseRST, sawCloseTimeout bool
		peerCloseRes := Explore(1, DPOR, func() error {
			var ln net.Listener
			var addr string
			Host("h1", func() {
				var err error
				ln, err = net.Listen("tcp", ":9513")
				if err != nil {
					panic(err)
				}
				addr = net.JoinHostPort(HostIP("h1"), "9513")
			})
			defer ln.Close()
			var cli net.Conn
			Host("h2", func() {
				var err error
				cli, err = net.Dial("tcp", addr)
				if err != nil {
					panic(err)
				}
			})
			var srv net.Conn
			Host("h1", func() {
				var err error
				srv, err = ln.Accept()
				if err != nil {
					panic(err)
				}
				if _, err := srv.Write([]byte("x")); err != nil {
					panic(err)
				}
			})
			defer srv.Close()
			// Let srv's byte arrive and be ACKed in cli's receive queue before the
			// t=1ms close-vs-isolate race: if IsolateHost("h1") wins, cli's single
			// close RST is dropped by the cut and srv has no un-ACKed data left to
			// retransmit after UnisolateHost("h1"), leaving srv half-open to hit its
			// read deadline; if cli.Close() wins, the RST reaches h1 before the cut.
			time.Sleep(time.Millisecond)

			doneIso := make(chan struct{})
			doneClose := make(chan struct{})
			runIsolate := func() {
				defer close(doneIso)
				IsolateHost("h1")
			}
			runClose := func() {
				defer close(doneClose)
				_ = cli.Close()
			}
			if opFirst {
				go runClose()
				go runIsolate()
			} else {
				go runIsolate()
				go runClose()
			}
			<-doneIso
			<-doneClose
			UnisolateHost("h1")
			_ = srv.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			var buf [1]byte
			_, err := srv.Read(buf[:])
			switch {
			case errors.Is(err, syscall.ECONNRESET):
				sawCloseRST = true
			case errors.Is(err, os.ErrDeadlineExceeded):
				sawCloseTimeout = true
			default:
				panic(fmt.Sprintf("unexpected srv.Read err = %v", err))
			}
			return nil
		})
		if len(peerCloseRes.Failures) != 0 || !peerCloseRes.Exhausted || !sawCloseRST || !sawCloseTimeout {
			t.Fatalf("Close vs IsolateHost(peerHost) opFirst=%v: rst=%v timeout=%v Schedules=%d Exhausted=%v Failures=%+v",
				opFirst, sawCloseRST, sawCloseTimeout, peerCloseRes.Schedules, peerCloseRes.Exhausted, peerCloseRes.Failures)
		}
	}

	if dstRaceEnabledFP() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDSTKernelConfigureLinkCoarseDPOR$")
		cmd.Env = append(os.Environ(), "DST_CONFIGURE_LINK_NO_HB_HELPER=1")
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), "CONFIGURE_LINK_NO_HB_RACE_CAUGHT=true") && !strings.Contains(string(out), "DATA RACE") {
			t.Fatalf("ConfigureLink unexpectedly forged TSan HB:\n%s", out)
		}
	}
}
