// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"context"
	"fmt"
	"internal/synctest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Durable in-bubble mutex waits. Under whole-world DST every goroutine that
// can unlock a mutex is inside the simulation bubble, so a sync.Mutex /
// sync.RWMutex wait is a durable block: virtual time may advance across it,
// and a SUT that sleeps — a SlowDisk delay, a batching window — while
// holding a lock its peers contend progresses exactly as on real hardware.
// Under the upstream (plain synctest) classification the same shape froze
// virtual time: the contender's park kept the bubble non-quiescent, the
// holder's sleep never fired, and the wedge detector aborted the run — the
// exact composition failure the WAL bubble tier hit with SlowDisk. Foreign
// (plain synctest) bubbles keep the upstream non-durable semantics.

// TestDSTMutexSleepUnderLockAdvances: the motivating shape. The holder
// sleeps virtual time while a contender is parked on the mutex; the run
// completes with exactly the slept time elapsed.
func TestDSTMutexSleepUnderLockAdvances(t *testing.T) {
	Run(1, func() {
		var mu sync.Mutex
		start := time.Now()
		done := make(chan struct{})
		mu.Lock()
		go func() {
			mu.Lock()
			mu.Unlock()
			close(done)
		}()
		for range 20 {
			runtime.Gosched() // park the contender on the mutex
		}
		time.Sleep(10 * time.Millisecond) // holder sleeps UNDER the lock
		mu.Unlock()
		<-done
		if got := time.Since(start); got != 10*time.Millisecond {
			panic(fmt.Sprintf("virtual elapsed %v, want exactly 10ms (the holder's sleep)", got))
		}
	})
}

// TestDSTRWMutexWaitDurable: both RWMutex park flavors are durable — a
// writer parked behind a reader, and a reader parked behind a writer, each
// while the holder sleeps.
func TestDSTRWMutexWaitDurable(t *testing.T) {
	Run(1, func() {
		var mu sync.RWMutex

		// Reader holds; writer parks; reader sleeps under RLock.
		start := time.Now()
		wdone := make(chan struct{})
		mu.RLock()
		go func() {
			mu.Lock()
			mu.Unlock()
			close(wdone)
		}()
		for range 20 {
			runtime.Gosched()
		}
		time.Sleep(3 * time.Millisecond)
		mu.RUnlock()
		<-wdone

		// Writer holds; reader parks; writer sleeps under Lock.
		rdone := make(chan struct{})
		mu.Lock()
		go func() {
			mu.RLock()
			mu.RUnlock()
			close(rdone)
		}()
		for range 20 {
			runtime.Gosched()
		}
		time.Sleep(4 * time.Millisecond)
		mu.Unlock()
		<-rdone

		if got := time.Since(start); got != 7*time.Millisecond {
			panic(fmt.Sprintf("virtual elapsed %v, want exactly 7ms (3ms + 4ms holder sleeps)", got))
		}
	})
}

// TestDSTSlowDiskUnderSUTMutex: the consumer composition the durability
// change exists for — disk latency on a store whose goroutines perform I/O
// under a shared mutex (fsync-under-lock, every real database's shape).
// Before the change this wedged; now both workers complete and the virtual
// clock has paid every per-op delay.
func TestDSTSlowDiskUnderSUTMutex(t *testing.T) {
	Run(1, func() {
		Host("h", HostConfig{}, func() {
			SlowDisk("h", time.Millisecond)
			var mu sync.Mutex
			start := time.Now()
			var wg sync.WaitGroup
			for w := 0; w < 2; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; i < 3; i++ {
						mu.Lock()
						name := fmt.Sprintf("/f-%d", w)
						if err := os.WriteFile(name, []byte{byte(i)}, 0o644); err != nil {
							panic(err)
						}
						f, err := os.OpenFile(name, os.O_WRONLY, 0)
						if err != nil {
							panic(err)
						}
						if err := f.Sync(); err != nil {
							panic(err)
						}
						if err := f.Close(); err != nil {
							panic(err)
						}
						mu.Unlock()
					}
				}(w)
			}
			wg.Wait()
			// Every disk-touching op paid 1ms and they all serialized under
			// the mutex; the exact op count is the backend's business, but
			// the elapsed time must at least cover the syncs and writes.
			if got := time.Since(start); got < 12*time.Millisecond {
				panic(fmt.Sprintf("virtual elapsed %v, want >= 12ms of paid disk latency", got))
			}
		})
	})
}

