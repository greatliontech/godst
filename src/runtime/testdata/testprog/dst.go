// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	crand "crypto/rand"
	"encoding/hex"
	"internal/synctest"
	"math/rand/v2"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing/simulation"
	"time"
	_ "unsafe" // for go:linkname
	"weak"
)

func init() {
	register("DSTSelectOrder", DSTSelectOrder)
	register("DSTNoPreempt", DSTNoPreempt)
	register("DSTMapOrder", DSTMapOrder)
	register("DSTSelectChurn", DSTSelectChurn)
	register("DSTMathRandChurn", DSTMathRandChurn)
	register("DSTBubbleReproNoise", DSTBubbleReproNoise)
	register("DSTBubbleReproPlain", DSTBubbleReproPlain)
	register("DSTRunDeterminism", DSTRunDeterminism)
	register("DSTPoolAcrossRuns", DSTPoolAcrossRuns)
	register("DSTGCAllocBound", DSTGCAllocBound)
	register("DSTGCFinDiscovery", DSTGCFinDiscovery)
	register("DSTFinChanOp", DSTFinChanOp)
	register("DSTFinRunSet", DSTFinRunSet)
	register("DSTFinSpawn", DSTFinSpawn)
	register("DSTFinPreBubble", DSTFinPreBubble)
	register("DSTCleanupChanOp", DSTCleanupChanOp)
	register("DSTCleanupRunSet", DSTCleanupRunSet)
	register("DSTCleanupRNGIsolation", DSTCleanupRNGIsolation)
	register("DSTCleanupPreBubble", DSTCleanupPreBubble)
	register("DSTCleanupChanOpPriorG", DSTCleanupChanOpPriorG)
	register("DSTWeakClearing", DSTWeakClearing)
	register("DSTGCOffBound", DSTGCOffBound)
	register("DSTProcessIdentity", DSTProcessIdentity)
	register("DSTIdentityExtra", DSTIdentityExtra)
	register("DSTCryptoRand", DSTCryptoRand)
	register("DSTFinChain", DSTFinChain)
	register("DSTMemLimit", DSTMemLimit)
}

// dstMemSink keeps the most recent allocation live so the rest become garbage.
var dstMemSink []byte

// DSTMemLimit allocates ~16 MB of non-blocking garbage under a deterministic
// bubble-local Options.MemoryLimit (DSTMEMLIMIT bytes) and prints the resulting
// NumGC. A tighter limit forces more GCs; the count is deterministic.
func DSTMemLimit() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	limit, _ := strconv.ParseInt(os.Getenv("DSTMEMLIMIT"), 10, 64)
	// Optionally retain a large heap *before* the run so the bubble-entry baseline
	// dstHeapBase is large. The relative trigger subtracts it (heapLive - base), so
	// numGC is independent of this pre-bubble heap — the property that proves the
	// baseline is load-bearing. An absolute trigger (base dropped) lets the
	// pre-bubble heap inflate the live total and changes numGC. Retained in a
	// package var so the entry GC keeps it live.
	if pre, _ := strconv.Atoi(os.Getenv("DSTPREBUBBLE")); pre > 0 {
		for b := 0; b < pre; b += 4096 {
			dstPreBubbleSink = append(dstPreBubbleSink, make([]byte, 4096))
		}
	}
	var ngc uint32
	simulation.RunWith(n, simulation.Options{MemoryLimit: limit}, func() {
		for i := 0; i < 16384; i++ {
			dstMemSink = make([]byte, 1024)
			dstMemSink[0] = byte(i)
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		ngc = ms.NumGC
	})
	os.Stdout.WriteString(strconv.FormatUint(uint64(ngc), 10) + "\n")
}

// dstPreBubbleSink retains a pre-bubble heap for DSTMemLimit's baseline-
// independence check (DSTPREBUBBLE).
var dstPreBubbleSink [][]byte

// dstMakeFinChain builds a 3-level finalizer chain in a dropped frame: c1's
// finalizer keeps c2 alive, c2's keeps c3, and c3's finalizer touches a bubble
// channel. Each object is reachable only through the previous object's pending
// finalizer, so the chain resolves one GC per level. Three levels guarantee the
// channel-touching tail (c3) is not reached by the ordinary Run-end teardown
// (which drains ~2 levels) and would leak to the post-Run reap without the
// Run-end fixpoint.
//
//go:noinline
func dstMakeFinChain(ch chan int) {
	c3 := &dstFinObj{}
	runtime.SetFinalizer(c3, func(p *dstFinObj) { ch <- 99 })
	c2 := &dstFinObj{}
	runtime.SetFinalizer(c2, func(p *dstFinObj) { runtime.KeepAlive(c3) })
	c1 := &dstFinObj{}
	runtime.SetFinalizer(c1, func(p *dstFinObj) { runtime.KeepAlive(c2) })
	runtime.KeepAlive(c1)
}

