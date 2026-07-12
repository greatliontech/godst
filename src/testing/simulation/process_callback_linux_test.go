// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package simulation

import (
	"runtime"
	"runtime/metrics"
	"sync/atomic"
	"testing"
	"time"
	_ "unsafe"
)

//go:linkname dstCallbacksPendingFP runtime.dstCallbacksPendingFP
func dstCallbacksPendingFP() bool

func TestDSTProcessCallbacksStopAtExit(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	for _, callback := range []string{"finalizer", "cleanup"} {
		for _, queued := range []bool{false, true} {
			for _, crash := range []bool{false, true} {
				name := callback
				if queued {
					name += "/queued-before-death"
				} else {
					name += "/discovered-after-death"
				}
				if crash {
					name += "/crash"
				} else {
					name += "/return"
				}
				t.Run(name, func(t *testing.T) {
					var ran atomic.Int32
					Test(t, 1, func(t *testing.T) {
						ready := make(chan struct{})
						body := func() {
							if callback == "finalizer" {
								func() {
									x := new(int)
									runtime.SetFinalizer(x, func(*int) { ran.Add(1) })
								}()
							} else {
								func() {
									x := new(int)
									runtime.AddCleanup(x, func(counter *atomic.Int32) { counter.Add(1) }, &ran)
								}()
							}
							if queued {
								runtime.GC()
							}
							close(ready)
							if crash {
								select {}
							}
						}
						if crash {
							go Process("worker", body)
							<-ready
							Crash("worker")
						} else {
							Process("worker", body)
							<-ready
						}
						runtime.GC()
						time.Sleep(time.Millisecond)
						if got := ran.Load(); got != 0 {
							t.Fatalf("dead process callback ran %d times", got)
						}
					})
				})
			}
		}
	}
}

func TestDSTProcessCallbackOwnershipIsPerInvocation(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	Test(t, 1, func(t *testing.T) {
		var exitedRan, liveRan atomic.Int32
		exitedReady := make(chan struct{})
		liveReady := make(chan struct{})
		releaseExited := make(chan struct{})
		releaseLive := make(chan struct{})
		exitedDone := make(chan struct{})

		go func() {
			Process("worker", func() {
				func() {
					x := new(int)
					runtime.SetFinalizer(x, func(*int) { exitedRan.Add(1) })
				}()
				close(exitedReady)
				<-releaseExited
			})
			close(exitedDone)
		}()
		go Process("worker", func() {
			func() {
				x := new(int)
				runtime.SetFinalizer(x, func(*int) { liveRan.Add(1) })
			}()
			close(liveReady)
			<-releaseLive
		})

		<-exitedReady
		<-liveReady
		close(releaseExited)
		<-exitedDone
		runtime.GC()
		time.Sleep(time.Millisecond)
		if got := exitedRan.Load(); got != 0 {
			t.Fatalf("exited invocation callback ran %d times", got)
		}
		if got := liveRan.Load(); got != 1 {
			t.Fatalf("live sibling invocation callback ran %d times, want 1", got)
		}
		close(releaseLive)
	})
}

func TestDSTProcessCrashStopsRunningCallback(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	Test(t, 1, func(t *testing.T) {
		var postCrash atomic.Int32
		registered := make(chan struct{})
		started := make(chan struct{})
		release := make(chan struct{})
		go Process("worker", func() {
			func() {
				x := new(int)
				runtime.SetFinalizer(x, func(*int) {
					close(started)
					<-release
					postCrash.Add(1)
				})
			}()
			close(registered)
			select {}
		})
		<-registered
		runtime.GC()
		<-started
		Crash("worker")
		close(release)
		time.Sleep(time.Millisecond)
		if got := postCrash.Load(); got != 0 {
			t.Fatalf("callback resumed after its process crashed: %d", got)
		}
	})
}

func TestDSTProcessCrashStopsRunningCleanup(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	Test(t, 1, func(t *testing.T) {
		var postCrash atomic.Int32
		registered := make(chan struct{})
		started := make(chan struct{})
		release := make(chan struct{})
		go Process("worker", func() {
			func() {
				x := new([1024]byte)
				runtime.AddCleanup(x, func(_ *int) {
					close(started)
					<-release
					postCrash.Add(1)
				}, new(int))
			}()
			close(registered)
			select {}
		})
		<-registered
		runtime.GC()
		<-started
		Crash("worker")
		close(release)
		time.Sleep(time.Millisecond)
		if got := postCrash.Load(); got != 0 {
			t.Fatalf("cleanup resumed after its process crashed: %d", got)
		}
	})
}

