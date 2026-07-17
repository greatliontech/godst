// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os_test

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
	"unsafe"
)

const (
	futexWait    = 0
	futexWake    = 1
	futexPrivate = 128
)

func rawFutex(t *testing.T, addr *uint32, op int, val uint32, ts *syscall.Timespec) (uintptr, syscall.Errno) {
	t.Helper()
	r1, _, e := syscall.Syscall6(syscall.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)), uintptr(op), uintptr(val),
		uintptr(unsafe.Pointer(ts)), 0, 0)
	return r1, e
}

func futexWord(t *testing.T, path string) (*uint32, []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if err := f.Truncate(8); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	m, err := syscall.Mmap(int(f.Fd()), 0, 8, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	return (*uint32)(unsafe.Pointer(&m[0])), m
}

// TestDSTFutexWaitWakeCrossProcess pins the shared futex pair end to end:
// a waiter in one process parks on the word through its own mapping of the
// file; a peer process stores a new value through a DIFFERENT mapping and
// issues FUTEX_WAKE — the waiter returns woken and observes the store. The
// (node, offset) queue identity is what makes the two mappings one futex.
func TestDSTFutexWaitWakeCrossProcess(t *testing.T) {
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			done := make(chan error, 1)
			go simulation.Process("waiter", func() {
				w, _ := futexWord(t, "/w")
				for {
					_, e := rawFutex(t, w, futexWait, 0, nil)
					if e != 0 && e != syscall.EAGAIN {
						done <- e
						return
					}
					if atomic.LoadUint32(w) == 1 {
						atomic.StoreUint32(w, 2) // handshake: observed
						done <- nil
						return
					}
				}
			})
			simulation.Process("waker", func() {
				w, _ := futexWord(t, "/w")
				time.Sleep(10 * time.Millisecond) // virtual: let the waiter park
				atomic.StoreUint32(w, 1)
				if _, e := rawFutex(t, w, futexWake, 1<<30, nil); e != 0 {
					t.Errorf("WAKE: %v", e)
				}
				for atomic.LoadUint32(w) != 2 {
					time.Sleep(time.Millisecond)
				}
			})
			if err := <-done; err != nil {
				t.Fatalf("waiter: %v", err)
			}
		})
	})
}

// TestDSTFutexSemantics pins the single-process contract: value-mismatch
// EAGAIN, virtual-clock ETIMEDOUT, zero-wake counts, FIFO wake order,
// unaligned EINVAL, and the fence for non-shared ops.
func TestDSTFutexSemantics(t *testing.T) {
	simulation.Run(2, func() {
		w, m := futexWord(t, "/s")

		if _, e := rawFutex(t, w, futexWait, 1, nil); e != syscall.EAGAIN {
			t.Fatalf("WAIT on mismatched value = %v, want EAGAIN", e)
		}
		if n, e := rawFutex(t, w, futexWake, 1<<30, nil); e != 0 || n != 0 {
			t.Fatalf("WAKE with no waiters = (%d, %v), want (0, nil)", n, e)
		}

		start := time.Now()
		ts := syscall.NsecToTimespec((50 * time.Millisecond).Nanoseconds())
		if _, e := rawFutex(t, w, futexWait, 0, &ts); e != syscall.ETIMEDOUT {
			t.Fatalf("WAIT timeout = %v, want ETIMEDOUT", e)
		}
		if el := time.Since(start); el < 50*time.Millisecond {
			t.Fatalf("timeout after %v of virtual time, want >= 50ms", el)
		}

		// FIFO: first parked, first woken.
		order := make(chan int, 2)
		park := func(id int) {
			if _, e := rawFutex(t, w, futexWait, 0, nil); e != 0 {
				t.Errorf("waiter %d: %v", id, e)
			}
			order <- id
		}
		go park(1)
		time.Sleep(time.Millisecond) // deterministic park order on the virtual clock
		go park(2)
		time.Sleep(time.Millisecond)
		if n, e := rawFutex(t, w, futexWake, 1, nil); e != 0 || n != 1 {
			t.Fatalf("WAKE(1) = (%d, %v), want (1, nil)", n, e)
		}
		if got := <-order; got != 1 {
			t.Fatalf("WAKE(1) woke waiter %d, want 1 (FIFO)", got)
		}
		if n, e := rawFutex(t, w, futexWake, 1<<30, nil); e != 0 || n != 1 {
			t.Fatalf("WAKE(all) = (%d, %v), want (1, nil)", n, e)
		}
		if got := <-order; got != 2 {
			t.Fatalf("second wake reached waiter %d, want 2", got)
		}

		unaligned := (*uint32)(unsafe.Pointer(&m[1]))
		if _, e := rawFutex(t, unaligned, futexWait, 0, nil); e != syscall.EINVAL {
			t.Fatalf("unaligned WAIT = %v, want EINVAL", e)
		}

		// PRIVATE (and any op outside shared WAIT/WAKE) meets the fence.
		p := func() (p any) {
			defer func() { p = recover() }()
			rawFutex(t, w, futexWait|futexPrivate, 0, nil)
			return nil
		}()
		if p == nil || !strings.Contains(fmt.Sprint(p), "unsupported under deterministic simulation") {
			t.Fatalf("PRIVATE futex = %v, want the fence refusal", p)
		}
	})
}

