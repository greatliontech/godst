// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"internal/synctest"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

//go:noinline
func registerPreRunSometimesCallbacks(finRan, cleanRan *atomic.Bool) {
	finObj := new([64]byte)
	runtime.SetFinalizer(finObj, func(*[64]byte) {
		if Sometimes("pre-run-finalizer", 1) {
			panic("pre-run finalizer Sometimes returned true")
		}
		finRan.Store(true)
	})
	cleanObj := new([64]byte)
	runtime.AddCleanup(cleanObj, func(flag *atomic.Bool) {
		if Sometimes("pre-run-cleanup", 1) {
			panic("pre-run cleanup Sometimes returned true")
		}
		flag.Store(true)
	}, cleanRan)
}

func TestDSTSometimesValidationAndBoundaryGates(t *testing.T) {
	invalidCases := []struct {
		name string
		call func()
		want string
	}{
		{"empty", func() { Sometimes("", 0.5) }, "Sometimes name must be non-empty"},
		{"neg", func() { Sometimes("site", -1e-9) }, "Sometimes prob must be in [0, 1]"},
		{"gt1", func() { Sometimes("site", 1+1e-9) }, "Sometimes prob must be in [0, 1]"},
		{"nan", func() { Sometimes("site", math.NaN()) }, "Sometimes prob must be in [0, 1]"},
		{"pos-inf", func() { Sometimes("site", math.Inf(1)) }, "Sometimes prob must be in [0, 1]"},
		{"neg-inf", func() { Sometimes("site", math.Inf(-1)) }, "Sometimes prob must be in [0, 1]"},
	}
	for _, tc := range invalidCases {
		panicked, msg := didPanicWith(tc.call)
		if !panicked || !strings.Contains(msg, tc.want) {
			t.Errorf("%s outside run: panicked=%v msg=%q, want %q", tc.name, panicked, msg, tc.want)
		}
	}
	if Sometimes("outside-run", 1) {
		t.Fatal("Sometimes(prob=1) returned true outside a run")
	}

	// Foreign goroutines and foreign synctest bubbles during an active run must
	// still validate invalid arguments loudly, while valid Sometimes calls return
	// false with zero diagnostic or RNG perturbation.
	foreignCall := make(chan func() bool)
	foreignResp := make(chan string)
	go func() {
		for fn := range foreignCall {
			func() {
				defer func() {
					if r := recover(); r != nil {
						foreignResp <- "panic:" + fmt.Sprint(r)
					}
				}()
				if fn() {
					foreignResp <- "true"
				} else {
					foreignResp <- "false"
				}
			}()
		}
	}()
	defer close(foreignCall)

	Run(11, func() {
		for _, tc := range invalidCases {
			panicked, msg := didPanicWith(tc.call)
			if !panicked || !strings.Contains(msg, tc.want) {
				t.Errorf("%s inside run: panicked=%v msg=%q, want %q", tc.name, panicked, msg, tc.want)
			}
		}

		foreignCall <- func() bool { return Sometimes("foreign-goroutine", 1) }
		if got := <-foreignResp; got != "false" {
			t.Fatalf("foreign goroutine Sometimes(prob=1) = %q, want \"false\"", got)
		}
		foreignCall <- func() bool {
			var inner bool
			synctest.Run(func() {
				inner = Sometimes("foreign-synctest", 1)
			})
			return inner
		}
		if got := <-foreignResp; got != "false" {
			t.Fatalf("foreign synctest Sometimes(prob=1) = %q, want \"false\"", got)
		}
		foreignCall <- func() bool { return Sometimes("", 0.5) }
		if got := <-foreignResp; !strings.Contains(got, "Sometimes name must be non-empty") {
			t.Fatalf("foreign goroutine invalid Sometimes = %q, want validation panic", got)
		}
	})
	if diag := formatRunDiagnosticsBody(); strings.Contains(diag, "foreign-") {
		t.Fatalf("foreign Sometimes call leaked into run diagnostics:\n%s", diag)
	}

	// Pre-run SetFinalizer and AddCleanup callbacks executing on process-global
	// GC workers across Run and multi-episode Explore transitions must return
	// false cleanly without panicking or polluting diagnostics.
	var finRan, cleanRan atomic.Bool
	registerPreRunSometimesCallbacks(&finRan, &cleanRan)
	Run(13, func() {
		runtime.GC()
	})
	expRes := ExploreWith(17, ExploreOptions{Mode: Exhaustive, MaxSchedules: 3}, func() error {
		var mu sync.Mutex
		var wg sync.WaitGroup
		wg.Add(2)
		for range 2 {
			go func() {
				defer wg.Done()
				mu.Lock()
				mu.Unlock()
			}()
		}
		wg.Wait()
		return nil
	})
	if len(expRes.Failures) != 0 {
		t.Fatalf("ExploreWith failed while pre-run callbacks were pending/draining: %+v", expRes.Failures)
	}
	runtime.GC()
	runtime.GC()
	deadline := time.Now().Add(2 * time.Second)
	for (!finRan.Load() || !cleanRan.Load()) && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if !finRan.Load() || !cleanRan.Load() {
		t.Fatalf("pre-run callbacks did not complete: fin=%v cleanup=%v", finRan.Load(), cleanRan.Load())
	}

	for seed := uint64(1); seed <= 64; seed++ {
		Run(seed, func() {
			for range 32 {
				if Sometimes("never-fires", 0) {
					t.Fatalf("seed %d: Sometimes(prob=0) returned true", seed)
				}
				if !Sometimes("always-fires", 1) {
					t.Fatalf("seed %d: Sometimes(prob=1) returned false", seed)
				}
			}
		})
	}
}