// DSTFinChain drops a finalizer chain whose tail touches a bubble channel and
// then returns immediately (no in-run quiescence to resolve it), exercising the
// Run-end fixpoint drain (design.md D4: Run-end fixpoint). Without the
// fixpoint, the tail's finalizer runs on the post-Run reap's async fing
// (g.bubble == nil) and its send fatals "send on synctest channel from outside
// bubble"; with it, the whole chain runs in-bubble. Prints "ok" iff the run
// completes without a fatal.
func DSTFinChain() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	simulation.Run(n, func() {
		ch := make(chan int, 1) // buffered: the finalizer send must not block
		dstMakeFinChain(ch)
		_ = ch
	})
	os.Stdout.WriteString("ok\n")
}

// DSTProcessIdentity checks that os.Getpid/os.Hostname return the simulated
// process identity inside simulation.Run (a deterministic default, or the value
// set via Options), and the real machine's identity outside it. Prints
// "def=<pid>/<host> custom=<pid>/<host> restored=<bool> realoverridden=<bool>".
func DSTProcessIdentity() {
	host := func() string { h, _ := os.Hostname(); return h }
	realPID, realHost := os.Getpid(), host()
	var def, custom string
	simulation.Run(1, func() {
		def = strconv.Itoa(os.Getpid()) + "/" + host()
	})
	simulation.RunWith(1, simulation.Options{Hostname: "node7", PID: 4242}, func() {
		custom = strconv.Itoa(os.Getpid()) + "/" + host()
	})
	restored := os.Getpid() == realPID && host() == realHost
	// realoverridden confirms the real identity differs from the simulated default,
	// so def=1/sim is a genuine override and not a coincidence.
	realOverridden := realPID != 1 || realHost != "sim"
	os.Stdout.WriteString("def=" + def + " custom=" + custom +
		" restored=" + strconv.FormatBool(restored) +
		" realoverridden=" + strconv.FormatBool(realOverridden) + "\n")
}

// DSTIdentityExtra checks the rest of the process-identity surface beyond
// pid/hostname: os.Getppid/Getuid/Getgid/Geteuid/Getegid, runtime.NumCPU, and
// os/user.Current return fixed simulated values inside simulation.Run (NumCPU
// overridable via Options), and are restored to real values outside it. Prints
// "inside=[<ppid> <uid> <gid> <euid> <egid> <numcpu> <uid:gid:user:home>]
// customcpu=<n> restoredids=<bool>". restoredids compares the whole identity
// surface read *outside* the run before and after it: equality proves the run
// did not leak simulated identity (and, since the pre-run read caches the real
// os/user, that the in-run synthetic user never poisoned that cache).
func DSTIdentityExtra() {
	read := func() string {
		u, _ := user.Current()
		return strings.Join([]string{
			strconv.Itoa(os.Getppid()),
			strconv.Itoa(os.Getuid()),
			strconv.Itoa(os.Getgid()),
			strconv.Itoa(os.Geteuid()),
			strconv.Itoa(os.Getegid()),
			strconv.Itoa(runtime.NumCPU()),
			u.Uid + ":" + u.Gid + ":" + u.Username + ":" + u.HomeDir,
		}, " ")
	}
	realBefore := read()
	var inside string
	simulation.Run(1, func() { inside = read() })
	// A custom NumCPU unlikely to equal the host's real count proves the override
	// is genuine on any machine (whereas the default 8 could coincide with it).
	var customCPU int
	simulation.RunWith(1, simulation.Options{NumCPU: 3}, func() { customCPU = runtime.NumCPU() })
	realAfter := read()
	os.Stdout.WriteString("inside=[" + inside + "] customcpu=" + strconv.Itoa(customCPU) +
		" restoredids=" + strconv.FormatBool(realAfter == realBefore) + "\n")
}

// DSTCryptoRand checks that crypto/rand is deterministic inside simulation.Run
// (seeded by the run) but real OS entropy outside it. With DSTSEED=s it prints
// "h=<hex> eq=<bool> seedvaries=<bool> realdiffers=<bool>": h is the bytes read
// under seed s (stable across processes — replay), eq that a second seed-s run
// matches (same-seed determinism), seedvaries that seed s+1 differs (not a
// constant), and realdiffers that two reads *outside* a run differ (production
// crypto/rand is untouched). This is the executable form of INV-CRYPTO.
func DSTCryptoRand() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	readSeed := func(seed uint64) [32]byte {
		var b [32]byte
		simulation.Run(seed, func() {
			crand.Read(b[:])
		})
		return b
	}
	a := readSeed(n)
	b := readSeed(n)     // same seed: must equal a
	c := readSeed(n + 1) // different seed: must differ
	// Outside any run, crypto/rand is real entropy: two reads differ.
	var x, y [16]byte
	crand.Read(x[:])
	crand.Read(y[:])
	os.Stdout.WriteString("h=" + hex.EncodeToString(a[:]) +
		" eq=" + strconv.FormatBool(a == b) +
		" seedvaries=" + strconv.FormatBool(a != c) +
		" realdiffers=" + strconv.FormatBool(x != y) + "\n")
}