// TestDSTFutexCrashDropsWaiters pins the kernel's exit semantics: a crashed
// process's parked waiter leaves the queue, so a later FUTEX_WAKE neither
// counts it nor spends a wake slot on it (no stolen wakes).
func TestDSTFutexCrashDropsWaiters(t *testing.T) {
	simulation.Run(3, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			go simulation.Process("doomed", func() {
				w, _ := futexWord(t, "/c")
				rawFutex(t, w, futexWait, 0, nil) // parks forever; crash reaps it
				t.Error("doomed waiter returned")
			})
			simulation.Process("survivor", func() {
				w, _ := futexWord(t, "/c")
				time.Sleep(10 * time.Millisecond) // let the doomed waiter park
				simulation.Crash("doomed")
				if n, e := rawFutex(t, w, futexWake, 1, nil); e != 0 || n != 0 {
					t.Errorf("WAKE after crash = (%d, %v), want (0, nil): the dead waiter must not absorb the wake", n, e)
				}
			})
		})
	})
}

// TestDSTFutexLostWakeWindow pins the hash-bucket-lock invariant: the value
// check and the enqueue are one critical section with the wake's dequeue.
// Choreography: the test holds the bucket lock, lets the waiter run up to
// it, stores a new value, then releases — a correct WAIT re-checks the value
// UNDER the lock and answers EAGAIN; an implementation that loaded before
// the lock would park on a stale value and need the rescue wake (returning
// 0), the exact lost-wake shape futex(2) exists to close.
func TestDSTFutexLostWakeWindow(t *testing.T) {
	simulation.Run(4, func() {
		w, _ := futexWord(t, "/lw")
		mu := os.DSTFutexMuForTest()
		res := make(chan syscall.Errno, 1)
		mu.Lock()
		go func() {
			_, e := rawFutex(t, w, futexWait, 0, nil)
			res <- e
		}()
		// A mutex park is not durably blocking for the bubble's clock, so
		// yield cooperatively (never sleep while holding the bucket lock).
		for range 200 {
			runtime.Gosched() // waiter reaches the bucket lock
		}
		atomic.StoreUint32(w, 1)
		mu.Unlock()
		select {
		case e := <-res:
			if e != syscall.EAGAIN {
				t.Fatalf("WAIT across the window = %v, want EAGAIN", e)
			}
		case <-time.After(100 * time.Millisecond):
			// Parked on the stale value: the lost wake. Rescue and fail.
			rawFutex(t, w, futexWake, 1<<30, nil)
			<-res
			t.Fatal("waiter parked on a stale value — value check ran outside the bucket lock")
		}
	})
}