func TestDSTSometimesTwoLevelEnablementAndDistribution(t *testing.T) {
	const (
		numSeeds     = 200
		callsPerSeed = 400
		targetProb   = 0.25
	)

	enabledAlpha := 0
	totalEnabledCalls := 0
	totalAlphaHits := 0
	var combos [4]int // bit0 = alpha enabled, bit1 = beta enabled

	for seed := uint64(1); seed <= numSeeds; seed++ {
		var alphaHits, betaHits int
		Run(seed, func() {
			for range callsPerSeed {
				if Sometimes("site-alpha", targetProb) {
					alphaHits++
				}
				if Sometimes("site-beta", 0.5) {
					betaHits++
				}
			}
		})

		// Replay with the same seed must produce the exact same hit counts.
		var replayAlpha, replayBeta int
		Run(seed, func() {
			for range callsPerSeed {
				if Sometimes("site-alpha", targetProb) {
					replayAlpha++
				}
				if Sometimes("site-beta", 0.5) {
					replayBeta++
				}
			}
		})
		if alphaHits != replayAlpha || betaHits != replayBeta {
			t.Fatalf("seed %d replay mismatch: got (%d, %d), want (%d, %d)",
				seed, replayAlpha, replayBeta, alphaHits, betaHits)
		}

		alphaOn := alphaHits > 0
		betaOn := betaHits > 0
		if alphaOn {
			enabledAlpha++
			totalEnabledCalls += callsPerSeed
			totalAlphaHits += alphaHits
		}
		mask := 0
		if alphaOn {
			mask |= 1
		}
		if betaOn {
			mask |= 2
		}
		combos[mask]++
	}

	if enabledAlpha < 70 || enabledAlpha > 130 {
		t.Fatalf("site-alpha enabled on %d/%d seeds, want ~50%% in [70, 130]", enabledAlpha, numSeeds)
	}
	rate := float64(totalAlphaHits) / float64(totalEnabledCalls)
	if rate < 0.20 || rate > 0.30 {
		t.Fatalf("site-alpha firing rate on enabled seeds = %.4f (%d/%d), want ~%.2f in [0.20, 0.30]",
			rate, totalAlphaHits, totalEnabledCalls, targetProb)
	}
	for mask, count := range combos {
		if count < 25 {
			t.Errorf("2-site enablement combination mask=%02b occurred on %d/%d seeds, want >= 25 (independent per-site gating)",
				mask, count, numSeeds)
		}
	}
}