// dstBubbleFinqFP returns the bubble-local total finalizers discovered (the
// set-level finalizer-discovery observable). See runtime/dst.go.
//
//go:linkname dstBubbleFinqFP runtime.dstBubbleFinqFP
func dstBubbleFinqFP() uint64

type dstFinObj struct{ b [256]byte }

// DSTGCFinDiscovery exercises the per-bubble relative GC trigger's effect on
// finalizer discovery. A single goroutine (so no Seq-5 interleaving) allocates
// finalizable objects through a ring buffer, giving them varied lifetimes so the
// GC discovers them across many cycles. It prints "numGC total" — the set-level
// observable (the GC count and the total set of finalizers discovered), which is
// the DST contract (DST-GC-1): deterministic in normal AND -race builds (the
// trigger fires the right number of times with the right total). Which GC *cycle*
// discovers a given object is sub-observable byte-trigger noise, not part of the
// contract — the simulation neither claims nor tests it (design.md D1).
func DSTGCFinDiscovery() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var ngc uint32
	simulation.Run(n, func() {
		const K = 512
		ring := make([]*dstFinObj, K)
		for i := 0; i < 40000; i++ {
			o := &dstFinObj{}
			o.b[0] = byte(i)
			runtime.SetFinalizer(o, func(p *dstFinObj) { _ = p.b[0] })
			ring[i%K] = o // evicted entries die with a finalizer set
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		ngc = ms.NumGC
	})
	os.Stdout.WriteString(strconv.FormatUint(uint64(ngc), 10) + " " +
		strconv.FormatUint(dstBubbleFinqFP(), 10) + "\n")
}

// dstSliceSink forces the allocations in DSTGCAllocBound to escape to the heap
// (storing the whole slice, not just an element), so the loop produces real heap
// churn that drives the GC trigger. It is indexed per goroutine so concurrent
// stores hit distinct array slots and are race-free (the test runs under -race).
var dstSliceSink [16][]byte

//go:noinline
func dstEscape(id int, b []byte) { dstSliceSink[id&15] = b }

// DSTGCAllocBound allocates heavily across several goroutines that do not block
// during allocation, inside simulation.Run with GC enabled. A non-blocking alloc-heavy
// span never reaches a synctest quiescence point, so the *heap* trigger — not
// quiescence — is what bounds memory (design dimension 11). Under STW (Tier 2,
// D2) the number of GC cycles is deterministic (no concurrent floating garbage),
// so the printed "<sum> <numGC>" line is identical across runs of the same seed,
// and numGC>0 proves GC fired (memory stayed bounded rather than growing
// unbounded). Without STW the count floats run-to-run.
func DSTGCAllocBound() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var sum uint64
	var ngc uint32
	simulation.Run(n, func() {
		const G = 4
		done := make(chan uint64, G)
		for g := 0; g < G; g++ {
			go func(id int) {
				var acc uint64
				for i := 0; i < 30000; i++ {
					b := make([]byte, 512)
					b[0] = byte(i)
					b[len(b)-1] = byte(id)
					acc = acc*1099511628211 + uint64(b[0]) + uint64(b[len(b)-1])
					dstEscape(id, b)
				}
				done <- acc
			}(g)
		}
		for g := 0; g < G; g++ {
			sum ^= <-done
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		ngc = ms.NumGC
	})
	os.Stdout.WriteString(strconv.FormatUint(sum, 16) + " " + strconv.FormatUint(uint64(ngc), 10) + "\n")
}

// dstMakeFinSender allocates a finalizable object whose finalizer sends on a
// bubble channel, then returns without keeping a reference, so the object is dead
// once this (non-inlined) frame is gone. It is a separate function so no stack
// slot keeps the object live for the caller.
//
//go:noinline
func dstMakeFinSender(ch chan int) {
	o := &dstFinObj{}
	runtime.SetFinalizer(o, func(p *dstFinObj) { ch <- 42 })
}