// TestDSTMutexDeadlockAborts: with mutex waits durable, a genuine lock
// inversion is no longer an invisible wedge — the bubble sees every
// goroutine durably blocked with no timer to fire and aborts with the
// bubble-deadlock diagnostic.
func TestDSTMutexDeadlockAborts(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("run with a mutex lock inversion completed; want the bubble-deadlock abort")
		}
		if !strings.Contains(fmt.Sprint(r), "deadlock") {
			panic(r) // not the bubble-deadlock diagnostic: repanic
		}
	}()
	Run(1, func() {
		var a, b sync.Mutex
		go func() {
			a.Lock()
			for range 20 {
				runtime.Gosched()
			}
			b.Lock() // parks forever: the inversion
		}()
		go func() {
			b.Lock()
			for range 20 {
				runtime.Gosched()
			}
			a.Lock() // parks forever: the inversion
		}()
		for range 100 {
			runtime.Gosched() // let both take their first lock and park
		}
	})
}

// TestDSTMutexDurableReplays: the acquisition order of a contended mutex
// under holder sleeps is a deterministic function of the seed.
func TestDSTMutexDurableReplays(t *testing.T) {
	trace := func(seed uint64) string {
		var order string
		RunWith(seed, Options{}, func() {
			var mu sync.Mutex
			var wg sync.WaitGroup
			for w := 0; w < 3; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; i < 4; i++ {
						mu.Lock()
						order += fmt.Sprintf("%d", w)
						time.Sleep(time.Duration(w+1) * 100 * time.Microsecond)
						mu.Unlock()
						time.Sleep(50 * time.Microsecond)
					}
				}(w)
			}
			wg.Wait()
		})
		return order
	}
	a, b := trace(5), trace(5)
	if a != b {
		t.Fatalf("same seed acquired in different orders:\n a: %s\n b: %s", a, b)
	}
	if c := trace(6); c == a {
		t.Logf("note: seeds 5 and 6 acquired identically (legal; scheduling may still differ)")
	}
}

// TestDSTForeignBubbleMutexStaysNonDurable: the upstream boundary. In a
// FOREIGN (plain synctest) bubble a mutex wait is NOT durable — the bubble
// must not advance fake time across it (its mutexes may be held by
// goroutines outside the bubble). The helper hangs under correct semantics
// and is killed by the timeout; under a broken gate (durable in ANY bubble)
// it progresses and prints the marker.
func TestDSTForeignBubbleMutexStaysNonDurable(t *testing.T) {
	if os.Getenv("DST_FOREIGN_MUTEX_HELPER") == "1" {
		foreignBubbleMutexHelper()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDSTForeignBubbleMutexStaysNonDurable$", "-test.v")
	cmd.Env = append(os.Environ(), "DST_FOREIGN_MUTEX_HELPER=1")
	out, _ := cmd.Output()
	if strings.Contains(string(out), "FOREIGN-TIME-ADVANCED") {
		t.Fatalf("foreign bubble advanced fake time across a mutex wait — the durable-mutex gate leaks past dstSimBubble:\n%s", out)
	}
	if ctx.Err() == nil {
		t.Fatalf("helper exited without hanging or advancing — unexpected shape:\n%s", out)
	}
}

// foreignBubbleMutexHelper runs OUTSIDE any dst run: a plain synctest
// bubble whose main sleeps while another bubble goroutine is parked on a
// mutex main holds. Correct semantics: the park is non-durable, the bubble
// never quiesces, the sleep never fires — the process hangs until the
// parent's timeout kills it.
func foreignBubbleMutexHelper() {
	synctest.Run(func() {
		var mu sync.Mutex
		mu.Lock()
		go func() {
			mu.Lock()
			mu.Unlock()
		}()
		for range 20 {
			runtime.Gosched()
		}
		time.Sleep(time.Millisecond)
		fmt.Println("FOREIGN-TIME-ADVANCED")
		mu.Unlock()
	})
}

// TestDSTCrashWithMutexParkedVictims: the crash-mark paths classify parked
// victims with the same durable predicate as the accounting sites — a
// process (and then a host) dying while one of its goroutines HOLDS a
// contended mutex and another is PARKED on it must leave the bubble's
// running count coherent: the run continues, timers still fire, and the
// run completes. A misclassified victim would double-decrement (a fatal)
// or leak a running count (a wedge).
func TestDSTCrashWithMutexParkedVictims(t *testing.T) {
	Run(1, func() {
		Host("h", HostConfig{}, func() {
			var mu sync.Mutex
			go Process("victim", func() {
				mu.Lock()
				go func() {
					mu.Lock() // parks; dies parked when the process crashes
					mu.Unlock()
				}()
				select {}
			})
			for range 50 {
				runtime.Gosched()
			}
			Crash("victim")
			time.Sleep(time.Millisecond) // timers must still fire post-crash
		})
		Host("h2", HostConfig{}, func() {
			var mu sync.Mutex
			go Process("victim2", func() {
				mu.Lock()
				go func() {
					mu.Lock()
					mu.Unlock()
				}()
				select {}
			})
			for range 50 {
				runtime.Gosched()
			}
		})
		CrashHost("h2")
		time.Sleep(time.Millisecond)
	})
}