func TestDSTSometimesObservationNeutralityAndDPORCommutativity(t *testing.T) {
	type snapshot struct {
		transcript string
		cryptoHex  string
		netDelayNs int64
		gcDelta    uint32
		procBytes  int64
		probHits   int64
	}

	var neutralitySeed uint64
	for s := uint64(1); s <= 64; s++ {
		var hit bool
		Run(s, func() {
			for range 24 {
				if Sometimes("prob-probe-a", 0.5) {
					hit = true
				}
			}
		})
		if hit {
			neutralitySeed = s
			break
		}
	}
	if neutralitySeed == 0 {
		t.Fatal("failed to find enabled seed for prob-probe-a")
	}

	runWorkload := func(mode int) snapshot {
		var out snapshot
		opts := Options{
			Network: NetworkConfig{
				CrossHost: LinkConfig{
					Latency: 2 * time.Millisecond,
					Jitter:  5 * time.Millisecond,
				},
			},
		}
		// Pre-build >260 site names outside the bubble so Mode 2 exercises both
		// off-heap slot allocation and overflow without charging string building
		// to the SUT's heap.
		siteNames := make([]string, 270)
		for i := range siteNames {
			siteNames[i] = fmt.Sprintf("neutrality-site-%03d", i)
		}

		RunWith(neutralitySeed, opts, func() {
			var ms0, ms1 runtime.MemStats
			runtime.ReadMemStats(&ms0)

			addrCh := make(chan string, 1)
			recvAtCh := make(chan int64, 1)
			Host("srv", func() {
				go Process("server", func() {
					ln, err := net.Listen("tcp", "0.0.0.0:9100")
					if err != nil {
						panic(err)
					}
					defer ln.Close()
					addrCh <- HostIP("srv") + ":9100"
					c, err := ln.Accept()
					if err != nil {
						panic(err)
					}
					defer c.Close()
					var buf [4]byte
					if _, err := c.Read(buf[:]); err != nil {
						panic(err)
					}
					recvAtCh <- time.Now().UnixNano()
				})
			})

			var procID uint32
			Host("cli", func() {
				Process("client", func() {
					_, procID = dstCurrentNode()
					addr := <-addrCh
					c, err := net.Dial("tcp", addr)
					if err != nil {
						panic(err)
					}
					sendAt := time.Now().UnixNano()
					if mode == 1 {
						_ = Sometimes("disabled-zero", 0)
					} else if mode == 2 {
						for _, name := range siteNames {
							_ = Sometimes(name, 1)
						}
					}
					if _, err := c.Write([]byte("ping")); err != nil {
						panic(err)
					}
					_ = c.Close()
					out.netDelayNs = (<-recvAtCh) - sendAt

					var mu sync.Mutex
					var sb strings.Builder
					chA := make(chan int, 1)
					chB := make(chan int, 1)
					var wg sync.WaitGroup
					for g := range 3 {
						wg.Add(1)
						go func(gid int) {
							defer wg.Done()
							for step := range 80 {
								var firedProb bool
								if mode == 1 {
									_ = Sometimes("false-probe", 0)
								} else if mode == 2 {
									firedProb = Sometimes("prob-probe-a", 0.5)
									_ = Sometimes("prob-probe-b", 1)
								}
								buf := make([]byte, 8<<10)
								buf[0] = byte(gid + step)
								rv := rand.Uint64()
								m := map[string]int{"a": step, "b": gid, "c": step ^ gid}
								acc := int(rv & 0xff)
								for k, v := range m {
									acc += int(k[0]) + v
								}
								chA <- acc
								chB <- acc + 1
								var picked int
								select {
								case v := <-chA:
									picked = v
									<-chB
								case v := <-chB:
									picked = -v
									<-chA
								}
								mu.Lock()
								if firedProb {
									out.probHits++
								}
								sb.WriteString(strconv.Itoa(gid))
								sb.WriteByte(':')
								sb.WriteString(strconv.Itoa(picked))
								sb.WriteByte(';')
								mu.Unlock()
							}
						}(g)
					}
					wg.Wait()
					var raw [32]byte
					if _, err := crand.Read(raw[:]); err != nil {
						panic(err)
					}
					out.cryptoHex = hex.EncodeToString(raw[:])
					out.transcript = sb.String()
					out.procBytes = dstProcAllocBytes(procID)
				})
			})

			runtime.ReadMemStats(&ms1)
			out.gcDelta = ms1.NumGC - ms0.NumGC
		})
		return out
	}

	base := runWorkload(0)
	falseMode := runWorkload(1)
	trueMode := runWorkload(2)
	if base.gcDelta == 0 {
		t.Fatalf("neutrality workload did not trigger GC (gcDelta=0)")
	}
	if trueMode.probHits == 0 {
		t.Fatalf("neutrality workload mode 2 never hit Level-2 probabilistic draw (probHits=0)")
	}
	for label, got := range map[string]snapshot{"false-mode": falseMode, "true-mode": trueMode} {
		if got.transcript != base.transcript {
			t.Fatalf("%s perturbed schedule/rand/map/select transcript:\n got=%.180s\nwant=%.180s", label, got.transcript, base.transcript)
		}
		if got.cryptoHex != base.cryptoHex {
			t.Fatalf("%s perturbed crypto/rand: got=%s want=%s", label, got.cryptoHex, base.cryptoHex)
		}
		if got.netDelayNs != base.netDelayNs {
			t.Fatalf("%s perturbed network jitter/latency: got=%d want=%d", label, got.netDelayNs, base.netDelayNs)
		}
		if got.gcDelta != base.gcDelta {
			t.Fatalf("%s perturbed NumGC: got=%d want=%d", label, got.gcDelta, base.gcDelta)
		}
		if got.procBytes != base.procBytes {
			t.Fatalf("%s perturbed per-process heap accounting: got=%d want=%d", label, got.procBytes, base.procBytes)
		}
	}

	// Concurrent Sometimes calls on independent goroutines must commute under DPOR
	// without adding spurious memory-access or synchronization conflicts.
	var dporSeed uint64
	for s := uint64(1); s <= 64; s++ {
		var hit bool
		Run(s, func() {
			for range 24 {
				if Sometimes("dpor-commute-a", 0.5) {
					hit = true
				}
			}
		})
		if hit {
			dporSeed = s
			break
		}
	}
	if dporSeed == 0 {
		t.Fatal("failed to find enabled seed for dpor-commute-a")
	}
	dporRun := func(withSometimes bool) ExploreResult {
		return ExploreWith(dporSeed, ExploreOptions{Mode: DPOR}, func() error {
			var shared int
			var mu sync.Mutex
			var wg sync.WaitGroup
			for i := range 2 {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					if withSometimes {
						_ = Sometimes("dpor-commute-a", 0.5)
						_ = Sometimes("dpor-commute-b", 1.0)
					}
					mu.Lock()
					shared = shared*10 + (id + 1)
					mu.Unlock()
				}(i)
			}
			wg.Wait()
			return nil
		})
	}
	quietDPOR := dporRun(false)
	activeDPOR := dporRun(true)
	if quietDPOR.Schedules != activeDPOR.Schedules ||
		quietDPOR.Footprint != activeDPOR.Footprint ||
		quietDPOR.UnattributedRaces != activeDPOR.UnattributedRaces ||
		quietDPOR.ForeignSched != activeDPOR.ForeignSched {
		t.Fatalf("Sometimes perturbed DPOR exploration footprint:\nwithout=%+v\n   with=%+v", quietDPOR, activeDPOR)
	}

	// Find an enabled seed for "buggy-interleaving" and verify Explore + Replay
	// reproduce a bug that requires BOTH a specific schedule AND Sometimes==true.
	var chosenSeed uint64
	for s := uint64(1); s <= 64; s++ {
		var hit bool
		Run(s, func() {
			for range 16 {
				if Sometimes("buggy-interleaving", 0.75) {
					hit = true
				}
			}
		})
		if hit {
			chosenSeed = s
			break
		}
	}
	if chosenSeed == 0 {
		t.Fatal("failed to find an enabled seed for buggy-interleaving")
	}

	sut := func() error {
		var order int
		var faulted bool
		var mu sync.Mutex
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			fired := Sometimes("buggy-interleaving", 0.75)
			mu.Lock()
			if order == 0 && fired {
				faulted = true
			}
			order = 1
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			mu.Lock()
			if order == 0 {
				order = 2
			}
			mu.Unlock()
		}()
		wg.Wait()
		if faulted {
			return errors.New("sometimes+schedule bug hit")
		}
		return nil
	}

	res := ExploreWith(chosenSeed, ExploreOptions{Mode: DPOR}, sut)
	if len(res.Failures) == 0 {
		t.Fatalf("ExploreWith(seed=%d) found 0 failures, want Sometimes+schedule failure", chosenSeed)
	}
	rep := Replay(chosenSeed, res.Failures[0], sut)
	if !rep.Failed() || rep.Err == nil || !strings.Contains(rep.Err.Error(), "sometimes+schedule bug hit") {
		t.Fatalf("Replay failed to reproduce Explore failure: %+v", rep)
	}
}