// DSTFinChanOp checks that a finalizer doing a bubble channel op runs without
// fatal inside simulation.Run (invariant DST-FIN-1): the finalizer must run on the
// bubble-scoped drain goroutine (g.bubble == the bubble), not the async system
// finalizer goroutine fing (g.bubble == nil), which would fatal with "send on
// synctest channel from outside bubble". The main goroutine drops the object,
// then blocks on the channel; reaching quiescence forces the GC that discovers
// the object dead and the drain that runs its finalizer, whose send unblocks the
// receive. If finalizers never ran at all, the receive would deadlock instead.
// Prints "ok 42".
func DSTFinChanOp() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var got int
	simulation.Run(n, func() {
		ch := make(chan int, 1)
		dstMakeFinSender(ch)
		got = <-ch
	})
	os.Stdout.WriteString("ok " + strconv.Itoa(got) + "\n")
}

// dstMakeFinSpawner allocates a finalizable object whose finalizer spawns a
// goroutine that sends on a bubble channel — exercising D4 dimension 5 (a
// finalizer that starts a goroutine). The spawned goroutine must inherit the
// bubble (it is created from the drain goroutine, whose g.bubble is the bubble)
// and be deterministically scheduled and accounted, so the bubble waits for it.
//
//go:noinline
func dstMakeFinSpawner(ch chan int) {
	o := &dstFinObj{}
	runtime.SetFinalizer(o, func(p *dstFinObj) {
		go func() { ch <- 7 }()
	})
}

// DSTFinSpawn checks that a finalizer that spawns a goroutine works inside
// simulation.Run (D4 dimension 5): the spawned goroutine inherits the bubble (so its
// channel op does not fatal) and is deterministically scheduled and accounted (so
// the bubble does not deadlock or advance past it). Prints "ok 7".
func DSTFinSpawn() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var got int
	simulation.Run(n, func() {
		ch := make(chan int, 1)
		dstMakeFinSpawner(ch)
		got = <-ch
	})
	os.Stdout.WriteString("ok " + strconv.Itoa(got) + "\n")
}

// dstFinRunCount and dstFinRunSum accumulate the set of finalizers that ran, from
// the drain goroutine. Atomic so the read in the main goroutine is race-free
// regardless of the bubble's happens-before edges. The sum folds an
// order-independent per-id mix, so it captures the run *set* (not order, which the
// determinism contract does not guarantee).
var dstFinRunCount atomic.Uint64
var dstFinRunSum atomic.Uint64

// DSTFinRunSet checks that the set of finalizers run by Run end is deterministic
// (invariant DST-FIN-2) and that they actually ran (count > 0). A single
// goroutine (so no Seq-5 interleaving) allocates 2000 finalizable objects through
// a small ring so they die across the run; every object is unreferenced by the
// time f returns, so the final quiescence drain runs all 2000. Prints
// "count sumHex"; both are identical across runs of the same seed, in normal and
// -race builds (the run set is the whole dead set regardless of per-cycle
// discovery jitter).
func DSTFinRunSet() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var gotCount, gotSum uint64
	simulation.Run(n, func() {
		const N = 2000
		ring := make([]*dstFinObj, 64)
		for i := 0; i < N; i++ {
			o := &dstFinObj{}
			o.b[0] = byte(i)
			id := uint64(i)
			runtime.SetFinalizer(o, func(p *dstFinObj) {
				dstFinRunCount.Add(1)
				dstFinRunSum.Add(id*0x9e3779b97f4a7c15 + 1)
			})
			ring[i%64] = o // evicted entries die with a finalizer set
		}
		for i := range ring {
			ring[i] = nil // drop the last generation so every object is dead
		}
		// Snapshot the counters INSIDE the bubble, after an intervening quiescence
		// (time.Sleep blocks on a fake timer, forcing the drain to run). This makes
		// the assertion require the in-bubble drain: reading after simulation.Run returns
		// would let the post-Run reap (runtime.GC()×2 after dstDeactivate) run any
		// missed finalizers on fing and launder the count.
		time.Sleep(time.Millisecond)
		gotCount = dstFinRunCount.Load()
		gotSum = dstFinRunSum.Load()
	})
	os.Stdout.WriteString(strconv.FormatUint(gotCount, 10) + " " +
		strconv.FormatUint(gotSum, 16) + "\n")
}

// dstMakeCleanupSender attaches a cleanup that sends on a bubble channel to a
// fresh object, then drops it, so the object is dead once this frame is gone.
//
//go:noinline
func dstMakeCleanupSender(ch chan int) {
	o := &dstFinObj{}
	runtime.AddCleanup(o, func(c chan int) { c <- 42 }, ch)
}

// DSTCleanupChanOp is the cleanup analogue of DSTFinChanOp (invariant
// DST-CLEANUP-1): a cleanup doing a bubble channel op must run on the bubble drain
// (g.bubble == the bubble), not the async cleanup pool (g.bubble == nil, which
// fatals). The main goroutine drops the watched object, then blocks on the
// channel; reaching quiescence runs the cleanup, whose send unblocks the receive.
// Prints "ok 42".
func DSTCleanupChanOp() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var got int
	simulation.Run(n, func() {
		ch := make(chan int, 1)
		dstMakeCleanupSender(ch)
		got = <-ch
	})
	os.Stdout.WriteString("ok " + strconv.Itoa(got) + "\n")
}