// TestDSTFutexWakeBeatsTimeout pins the timeout-vs-wake resolution: a wake
// that dequeued the waiter wins even when the virtual-clock timer fired at
// the same instant — the waiter reports woken (0), never ETIMEDOUT, because
// the wake spent a slot on it. Fifty same-instant collisions; the seeded
// select must take the timer arm at least once for the pin to bite, which a
// fixed seed makes reproducible.
func TestDSTFutexWakeBeatsTimeout(t *testing.T) {
	simulation.Run(5, func() {
		w, _ := futexWord(t, "/tw")
		for i := range 50 {
			atomic.StoreUint32(w, 0)
			res := make(chan syscall.Errno, 1)
			ts := syscall.NsecToTimespec((10 * time.Millisecond).Nanoseconds())
			go func() {
				_, e := rawFutex(t, w, futexWait, 0, &ts)
				res <- e
			}()
			time.Sleep(10 * time.Millisecond) // timer and wake land at one instant
			n, e := rawFutex(t, w, futexWake, 1<<30, nil)
			if e != 0 {
				t.Fatalf("iter %d: WAKE: %v", i, e)
			}
			// The two sides must agree: a wake that dequeued the waiter
			// (n==1) spent its slot — the waiter reports woken (0), never
			// ETIMEDOUT; a wake that missed (n==0) means the waiter
			// already timed out and removed itself.
			we := <-res
			if (n == 1) != (we == 0) {
				t.Fatalf("iter %d: wake count %d vs waiter %v — a consumed wake must win over the timeout", i, n, we)
			}
		}
	})
}

// TestDSTFutexUnbackedWordEFAULT pins the kernel's answer for a futex word
// on an unbacked page (a reservation window past EOF): EFAULT from both
// WAIT and WAKE — never a fault, never a wedged bucket lock.
func TestDSTFutexUnbackedWordEFAULT(t *testing.T) {
	simulation.Run(6, func() {
		f, err := os.OpenFile("/u", os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := f.Truncate(4096); err != nil {
			t.Fatal(err)
		}
		m, err := syscall.Mmap(int(f.Fd()), 0, 8192, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatal(err)
		}
		hole := (*uint32)(unsafe.Pointer(&m[4096]))
		if _, e := rawFutex(t, hole, futexWait, 0, nil); e != syscall.EFAULT {
			t.Fatalf("WAIT on unbacked word = %v, want EFAULT", e)
		}
		if _, e := rawFutex(t, hole, futexWake, 1, nil); e != syscall.EFAULT {
			t.Fatalf("WAKE on unbacked word = %v, want EFAULT", e)
		}
	})
}

// TestDSTFutexWakeZeroWakesOne pins the kernel's wake floor (host-probed):
// FUTEX_WAKE with val=0 or negative still wakes one waiter.
func TestDSTFutexWakeZeroWakesOne(t *testing.T) {
	simulation.Run(7, func() {
		w, _ := futexWord(t, "/z")
		res := make(chan syscall.Errno, 1)
		go func() {
			_, e := rawFutex(t, w, futexWait, 0, nil)
			res <- e
		}()
		time.Sleep(time.Millisecond)
		if n, e := rawFutex(t, w, futexWake, 0, nil); e != 0 || n != 1 {
			t.Fatalf("WAKE(0) = (%d, %v), want (1, nil): the kernel wakes before it checks the count", n, e)
		}
		if e := <-res; e != 0 {
			t.Fatalf("waiter woken by WAKE(0) reported %v", e)
		}
	})
}

// TestDSTFutexHostCrashDropsWaiters pins reboot semantics: a power loss
// destroys the futex queues even though the durable file node — the queue
// key — survives the restore; the rebooted host's fresh waiter must not
// queue behind a dead one (a WAKE(1) starving it would be the lost wake).
func TestDSTFutexHostCrashDropsWaiters(t *testing.T) {
	simulation.Run(8, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			simulation.Process("setup", func() {
				f, err := os.OpenFile("/n", os.O_RDWR|os.O_CREATE, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(8); err != nil {
					t.Fatal(err)
				}
				if err := f.Sync(); err != nil { // durable: the node survives the crash
					t.Fatal(err)
				}
				d, err := os.Open("/")
				if err != nil {
					t.Fatal(err)
				}
				if err := d.Sync(); err != nil {
					t.Fatal(err)
				}
				d.Close()
				f.Close()
			})
			go simulation.Process("victim", func() {
				w, _ := futexWord(t, "/n")
				rawFutex(t, w, futexWait, 0, nil) // parked at power loss
				t.Error("victim returned across a host crash")
			})
			simulation.Process("observer", func() {
				time.Sleep(10 * time.Millisecond) // victim parks
			})
		})
		simulation.CrashHost("h")
		simulation.Host("h", simulation.HostConfig{}, func() {
			done := make(chan struct{})
			go simulation.Process("fresh", func() {
				w, _ := futexWord(t, "/n")
				for {
					_, e := rawFutex(t, w, futexWait, 0, nil)
					if e != 0 && e != syscall.EAGAIN {
						t.Errorf("fresh waiter: %v", e)
						return
					}
					if atomic.LoadUint32(w) == 1 {
						close(done)
						return
					}
				}
			})
			simulation.Process("waker", func() {
				w, _ := futexWord(t, "/n")
				time.Sleep(10 * time.Millisecond)
				atomic.StoreUint32(w, 1)
				if n, e := rawFutex(t, w, futexWake, 1, nil); e != 0 || n != 1 {
					t.Errorf("WAKE(1) after reboot = (%d, %v), want (1, nil): the dead waiter must be gone", n, e)
				}
				<-done
			})
		})
	})
}