//go:noinline
func registerSometimesTestCleanup(done chan uint64) {
	obj := new([64]byte)
	runtime.AddCleanup(obj, func(ch chan uint64) {
		var mask uint64
		for i := range 16 {
			if Sometimes("cleanup-site", 0.5) {
				mask |= 1 << i
			}
		}
		childDone := make(chan uint64, 1)
		go func() {
			var cmask uint64
			for i := range 16 {
				if Sometimes("cleanup-site", 0.5) {
					cmask |= 1 << i
				}
			}
			childDone <- cmask
		}()
		ch <- mask | ((<-childDone) << 16)
	}, done)
}

func TestDSTSometimesFinalizerAndCleanupTwoRootIsolation(t *testing.T) {
	// Pick a seed where "cleanup-site" and "main-site" are both enabled.
	var seed uint64
	for s := uint64(1); s <= 128; s++ {
		var a, b bool
		Run(s, func() {
			for range 32 {
				a = a || Sometimes("cleanup-site", 0.5)
				b = b || Sometimes("main-site", 0.5)
			}
		})
		if a && b {
			seed = s
			break
		}
	}
	if seed == 0 {
		t.Fatal("no seed enabled both cleanup-site and main-site")
	}

	runMainAndCleanup := func(withCleanup bool) (mainBits uint64, cleanupBits uint64) {
		Run(seed, func() {
			done := make(chan uint64, 1)
			if withCleanup {
				registerSometimesTestCleanup(done)
			}
			runtime.GC()
			if withCleanup {
				cleanupBits = <-done
			}
			for i := range 32 {
				if Sometimes("main-site", 0.5) {
					mainBits |= 1 << i
				}
			}
		})
		return mainBits, cleanupBits
	}

	mainWithout, _ := runMainAndCleanup(false)
	mainWith1, cleanup1 := runMainAndCleanup(true)
	mainWith2, cleanup2 := runMainAndCleanup(true)
	if mainWithout != mainWith1 {
		t.Fatalf("cleanup Sometimes draws perturbed bubble.main dstBugRand: without=%064b with=%064b", mainWithout, mainWith1)
	}
	if mainWith1 != mainWith2 || cleanup1 != cleanup2 {
		t.Fatalf("cleanup Sometimes replay mismatch: (%x, %x) vs (%x, %x)", mainWith1, cleanup1, mainWith2, cleanup2)
	}
	if cleanup1 == 0 || cleanup1 == 0xffffffff {
		t.Fatalf("degenerate 32-bit cleanup Sometimes sequence: 0x%x", cleanup1)
	}
}