// dstForcePriorCleanupG forces a cleanup goroutine to exist before any simulation.Run by
// running a cleanup outside DST (createGs is ungated there). The resulting cleanup
// goroutine persists, parked on dequeue, so a later Run exercises the cleanup WAKE
// gate (the createGs gate alone does not, since no cleanup G is created in-Run).
//
//go:noinline
func dstForcePriorCleanupG() {
	done := make(chan struct{}, 1)
	o := &dstFinObj{}
	runtime.AddCleanup(o, func(d chan struct{}) { d <- struct{}{} }, done)
	runtime.KeepAlive(o)
	runtime.GC() // outside DST: creates a cleanup G and runs the cleanup
	<-done       // the cleanup G is now parked on dequeue
}

// DSTCleanupChanOpPriorG is DSTCleanupChanOp with a cleanup goroutine forced to
// exist before the Run. Without the cleanup wake gate (proc.go), that pre-existing
// async cleanup goroutine is woken during the Run and runs the bubble cleanup with
// g.bubble == nil, fataling "send on synctest channel from outside bubble". With
// the gate it stays parked and the bubble drain runs the cleanup. Prints "ok 42".
func DSTCleanupChanOpPriorG() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	dstForcePriorCleanupG()
	var got int
	simulation.Run(n, func() {
		ch := make(chan int, 1)
		dstMakeCleanupSender(ch)
		got = <-ch
	})
	os.Stdout.WriteString("ok " + strconv.Itoa(got) + "\n")
}

var dstCleanupRunCount atomic.Uint64
var dstCleanupRunSum atomic.Uint64

// DSTCleanupRunSet is the cleanup analogue of DSTFinRunSet (invariant
// DST-CLEANUP-2): the set of cleanups run by the in-bubble drain is deterministic
// and they actually run. Counters are read inside f after an intervening
// quiescence so the post-Run reap cannot launder the count. Prints "count sumHex".
func DSTCleanupRunSet() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var gotCount, gotSum uint64
	simulation.Run(n, func() {
		const N = 2000
		ring := make([]*dstFinObj, 64)
		for i := 0; i < N; i++ {
			o := &dstFinObj{}
			id := uint64(i)
			runtime.AddCleanup(o, func(x uint64) {
				dstCleanupRunCount.Add(1)
				dstCleanupRunSum.Add(x*0x9e3779b97f4a7c15 + 1)
			}, id)
			ring[i%64] = o // evicted entries die with a cleanup attached
		}
		for i := range ring {
			ring[i] = nil
		}
		time.Sleep(time.Millisecond) // quiescence → the drain runs the cleanups
		gotCount = dstCleanupRunCount.Load()
		gotSum = dstCleanupRunSum.Load()
	})
	os.Stdout.WriteString(strconv.FormatUint(gotCount, 10) + " " +
		strconv.FormatUint(gotSum, 16) + "\n")
}

// DSTCleanupRNGIsolation checks that using AddCleanup does not perturb the
// bubble's DST RNG stream — i.e. that the async cleanup goroutine (which draws
// from the creating goroutine's stream via newproc1, and persists across Runs) is
// NOT created under DST (the createGs gate in mcleanup.go). It compares the first
// rand draw in a Run that calls AddCleanup against one that does not; they must be
// equal. Without the gate, the first AddCleanup creates a cleanup goroutine that
// advances bubble.main's stream, so the draws differ. Prints "ok" or "perturbed".
func DSTCleanupRNGIsolation() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	measure := func(withCleanup bool) uint64 {
		var r uint64
		simulation.Run(n, func() {
			if withCleanup {
				o := &dstFinObj{}
				runtime.AddCleanup(o, func(int) {}, 0)
				runtime.KeepAlive(o)
			}
			r = rand.Uint64()
		})
		return r
	}
	if measure(true) == measure(false) {
		os.Stdout.WriteString("ok\n")
	} else {
		os.Stdout.WriteString("perturbed\n")
	}
}

// dstPreBubbleCleanupRan records whether the pre-bubble cleanup in
// DSTCleanupPreBubble has run yet.
var dstPreBubbleCleanupRan atomic.Bool

//go:noinline
func dstMakePreBubbleCleanup() {
	o := &dstFinObj{}
	runtime.AddCleanup(o, func(int) { dstPreBubbleCleanupRan.Store(true) }, 0)
}