// TestDSTFutexProtNoneWordStillWaits pins the harness-view value check: a
// SUT that PROT_NONEs its own view can still WAIT and be woken — the model
// reads the page-cache view, never the caller's mapping (recorded
// divergence from the kernel's EFAULT), and no fault can wedge the queue.
func TestDSTFutexProtNoneWordStillWaits(t *testing.T) {
	simulation.Run(9, func() {
		w, m := futexWord(t, "/p")
		if err := syscall.Mprotect(m, syscall.PROT_NONE); err != nil {
			t.Fatalf("mprotect: %v", err)
		}
		res := make(chan syscall.Errno, 1)
		go func() {
			_, e := rawFutex(t, w, futexWait, 0, nil)
			res <- e
		}()
		time.Sleep(time.Millisecond)
		if n, e := rawFutex(t, w, futexWake, 1, nil); e != 0 || n != 1 {
			t.Fatalf("WAKE over PROT_NONE view = (%d, %v), want (1, nil)", n, e)
		}
		if e := <-res; e != 0 {
			t.Fatalf("waiter under PROT_NONE view = %v, want woken", e)
		}
	})
}

// TestDSTFutexPartialPageBacked pins page-granular reachability: a word on
// the file's last partially-used page is backed (kernel semantics), one on
// the next page is EFAULT.
func TestDSTFutexPartialPageBacked(t *testing.T) {
	simulation.Run(10, func() {
		f, err := os.OpenFile("/pp", os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := f.Truncate(4100); err != nil { // backs pages 0 and 1
			t.Fatal(err)
		}
		m, err := syscall.Mmap(int(f.Fd()), 0, 12288, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			t.Fatal(err)
		}
		inPartial := (*uint32)(unsafe.Pointer(&m[8000])) // page 1, past i_size
		if _, e := rawFutex(t, inPartial, futexWait, 1, nil); e != syscall.EAGAIN {
			t.Fatalf("WAIT on partial-page word = %v, want EAGAIN (backed, reads zero)", e)
		}
		beyond := (*uint32)(unsafe.Pointer(&m[8192]))
		if _, e := rawFutex(t, beyond, futexWait, 0, nil); e != syscall.EFAULT {
			t.Fatalf("WAIT past the backed pages = %v, want EFAULT", e)
		}
	})
}