func TestDSTAfterFuncIdentityDeterminismAndCrashLiveness(t *testing.T) {
	type obs struct {
		hostname string
		pid      int
		fileBody string
		wallSkew time.Duration
		randU64  uint64
		crypto   string
		bugBits  uint64
		immHost  string
		immPid   int
	}

	// Pick a seed where "afterfunc-site" is enabled.
	var seed uint64
	for s := uint64(1); s <= 64; s++ {
		var on bool
		Run(s, func() {
			for range 24 {
				on = on || Sometimes("afterfunc-site", 0.5)
			}
		})
		if on {
			seed = s
			break
		}
	}
	if seed == 0 {
		t.Fatal("failed to find enabled seed for afterfunc-site")
	}

	runOnce := func(s uint64, armExtraCanceledCtx bool) (callerRand uint64, callerBug uint64, got obs) {
		Run(s, func() {
			baseStart := time.Now()
			HostWith("h-timer", HostConfig{Clock: Skew(7 * time.Minute)}, func() {
				Process("p-timer", func() {
					wantPid := os.Getpid()
					if err := os.WriteFile("/owned.txt", []byte("host-fs-ok"), 0o600); err != nil {
						t.Fatalf("WriteFile: %v", err)
					}

					if armExtraCanceledCtx {
						ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
						cancel()
						_ = ctx
						tm := time.AfterFunc(time.Hour, func() {})
						tm.Stop()
					}

					immDone := make(chan struct{})
					time.AfterFunc(0, func() {
						got.immHost, _ = os.Hostname()
						got.immPid = os.Getpid()
						close(immDone)
					})
					<-immDone

					done := make(chan struct{})
					time.AfterFunc(25*time.Millisecond, func() {
						got.hostname, _ = os.Hostname()
						got.pid = os.Getpid()
						data, err := os.ReadFile("/owned.txt")
						if err == nil {
							got.fileBody = string(data)
						}
						got.wallSkew = time.Now().Sub(baseStart)
						got.randU64 = rand.Uint64()
						var raw [16]byte
						if _, err := crand.Read(raw[:]); err != nil {
							panic(err)
						}
						got.crypto = hex.EncodeToString(raw[:])
						for i := range 16 {
							if Sometimes("afterfunc-site", 0.5) {
								got.bugBits |= 1 << i
							}
						}
						close(done)
					})

					callerRand = rand.Uint64()
					for i := range 16 {
						if Sometimes("afterfunc-site", 0.5) {
							callerBug |= 1 << i
						}
					}
					<-done
					if got.pid != wantPid {
						t.Fatalf("AfterFunc pid = %d, want %d", got.pid, wantPid)
					}
				})
			})
		})
		return callerRand, callerBug, got
	}

	cr1, cb1, obs1 := runOnce(seed, false)
	_, cb2, obs2 := runOnce(seed, true)
	_, _, obsOtherSeed := runOnce(seed+1, false)

	if cb1 != cb2 {
		t.Fatalf("arming/canceling context.WithTimeout or AfterFunc perturbed caller dstBugRand: %x vs %x", cb1, cb2)
	}
	// Verify dstTimerStampLocked itself consumes zero gp.dstrand draws: resetting
	// an already-heaped AfterFunc re-runs dstTimerStampLocked without re-entering
	// maybeAdd's dstTimerRand() heap insertion tie-breaker.
	var randWithoutReset, randWithReset uint64
	Run(seed, func() {
		tm := time.AfterFunc(time.Hour, func() {})
		randWithoutReset = rand.Uint64()
		tm.Stop()
	})
	Run(seed, func() {
		tm := time.AfterFunc(time.Hour, func() {})
		tm.Reset(2 * time.Hour)
		randWithReset = rand.Uint64()
		tm.Stop()
	})
	if randWithoutReset != randWithReset {
		t.Fatalf("dstTimerStampLocked advanced gp.dstrand on Reset: %x vs %x", randWithReset, randWithoutReset)
	}
	_ = cr1
	if obs1.hostname != "h-timer" || obs1.immHost != "h-timer" || obs1.immPid != obs1.pid {
		t.Fatalf("AfterFunc identity mismatch: %+v", obs1)
	}
	if obs1.fileBody != "host-fs-ok" {
		t.Fatalf("AfterFunc filesystem view = %q, want %q", obs1.fileBody, "host-fs-ok")
	}
	if obs1.wallSkew != 7*time.Minute+25*time.Millisecond {
		t.Fatalf("AfterFunc wall reading delta = %v, want %v", obs1.wallSkew, 7*time.Minute+25*time.Millisecond)
	}
	// Note: armExtraCanceledCtx increments gp.dstTimerSeq on the arming goroutine,
	// while two identical runs with the same seed replay bit-for-bit identically.
	_, _, obs1Replay := runOnce(seed, false)
	if obs1 != obs1Replay {
		t.Fatalf("AfterFunc same-seed replay diverged:\n run1=%+v\n run2=%+v", obs1, obs1Replay)
	}
	if obs1.crypto == obsOtherSeed.crypto && obs1.randU64 == obsOtherSeed.randU64 {
		t.Fatalf("AfterFunc RNG did not vary across seeds: %+v vs %+v", obs1, obsOtherSeed)
	}
	_ = obs2

	// Armed AfterFunc timers must NEVER fire after normal Process exit,
	// CrashProcess, or CrashHost, and in-flight AfterFunc goroutines must stop on
	// CrashProcess / CrashHost.
	Test(t, 31, func(t *testing.T) {
		var exitedFired, crashedProcFired, crashedHostFired, inflightSurvived atomic.Bool

		Host("h-exit", func() {
			Process("p-exit", func() {
				time.AfterFunc(10*time.Millisecond, func() {
					exitedFired.Store(true)
				})
			})
		})

		procArmed := make(chan struct{})
		inflightEntered := make(chan struct{})
		Host("h-proc-crash", func() {
			go Process("victim", func() {
				time.AfterFunc(5*time.Millisecond, func() {
					close(inflightEntered)
					time.Sleep(20 * time.Millisecond)
					inflightSurvived.Store(true)
				})
				time.AfterFunc(30*time.Millisecond, func() {
					crashedProcFired.Store(true)
				})
				close(procArmed)
				select {}
			})
		})
		<-procArmed
		<-inflightEntered
		CrashProcess("h-proc-crash", "victim")

		hostArmed := make(chan struct{})
		Host("h-host-crash", func() {
			go Process("daemon", func() {
				time.AfterFunc(15*time.Millisecond, func() {
					crashedHostFired.Store(true)
				})
				close(hostArmed)
				select {}
			})
		})
		<-hostArmed
		CrashHost("h-host-crash")

		time.Sleep(100 * time.Millisecond)
		if exitedFired.Load() {
			t.Error("AfterFunc armed by normally exited Process fired after exit")
		}
		if crashedProcFired.Load() {
			t.Error("AfterFunc armed by CrashProcess victim fired after crash")
		}
		if inflightSurvived.Load() {
			t.Error("in-flight AfterFunc goroutine survived CrashProcess")
		}
		if crashedHostFired.Load() {
			t.Error("AfterFunc armed on crashed Host fired after CrashHost")
		}
	})

	// A host-owned TCP socket whose write was performed inside a short-lived
	// helper Process must keep its modeled-kernel RetransmitTimeout watchdog
	// alive after the helper Process exits (and never emit false exit-kill
	// diagnostics).
	TestWith(t, 37, Options{
		Network: NetworkConfig{
			CrossHost:         LinkConfig{Latency: time.Millisecond},
			RetransmitTimeout: 40 * time.Millisecond,
		},
	}, func(t *testing.T) {
		addrCh := make(chan string, 1)
		Host("peer", func() {
			go Process("srv", func() {
				ln, err := net.Listen("tcp", "0.0.0.0:9200")
				if err != nil {
					panic(err)
				}
				addrCh <- HostIP("peer") + ":9200"
				c, err := ln.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				var buf [64]byte
				_, _ = c.Read(buf[:])
				select {}
			})
		})
		defer CrashHost("peer")
		addr := <-addrCh
		var hostConn net.Conn
		Host("owner", func() {
			var err error
			hostConn, err = net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			Partition("owner", "peer")
			// Short-lived helper process writes on the host-owned conn (arming
			// the kernel retransmit horizon) and immediately exits.
			Process("writer-helper", func() {
				if _, err := hostConn.Write([]byte("into-blackhole")); err != nil {
					t.Fatalf("Write: %v", err)
				}
			})
		})
		defer hostConn.Close()
		time.Sleep(60 * time.Millisecond)
		var buf [8]byte
		_, err := hostConn.Read(buf[:])
		if err == nil {
			t.Fatal("host-owned socket did not hit ETIMEDOUT after helper Process exited")
		}
		if diag := formatRunDiagnosticsBody(); strings.Contains(diag, "killed goroutines on process exit:") {
			t.Fatalf("modeled-kernel TCP timer leaked into process-exit kill diagnostics:\n%s", diag)
		}
	})

	// Socket deadlines armed via SetReadDeadline/SetWriteDeadline must bind their
	// timer lifecycle and crystal drift to the socket's owning host (e.localHost)
	// rather than any transient foreign host scope from which SetReadDeadline was
	// invoked.
	Test(t, 39, func(t *testing.T) {
		addrCh := make(chan string, 1)
		Host("srv-host", func() {
			go Process("srv", func() {
				ln, err := net.Listen("tcp", "0.0.0.0:9201")
				if err != nil {
					panic(err)
				}
				addrCh <- HostIP("srv-host") + ":9201"
				c, err := ln.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				select {}
			})
		})
		defer CrashHost("srv-host")
		addr := <-addrCh

		var sockConn net.Conn
		HostWith("sock-host", HostConfig{Clock: Skew(5 * time.Minute)}, func() {
			var err error
			sockConn, err = net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
		})
		defer sockConn.Close()

		t0 := time.Now()
		Host("caller-host", func() {
			if err := sockConn.SetReadDeadline(time.Now().Add(40 * time.Millisecond)); err != nil {
				t.Fatalf("SetReadDeadline: %v", err)
			}
		})
		// Crashing the foreign caller host must NOT cancel sock-host's deadline,
		// and doubling sock-host's crystal rate must halve the base-time wait.
		CrashHost("caller-host")
		DriftClock("sock-host", 1_000_000_000)

		var buf [8]byte
		_, err := sockConn.Read(buf[:])
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("sockConn.Read err = %v, want os.ErrDeadlineExceeded", err)
		}
		if elapsed := time.Since(t0); elapsed != 20*time.Millisecond {
			t.Fatalf("sockConn.Read deadline elapsed base time = %v, want 20ms under 2x sock-host drift", elapsed)
		}
	})
}