// DSTCleanupPreBubble is the cleanup analogue of DSTFinPreBubble: the entry GC's
// pre-bubble cleanups run bubble-less in dstActivate, not in-bubble at the first
// quiescence. Prints "true"; without the dstActivate cleanup drain it is "false".
//
// dstForcePriorCleanupG first parks the cleanup goroutine (so it cannot itself run
// the pre-bubble cleanup during dstActivate — a freshly-created, still-runnable
// cleanup goroutine would, masking the dstActivate drain, just as fing being
// parked is why the finalizer analogue is testable). Requires GOMAXPROCS=1 so the
// park completes before the test object is created. The test object is then
// created after, kept alive until dstActivate's entry GC discovers it.
func DSTCleanupPreBubble() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	dstForcePriorCleanupG()   // park the cleanup goroutine
	dstMakePreBubbleCleanup() // test object, dead before simulation.Run's entry GC
	var atStart bool
	simulation.Run(n, func() {
		atStart = dstPreBubbleCleanupRan.Load()
	})
	os.Stdout.WriteString(strconv.FormatBool(atStart) + "\n")
}

// dstPreBubbleFinRan records whether the pre-bubble finalizer in DSTFinPreBubble
// has run yet.
var dstPreBubbleFinRan atomic.Bool

//go:noinline
func dstMakePreBubbleFin() {
	o := &dstFinObj{}
	runtime.SetFinalizer(o, func(p *dstFinObj) { dstPreBubbleFinRan.Store(true) })
}

// DSTFinPreBubble checks that the entry GC's *pre-bubble* finalizers run
// bubble-less in dstActivate, not in-bubble at the first quiescence (the M2 fix
// in dst.go). A finalizable object is created and dropped before simulation.Run, so
// dstActivate's entry GC discovers it dead. With the pre-bubble drain it runs
// DURING dstActivate (before f starts), so f's first read sees the flag set;
// without it, the finalizer would sit in finq (fing is gated under DST) and not
// run until the first in-bubble quiescence, so f's first read would see it unset.
// Runs with GOGC=off so no GC fires before simulation.Run (which would run the finalizer
// early on the ungated fing). Prints "true".
func DSTFinPreBubble() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	dstMakePreBubbleFin() // pre-bubble object: dead before simulation.Run's entry GC
	var atStart bool
	simulation.Run(n, func() {
		atStart = dstPreBubbleFinRan.Load() // M2: already true; without M2: false
	})
	os.Stdout.WriteString(strconv.FormatBool(atStart) + "\n")
}

// DSTWeakClearing checks that weak-pointer clearing is deterministic under DST
// (invariant DST-MEM-1, weak half): a single goroutine makes weak pointers to 256
// objects, drops the first half, then reaches quiescence; the quiescence STW GC
// clears exactly the dropped half's weak refs. Read in-bubble after the quiescence
// (time.Sleep) so the count reflects the in-run GC. Prints "cleared alive";
// identical across runs of the same seed, normal and -race (the cleared *set* is
// the dropped set regardless of per-cycle timing).
func DSTWeakClearing() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var gotCleared, gotAlive int
	simulation.Run(n, func() {
		const W = 256
		weaks := make([]weak.Pointer[dstFinObj], W)
		objs := make([]*dstFinObj, W)
		for i := range objs {
			objs[i] = &dstFinObj{}
			weaks[i] = weak.Make(objs[i])
		}
		for i := 0; i < W/2; i++ {
			objs[i] = nil // drop the first half; their weak refs should clear
		}
		time.Sleep(time.Millisecond) // quiescence → STW GC clears the dropped weaks
		for i := range weaks {
			if weaks[i].Value() == nil {
				gotCleared++
			} else {
				gotAlive++
			}
		}
		runtime.KeepAlive(objs) // keep the live half pinned past the count
	})
	os.Stdout.WriteString(strconv.Itoa(gotCleared) + " " + strconv.Itoa(gotAlive) + "\n")
}

// DSTGCOffBound checks that an allocating bubble is deterministically memory-
// bounded even with GOGC=off (invariant DST-MEM-2): the DST heap trigger falls
// back to the heapMinimum floor when gcPercent < 0 (mgc.go), so a GOGC=off bubble
// that churns ~20MB still triggers several STW GCs rather than growing unbounded.
// Prints NumGC; it is > 1 (memory bounded, not just the entry GC) and identical
// across runs. Run with GOGC=off. Without the floor, the trigger never fires under
// GOGC=off and NumGC stays at 1 (the dstActivate entry GC) while the heap grows
// without limit.
func DSTGCOffBound() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var ngc uint32
	simulation.Run(n, func() {
		for i := 0; i < 40000; i++ {
			b := make([]byte, 512)
			b[0] = byte(i)
			dstEscape(0, b)
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		ngc = ms.NumGC
	})
	os.Stdout.WriteString(strconv.FormatUint(uint64(ngc), 10) + "\n")
}