func TestDSTProcessExitStopsRunningCallback(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	Test(t, 1, func(t *testing.T) {
		var postExit atomic.Int32
		registered := make(chan struct{})
		started := make(chan struct{})
		exit := make(chan struct{})
		processDone := make(chan struct{})
		release := make(chan struct{})
		go func() {
			Process("worker", func() {
				func() {
					x := new(int)
					runtime.SetFinalizer(x, func(*int) {
						close(started)
						<-release
						postExit.Add(1)
					})
				}()
				close(registered)
				<-exit
			})
			close(processDone)
		}()
		<-registered
		runtime.GC()
		<-started
		close(exit)
		<-processDone
		close(release)
		time.Sleep(time.Millisecond)
		if got := postExit.Load(); got != 0 {
			t.Fatalf("callback resumed after its process exited: %d", got)
		}
	})
}

func TestDSTProcessCallbacksDoNotEscapeRun(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	for _, callback := range []string{"finalizer", "cleanup"} {
		t.Run(callback, func(t *testing.T) {
			var ran atomic.Int32
			var keep *[1024]byte
			Test(t, 1, func(t *testing.T) {
				Process("worker", func() {
					keep = new([1024]byte)
					if callback == "finalizer" {
						runtime.SetFinalizer(keep, func(*[1024]byte) { ran.Add(1) })
					} else {
						runtime.AddCleanup(keep, func(counter *atomic.Int32) { counter.Add(1) }, &ran)
					}
				})
			})
			metric := "/gc/finalizers/executed:finalizers"
			if callback == "cleanup" {
				metric = "/gc/cleanups/executed:cleanups"
			}
			before := processCallbackMetric(metric)
			keep = nil
			done := make(chan struct{})
			if callback == "finalizer" {
				func() {
					x := new([1024]byte)
					runtime.SetFinalizer(x, func(*[1024]byte) { close(done) })
				}()
			} else {
				func() {
					x := new([1024]byte)
					runtime.AddCleanup(x, func(ch chan struct{}) { close(ch) }, done)
				}()
			}
			runtime.GC()
			<-done
			waitProcessCallbacks(t)
			if got := ran.Load(); got != 0 {
				t.Fatalf("dead process callback escaped the run: %d", got)
			}
			if got := processCallbackMetric(metric); got != before+1 {
				t.Fatalf("executed metric changed by %d, want 1 sentinel callback", got-before)
			}
		})
	}
}

func processCallbackMetric(name string) uint64 {
	sample := []metrics.Sample{{Name: name}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

func waitProcessCallbacks(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for dstCallbacksPendingFP() && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if dstCallbacksPendingFP() {
		t.Fatal("callback queues did not drain")
	}
}

func TestDSTProcessCallbackEpochPreventsPIDReuse(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	var ran atomic.Int32
	var keep *[1024]byte
	Test(t, 1, func(t *testing.T) {
		Process("worker", func() {
			keep = new([1024]byte)
			runtime.SetFinalizer(keep, func(*[1024]byte) { ran.Add(1) })
		})
	})
	Test(t, 1, func(t *testing.T) {
		ready := make(chan struct{})
		release := make(chan struct{})
		go Process("worker", func() {
			close(ready)
			<-release
		})
		<-ready
		keep = nil
		runtime.GC()
		close(release)
	})
	done := make(chan struct{})
	func() {
		x := new([1024]byte)
		runtime.SetFinalizer(x, func(*[1024]byte) { close(done) })
	}()
	runtime.GC()
	<-done
	waitProcessCallbacks(t)
	if got := ran.Load(); got != 0 {
		t.Fatalf("prior-run callback attached to a reused PID: %d", got)
	}
}

func TestDSTProcessExitStopsRunningCleanup(t *testing.T) {
	if !dstBuilt() {
		t.Skip("requires -tags dst")
	}
	Test(t, 1, func(t *testing.T) {
		var postExit atomic.Int32
		registered := make(chan struct{})
		started := make(chan struct{})
		exit := make(chan struct{})
		processDone := make(chan struct{})
		release := make(chan struct{})
		go func() {
			Process("worker", func() {
				func() {
					x := new([1024]byte)
					runtime.AddCleanup(x, func(_ *int) {
						close(started)
						<-release
						postExit.Add(1)
					}, new(int))
				}()
				close(registered)
				<-exit
			})
			close(processDone)
		}()
		<-registered
		runtime.GC()
		<-started
		close(exit)
		<-processDone
		close(release)
		time.Sleep(time.Millisecond)
		if got := postExit.Load(); got != 0 {
			t.Fatalf("cleanup resumed after its process exited: %d", got)
		}
	})
}