func TestDiagnosticsSometimesActiveSitesOnFailure(t *testing.T) {
	if os.Getenv("GO_DST_DIAG_SOMETIMES_PASS_CHILD") == "1" {
		Test(t, 41, func(t *testing.T) {
			_ = Sometimes("silent-on-pass", 1)
		})
		return
	}
	if os.Getenv("GO_DST_DIAG_SOMETIMES_FAIL_CHILD") == "1" {
		Run(40, func() {
			_ = Sometimes("stale-prior-run-site", 1)
		})
		longPrefix := strings.Repeat("x", 130)
		longA := longPrefix + "-alpha-tail-0123456789"
		longB := longPrefix + "-bravo-tail-0123456789"
		Test(t, 43, func(t *testing.T) {
			_ = Sometimes("never-hit", 0)
			_ = Sometimes("wal/short-batch", 1)
			time.Sleep(15 * time.Millisecond)
			_ = Sometimes("wal/short-batch", 1)
			_ = Sometimes("btree/early-split", 1)
			_ = Sometimes("wal/short-batch", 1)
			_ = Sometimes(longA, 1)
			_ = Sometimes(longB, 1)
			for i := range 260 {
				_ = Sometimes(fmt.Sprintf("overflow-site-%03d", i), 1)
			}
			t.Fatal("invariant violated")
		})
		return
	}
	if os.Getenv("GO_DST_DIAG_SOMETIMES_EXPLORE_CHILD") == "1" {
		ExploreTest(t, 47, ExploreOptions{Mode: Exhaustive, MaxSchedules: 2}, func() error {
			_ = Sometimes("raft/compact-early", 1)
			return errors.New("log gap")
		})
		return
	}

	passOut := runDiagnosticsChild(t, "TestDiagnosticsSometimesActiveSitesOnFailure", "GO_DST_DIAG_SOMETIMES_PASS_CHILD", false, "-test.v")
	if strings.Contains(passOut, "activated fault points") || strings.Contains(passOut, "silent-on-pass") {
		t.Fatalf("passing -v test emitted Sometimes diagnostics:\n%s", passOut)
	}

	failOut := runDiagnosticsChild(t, "TestDiagnosticsSometimesActiveSitesOnFailure", "GO_DST_DIAG_SOMETIMES_FAIL_CHILD", true)
	for _, want := range []string{
		"--- simulation diagnostics (seed 43) ---",
		"activated fault points (Sometimes):",
		`"wal/short-batch": 3 hit(s) (first at +0s)`,
		`"btree/early-split": 1 hit(s) (first at +15ms)`,
		"(truncated from 152 bytes): 1 hit(s) (first at +15ms)",
		"... +8 activation(s) across additional fault point(s) omitted",
	} {
		if !strings.Contains(failOut, want) {
			t.Errorf("failure output missing %q:\n%s", want, failOut)
		}
	}
	if strings.Contains(failOut, "never-hit") {
		t.Errorf("untriggered Sometimes site appeared in diagnostics:\n%s", failOut)
	}
	if strings.Contains(failOut, "stale-prior-run-site") {
		t.Errorf("prior run Sometimes site leaked across dstActivate reset:\n%s", failOut)
	}
	// Two >128B names sharing the same 128B prefix have distinct FNV-1a hashes,
	// so both must occupy separate ledger entries rather than colliding.
	if got := strings.Count(failOut, "(truncated from 152 bytes): 1 hit(s)"); got != 2 {
		t.Errorf("truncated distinct-hash entries count = %d, want 2:\n%s", got, failOut)
	}
	idxWal := strings.Index(failOut, `"wal/short-batch"`)
	idxBtree := strings.Index(failOut, `"btree/early-split"`)
	if idxWal < 0 || idxBtree < 0 || idxWal > idxBtree {
		t.Errorf("Sometimes sites not rendered in first-fired chronological order:\n%s", failOut)
	}

	exploreOut := runDiagnosticsChild(t, "TestDiagnosticsSometimesActiveSitesOnFailure", "GO_DST_DIAG_SOMETIMES_EXPLORE_CHILD", true)
	for _, want := range []string{
		"activated fault points (Sometimes):",
		`"raft/compact-early": 1 hit(s) (first at +0s)`,
	} {
		if !strings.Contains(exploreOut, want) {
			t.Errorf("ExploreTest failure output missing %q:\n%s", want, exploreOut)
		}
	}
}