var dstChanPool = sync.Pool{New: func() any { return make(chan int, 1) }}

// DSTPoolAcrossRuns exercises a sync.Pool of channels reused across two simulation.Run
// calls — the pattern (e.g. Pebble's writeTaskPool) that fatals under plain
// synctest, because a channel created in one bubble is reused in the next. simulation.Run
// reaps pools when it returns, so the second run gets a fresh, bubble-local
// channel and the reuse succeeds. Without the reap, the second run would fatal
// with "send on synctest channel from outside bubble".
func DSTPoolAcrossRuns() {
	use := func(seed uint64, v int) int {
		var got int
		simulation.Run(seed, func() {
			ch := dstChanPool.Get().(chan int)
			ch <- v
			got = <-ch
			dstChanPool.Put(ch)
		})
		return got
	}
	use(1, 1)
	os.Stdout.WriteString("ok " + strconv.Itoa(use(2, 2)) + "\n")
}

// dstActivate is the low-level runtime entry that turns DST on and roots the
// calling goroutine's per-g RNG tree. These white-box tests use it directly
// (rather than simulation.Run) so they can exercise the per-g mechanism under
// GOMAXPROCS>1 M-migration, which Run (single-P) cannot reproduce.
//
//go:linkname dstActivate runtime.dstActivate
func dstActivate(seed uint64)

// dstActivateFromEnv activates DST with the seed in $DSTSEED, if set.
func dstActivateFromEnv() {
	if s := os.Getenv("DSTSEED"); s != "" {
		n, _ := strconv.ParseUint(s, 10, 64)
		dstActivate(n)
	}
}

// DSTRunDeterminism exercises the public simulation.Run API: it records a
// select order inside a deterministic simulation seeded by $DSTSEED. Run
// enforces GOMAXPROCS=1 and disables async/time preemption itself, so the test
// sets no other knobs. The same seed yields an identical sequence.
func DSTRunDeterminism() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var buf []byte
	simulation.Run(n, func() {
		buf = dstSelectSeq(64)
	})
	buf = append(buf, '\n')
	os.Stdout.Write(buf)
}

// dstSelectSeq drains four always-ready buffered channels via select, rounds
// times, returning the sequence of chosen case indices as digits.
func dstSelectSeq(rounds int) []byte {
	a := make(chan int, 1)
	b := make(chan int, 1)
	c := make(chan int, 1)
	d := make(chan int, 1)
	buf := make([]byte, 0, rounds*4)
	for r := 0; r < rounds; r++ {
		a <- 0
		b <- 1
		c <- 2
		d <- 3
		for k := 0; k < 4; k++ {
			var v int
			select {
			case v = <-a:
			case v = <-b:
			case v = <-c:
			case v = <-d:
			}
			buf = append(buf, byte('0'+v))
		}
	}
	return buf
}

// dstMeasuredBubble runs a synctest bubble that records 32 math/rand draws from
// the bubble's per-g stream. Because the per-g tree is re-rooted per bubble, the
// result is a function of the process seed only — independent of anything run
// before it in this process.
func dstMeasuredBubble() string {
	var s []byte
	synctest.Run(func() {
		const hexd = "0123456789abcdef"
		for i := 0; i < 32; i++ {
			s = append(s, hexd[rand.Uint64()&0xf])
		}
	})
	return string(s)
}

// DSTBubbleReproNoise runs a noise bubble (which advances the global goroutine
// tree) before the measured bubble. DSTBubbleReproPlain runs only the measured
// bubble. Their measured output must match: with per-bubble re-rooting the
// measured bubble does not inherit the caller's (now-advanced) tree position.
func DSTBubbleReproNoise() {
	dstActivateFromEnv()
	synctest.Run(func() {
		for i := 0; i < 10; i++ {
			_ = rand.Uint64()
		}
		done := make(chan struct{})
		go func() { close(done) }()
		<-done
	})
	os.Stdout.WriteString(dstMeasuredBubble() + "\n")
}

func DSTBubbleReproPlain() {
	dstActivateFromEnv()
	os.Stdout.WriteString(dstMeasuredBubble() + "\n")
}

// dstChurn starts background goroutines that keep several M's busy and bounces
// the calling goroutine across M's (via repeated block/resume), so a subsequent
// per-g-vs-per-m difference is observable. It returns a stop func. Requires
// GOMAXPROCS>1. The 6 child goroutines are created by the caller, so they
// advance the caller's own per-g stream deterministically.
func dstChurn() func() {
	const churn = 6
	stop := make(chan struct{})
	echo := make(chan chan int)
	for i := 0; i < churn; i++ {
		go func() {
			for {
				select {
				case <-stop:
					return
				case reply := <-echo:
					reply <- 1
				default:
					runtime.Gosched()
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		reply := make(chan int)
		echo <- reply
		<-reply
	}
	return func() { close(stop) }
}

// DSTMathRandChurn records a goroutine's math/rand/v2 global draws after
// migrating it across M's under churn. The math/rand globals are linkname'd to
// runtime.rand; with the per-g DST stream the sequence is identical across runs,
// with the per-m chacha8 stream it varies with which M the goroutine ran on.
func DSTMathRandChurn() {
	dstActivateFromEnv()
	stop := dstChurn()
	const hexd = "0123456789abcdef"
	buf := make([]byte, 0, 256+1)
	for i := 0; i < 256; i++ {
		buf = append(buf, hexd[rand.Uint64()&0xf])
	}
	stop()
	buf = append(buf, '\n')
	os.Stdout.Write(buf)
}

// DSTSelectChurn records a goroutine's select order after migrating it across
// OS threads (M's) under concurrent churn at GOMAXPROCS>1. With the per-g DST
// RNG the order depends only on this goroutine's logical history, so it is
// identical across process runs. With the per-m cheaprand stream it depends on
// which M the goroutine landed on and how many internal RNG draws happened
// there, so it varies run-to-run — this is the case the per-g tree fixes and
// that a single-goroutine no-load test cannot detect.
func DSTSelectChurn() {
	dstActivateFromEnv()
	stop := dstChurn()
	buf := dstSelectSeq(256)
	stop()
	buf = append(buf, '\n')
	os.Stdout.Write(buf)
}

// DSTMapOrder builds a map and prints its iteration order (two hex digits per
// key). Under DST the per-map seed and the iterator start offsets are drawn from
// this goroutine's per-g stream, so the order is a reproducible function of the
// seed, independent of which m runs the goroutine.
func DSTMapOrder() {
	dstActivateFromEnv()
	const n = 48
	m := make(map[int]int, n)
	for i := 0; i < n; i++ {
		m[i] = i
	}
	const hexd = "0123456789abcdef"
	buf := make([]byte, 0, n*2+1)
	for k := range m {
		buf = append(buf, hexd[(k>>4)&0xf], hexd[k&0xf])
	}
	buf = append(buf, '\n')
	os.Stdout.Write(buf)
}

// dstSink is a package-level sink so the compiler can't optimize the busy loop
// in dstBurnUntil away.
var dstSink uint64

//go:noinline
func dstBurnUntil(deadline time.Time) {
	// time.Now is a function call (a preemption point), so under the gate-off
	// case sysmon's poisoned stackguard0 is observed here.
	for time.Now().Before(deadline) {
		dstSink++
	}
}

// DSTNoPreempt verifies that under DST, sysmon does not time-preempt a running
// goroutine. At GOMAXPROCS=1 the watcher goroutine can only observe inBurst==1
// if the burst goroutine was preempted mid-burst — otherwise the burst runs to
// completion before it ever yields and the watcher only ever sees inBurst==0.
// With sysmon's time-based retake gated off (DST active, plus asyncpreemptoff=1
// and GOGC=off so no other preemption source fires) it prints "0"; without the
// gate, sysmon preempts the >forcePreemptNS burst and the watcher observes it,
// printing "1".
func DSTNoPreempt() {
	dstActivateFromEnv()
	var inBurst, ranDuringBurst, done int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for atomic.LoadInt32(&done) == 0 {
			if atomic.LoadInt32(&inBurst) == 1 {
				atomic.StoreInt32(&ranDuringBurst, 1)
			}
			runtime.Gosched()
		}
	}()

	// A burst far longer than sysmon's forcePreemptNS (10ms), so without the
	// DST gate sysmon would preempt it at least once.
	const burst = 60 * time.Millisecond
	atomic.StoreInt32(&inBurst, 1)
	dstBurnUntil(time.Now().Add(burst))
	atomic.StoreInt32(&inBurst, 0)

	atomic.StoreInt32(&done, 1)
	runtime.Gosched() // let the watcher observe done and exit
	wg.Wait()

	if atomic.LoadInt32(&ranDuringBurst) == 0 {
		os.Stdout.WriteString("0\n")
	} else {
		os.Stdout.WriteString("1\n")
	}
}

// DSTSelectOrder records a select order on a single non-blocking goroutine. With
// DST active (GOMAXPROCS=1, asyncpreemptoff=1, GOGC=off) the goroutine runs
// uninterrupted and the sequence is a reproducible function of the seed.
func DSTSelectOrder() {
	dstActivateFromEnv()
	buf := dstSelectSeq(256)
	buf = append(buf, '\n')
	os.Stdout.Write(buf)
}
