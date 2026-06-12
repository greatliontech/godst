// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"internal/synctest"
	"math/rand/v2"
	"net"
	"os"
	"os/user"
	"runtime"
	"runtime/metrics"
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
	register("DSTRunNestedGuard", DSTRunNestedGuard)
	register("DSTRunOverlapGuard", DSTRunOverlapGuard)
	register("DSTRunGOMAXPROCSPinned", DSTRunGOMAXPROCSPinned)
	register("DSTPoolAcrossRuns", DSTPoolAcrossRuns)
	register("DSTPooledFinalizerRunEnd", DSTPooledFinalizerRunEnd)
	register("DSTPooledCleanupRunEnd", DSTPooledCleanupRunEnd)
	register("DSTGCAllocBound", DSTGCAllocBound)
	register("DSTGCFinDiscovery", DSTGCFinDiscovery)
	register("DSTGCPerCycle", DSTGCPerCycle)
	register("DSTMemLimitPerCycle", DSTMemLimitPerCycle)
	register("DSTFinChanOp", DSTFinChanOp)
	register("DSTFinRunSet", DSTFinRunSet)
	register("DSTFinSpawn", DSTFinSpawn)
	register("DSTFinBlockedDrain", DSTFinBlockedDrain)
	register("DSTFinGoexitDrain", DSTFinGoexitDrain)
	register("DSTFinGoexitLedger", DSTFinGoexitLedger)
	register("DSTFinStuckDrainRunEnd", DSTFinStuckDrainRunEnd)
	register("DSTFinAbandonedChainReuse", DSTFinAbandonedChainReuse)
	register("DSTFinStuckDrainResidue", DSTFinStuckDrainResidue)
	register("DSTBubbleStreamIsolation", DSTBubbleStreamIsolation)
	register("DSTForeignBubbleIsolation", DSTForeignBubbleIsolation)
	register("DSTNonBubbleAllocTrigger", DSTNonBubbleAllocTrigger)
	register("DSTForeignCallbackDeferred", DSTForeignCallbackDeferred)
	register("DSTRunqOverflowOrder", DSTRunqOverflowOrder)
	register("DSTOvfFlushAtDeactivate", DSTOvfFlushAtDeactivate)
	register("DSTWhiteBoxCleanupChurnP4", DSTWhiteBoxCleanupChurnP4)
	register("DSTGOMAXPROCSEntryRace", DSTGOMAXPROCSEntryRace)
	register("DSTGOMAXPROCSDelayedSTW", DSTGOMAXPROCSDelayedSTW)
	register("DSTGOMAXPROCSAutoRestore", DSTGOMAXPROCSAutoRestore)
	register("DSTIdentityGroups", DSTIdentityGroups)
	register("DSTRunNoTag", DSTRunNoTag)
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
	register("DSTNet", DSTNet)
	register("DSTNetSemantics", DSTNetSemantics)
	register("DSTFinChain", DSTFinChain)
	register("DSTFinLongChain", DSTFinLongChain)
	register("DSTCleanupLongChain", DSTCleanupLongChain)
	register("DSTFinProfile", DSTFinProfile)
	register("DSTCleanupProfile", DSTCleanupProfile)
	register("DSTFinPreBubbleRelease", DSTFinPreBubbleRelease)
	register("DSTCleanupPreBubbleRelease", DSTCleanupPreBubbleRelease)
	register("DSTFinPreBubbleInFlight", DSTFinPreBubbleInFlight)
	register("DSTCleanupPreBubbleInFlight", DSTCleanupPreBubbleInFlight)
	register("DSTFinInFlightReleaseDuringRun", DSTFinInFlightReleaseDuringRun)
	register("DSTCleanupInFlightReleaseDuringRun", DSTCleanupInFlightReleaseDuringRun)
	register("DSTMemLimit", DSTMemLimit)
}

//go:linkname dstRuntimeActive runtime.dstActive
func dstRuntimeActive() bool

//go:linkname dstGOMAXPROCSAutoFP runtime.dstGOMAXPROCSAutoFP
func dstGOMAXPROCSAutoFP() bool

//go:linkname dstSchedOvfPutsFP runtime.dstSchedOvfPutsFP
func dstSchedOvfPutsFP() uint64

//go:linkname dstSetGOMAXPROCSSTWHook runtime.dstSetGOMAXPROCSSTWHook
func dstSetGOMAXPROCSSTWHook(f func())

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

// dstMakeFinChainLen builds a finalizer chain in a dropped frame: each
// finalizer keeps the next object alive, and the tail finalizer touches a bubble
// channel. Each object is reachable only through the previous object's pending
// finalizer, so the chain resolves one GC per level.
//
//go:noinline
func dstMakeFinChainLen(n int, ch chan int) {
	var next *dstFinObj
	for i := n - 1; i >= 0; i-- {
		cur := &dstFinObj{}
		if i == n-1 {
			runtime.SetFinalizer(cur, func(p *dstFinObj) { ch <- 99 })
		} else {
			hold := next
			runtime.SetFinalizer(cur, func(p *dstFinObj) { runtime.KeepAlive(hold) })
		}
		next = cur
	}
	runtime.KeepAlive(next)
}

func dstMakeFinChain(ch chan int) {
	dstMakeFinChainLen(3, ch)
}

//go:noinline
func dstMakeFinStoreChainLen(n int, done, active *atomic.Bool) {
	var next *dstFinObj
	for i := n - 1; i >= 0; i-- {
		cur := &dstFinObj{}
		if i == n-1 {
			runtime.SetFinalizer(cur, func(p *dstFinObj) {
				active.Store(dstRuntimeActive())
				done.Store(true)
			})
		} else {
			hold := next
			runtime.SetFinalizer(cur, func(p *dstFinObj) { runtime.KeepAlive(hold) })
		}
		next = cur
	}
	runtime.KeepAlive(next)
}

// DSTFinChain drops a finalizer chain whose tail touches a bubble channel and
// then returns immediately (no in-run quiescence to resolve it), exercising the
// Run-end fixpoint drain (design.md D4: Run-end fixpoint). Without the
// fixpoint, the tail's finalizer runs on post-teardown async fing
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

// DSTFinLongChain is DSTFinChain with a finite chain longer than the old run-end
// round cap. It prints "ok" iff the tail resolves while dstActive is still true.
var dstFinLongChainDone atomic.Bool
var dstFinLongChainActive atomic.Bool

func DSTFinLongChain() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	dstFinLongChainDone.Store(false)
	dstFinLongChainActive.Store(false)
	simulation.Run(n, func() {
		dstMakeFinStoreChainLen(300, &dstFinLongChainDone, &dstFinLongChainActive)
	})
	if dstFinLongChainDone.Load() && dstFinLongChainActive.Load() {
		os.Stdout.WriteString("ok\n")
	} else {
		os.Stdout.WriteString("tail-missing\n")
	}
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

// DSTNet exercises the in-memory deterministic network: inside simulation.Run a
// server Listens/Accepts and a client Dials over net's TCP API, exchange a
// request/response over the simulated connection, with the simulated addresses.
// With the real OS network this could not run deterministically (or at all in a
// sandbox); here it is a function of the seed. Prints a summary line; two runs (a
// and b) check same-seed determinism and that the per-run registry reset (b's
// Listen on the same address must not be "address already in use").
func DSTNet() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	exchange := func() string {
		var out string
		simulation.Run(n, func() {
			const addr = "10.0.0.1:9000"
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				out = "listen err: " + err.Error()
				return
			}
			done := make(chan string, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					done <- "accept err: " + err.Error()
					return
				}
				buf := make([]byte, 16)
				nr, _ := c.Read(buf)
				msg := string(buf[:nr])
				c.Write([]byte("echo:" + msg))
				from := c.RemoteAddr().String()
				c.Close()
				done <- "server saw " + msg + " from " + from
			}()
			client, err := net.Dial("tcp", addr)
			if err != nil {
				out = "dial err: " + err.Error()
				return
			}
			client.Write([]byte("ping"))
			buf := make([]byte, 32)
			nr, _ := client.Read(buf)
			resp := string(buf[:nr])
			srv := <-done
			out = "resp=" + resp + " local=" + client.LocalAddr().String() +
				" remote=" + client.RemoteAddr().String() + " | " + srv
		})
		return out
	}
	a := exchange()
	b := exchange()
	if a != b {
		os.Stdout.WriteString("DIVERGED\n a=" + a + "\n b=" + b + "\n")
		return
	}
	os.Stdout.WriteString(a + "\n")
}

// DSTNetSemantics checks that the DST network shim preserves public net semantics
// for the TCP surface it owns, and rejects protocol shapes it does not model.
// Prints booleans for: canceled/deadline DialContext errors, nil DialContext
// panics, unsupported UDP Listen/Dial errors, deterministic :0 listener ports,
// invalid ports, localhost canonicalization, DNS/service-name rejection, and
// tcp4/tcp6 address-family constraints.
func DSTNetSemantics() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var canceled, deadline, nilPanic, udpRejected, udpDialRejected, zeroPorts, invalidPort, localhost, dnsRejected, serviceRejected, familyRejected, wildcardFamilyRejected, tcp6WildcardRejected, tcp6Local, tcp4Unspecified bool
	simulation.Run(n, func() {
		ln, err := net.Listen("tcp", "127.0.0.1:9100")
		if err == nil {
			defer ln.Close()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			c, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:9100")
			if c != nil {
				c.Close()
			}
			canceled = errors.Is(err, context.Canceled)

			ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
			defer cancel()
			c, err = (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:9100")
			if c != nil {
				c.Close()
			}
			deadline = errors.Is(err, context.DeadlineExceeded)

			func() {
				defer func() { nilPanic = recover() != nil }()
				c, _ := (&net.Dialer{}).DialContext(nil, "tcp", "127.0.0.1:9100")
				if c != nil {
					c.Close()
				}
			}()
		}

		if ln, err := net.Listen("udp", "127.0.0.1:9101"); err != nil {
			udpRejected = true
		} else {
			ln.Close()
		}
		if c, err := net.Dial("udp", "127.0.0.1:9100"); err != nil {
			udpDialRejected = true
		} else {
			c.Close()
		}

		l1, err1 := net.Listen("tcp", "127.0.0.1:0")
		l2, err2 := net.Listen("tcp", "127.0.0.1:0")
		if err1 == nil && err2 == nil {
			p1 := l1.Addr().(*net.TCPAddr).Port
			p2 := l2.Addr().(*net.TCPAddr).Port
			zeroPorts = p1 == 10000 && p2 == 10001
		}
		if l1 != nil {
			l1.Close()
		}
		if l2 != nil {
			l2.Close()
		}

		if ln, err := net.Listen("tcp", "127.0.0.1:999999"); err != nil {
			invalidPort = true
		} else {
			ln.Close()
		}

		lnLocal, err := net.Listen("tcp", "127.0.0.1:9102")
		if err == nil {
			defer lnLocal.Close()
			c, err := net.Dial("tcp", "localhost:9102")
			if err == nil {
				localhost = true
				c.Close()
				if srv, err := lnLocal.Accept(); err == nil {
					srv.Close()
				}
			}
		}

		if c, err := net.Dial("tcp", "definitely-not-localhost.invalid:9102"); err != nil {
			dnsRejected = strings.Contains(err.Error(), "DNS lookup unsupported under deterministic simulation")
		} else {
			c.Close()
		}
		if ln, err := net.Listen("tcp", "127.0.0.1:http"); err != nil {
			serviceRejected = true
		} else {
			ln.Close()
		}

		if c, err := net.Dial("tcp6", "127.0.0.1:9102"); err != nil {
			familyRejected = true
		} else {
			c.Close()
		}
		if ln6, err := net.Listen("tcp6", "[::]:9103"); err == nil {
			if c, err := net.Dial("tcp4", "127.0.0.1:9103"); err != nil {
				wildcardFamilyRejected = true
			} else {
				c.Close()
			}
			ln6.Close()
		}
		if ln, err := net.Listen("tcp6", "0.0.0.0:9104"); err != nil {
			tcp6WildcardRejected = true
		} else {
			ln.Close()
		}
		if ln6, err := net.Listen("tcp6", "[::1]:9105"); err == nil {
			c, err := net.Dial("tcp6", "[::1]:9105")
			if err == nil {
				tcp6Local = c.LocalAddr().(*net.TCPAddr).IP.To4() == nil
				c.Close()
				if srv, err := ln6.Accept(); err == nil {
					srv.Close()
				}
			}
			ln6.Close()
		}
		if ln4, err := net.Listen("tcp4", "127.0.0.1:9106"); err == nil {
			c, err := net.Dial("tcp4", "[::]:9106")
			if err == nil {
				tcp4Unspecified = true
				c.Close()
				if srv, err := ln4.Accept(); err == nil {
					srv.Close()
				}
			}
			ln4.Close()
		}
	})
	simulation.Run(n, func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			zeroPorts = zeroPorts && ln.Addr().(*net.TCPAddr).Port == 10000
			ln.Close()
		} else {
			zeroPorts = false
		}
	})
	os.Stdout.WriteString("canceled=" + strconv.FormatBool(canceled) +
		" deadline=" + strconv.FormatBool(deadline) +
		" nilpanic=" + strconv.FormatBool(nilPanic) +
		" udpreject=" + strconv.FormatBool(udpRejected) +
		" udpdialreject=" + strconv.FormatBool(udpDialRejected) +
		" zeroports=" + strconv.FormatBool(zeroPorts) +
		" invalidport=" + strconv.FormatBool(invalidPort) +
		" localhost=" + strconv.FormatBool(localhost) +
		" dnsreject=" + strconv.FormatBool(dnsRejected) +
		" servicereject=" + strconv.FormatBool(serviceRejected) +
		" familyreject=" + strconv.FormatBool(familyRejected) +
		" wildcardfamilyreject=" + strconv.FormatBool(wildcardFamilyRejected) +
		" tcp6wildcardreject=" + strconv.FormatBool(tcp6WildcardRejected) +
		" tcp6local=" + strconv.FormatBool(tcp6Local) +
		" tcp4unspecified=" + strconv.FormatBool(tcp4Unspecified) + "\n")
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
	// Outside any run, crypto/rand is real entropy: two reads differ within
	// the process, DST must be fully deactivated, and the bytes must differ
	// ACROSS processes (the parent compares the out= field of two runs) — an
	// in-process x != y alone would pass even with a stuck-active
	// deterministic stream that merely advances.
	var x, y [16]byte
	crand.Read(x[:])
	crand.Read(y[:])
	os.Stdout.WriteString("h=" + hex.EncodeToString(a[:]) +
		" eq=" + strconv.FormatBool(a == b) +
		" seedvaries=" + strconv.FormatBool(a != c) +
		" realdiffers=" + strconv.FormatBool(x != y) +
		" active=" + strconv.FormatBool(dstRuntimeActive()) +
		" out=" + hex.EncodeToString(x[:]) + "\n")
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
// discovers a given object is ALSO contractual since the per-object trigger
// landed (per-cycle discovery determinism, design.md D1); that finer observable
// is exercised by DSTGCPerCycle / TestDSTGCPerCycleDiscoveryDeterministic.
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

// DSTMemLimitPerCycle exercises per-cycle finalizer discovery under
// Options.MemoryLimit without the finalizer-resurrection GC storm: a SMALL rate of
// finalizable objects (so the resurrected pile stays well under the limit) is
// interleaved with BULK non-finalizable garbage (single sink slot, so it dies
// immediately) that drives the memlimit crossings. The mid-run partial discovery
// count depends on the limit crossings; with the per-object dstHeapAlloc crossing
// it is a deterministic function of the seed (normal and -race). Prints
// "<partial> <total>"; under a tight limit the run ends mid-stream, so <total> is
// also a per-cycle (not set-level) observable — both fields must replay.
func DSTMemLimitPerCycle() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	limit, _ := strconv.ParseInt(os.Getenv("DSTMEMLIMIT"), 10, 64)
	var partial, total uint64
	simulation.RunWith(n, simulation.Options{MemoryLimit: limit}, func() {
		const NF, K, bulk = 4000, 512, 40
		ring := make([]*dstFinObj, K)
		for i := 0; i < NF; i++ {
			o := &dstFinObj{}
			o.b[0] = byte(i)
			runtime.SetFinalizer(o, func(p *dstFinObj) { _ = p.b[0] })
			ring[i%K] = o // evicted entries die with a finalizer set
			for j := 0; j < bulk; j++ {
				b := make([]byte, 256) // non-finalizable garbage, drives the memlimit
				b[0] = byte(j)
				dstEscape(0, b)
			}
			if i == NF/2 {
				partial = dstBubbleFinqFP()
			}
		}
		total = dstBubbleFinqFP()
	})
	os.Stdout.WriteString(strconv.FormatUint(partial, 10) + " " +
		strconv.FormatUint(total, 10) + "\n")
}

// DSTGCPerCycle exercises *per-cycle* finalizer discovery determinism (Tier 2,
// A.5 with the per-object dstHeapAlloc trigger). A single goroutine (no Seq-5
// interleaving axis) churns finalizable objects through a ring; at a fixed
// mid-run allocation it reads the bubble-local count of finalizers discovered so
// far (dstBubbleFinqFP). That partial count is a *per-cycle* observable: it
// depends on which GC cycles have fired by that allocation, i.e. on the trigger
// crossings — unlike the run-end total, which is set-level. Because the DST heap
// trigger fires on per-object allocated bytes (not span-granular heapLive), the
// crossings land at deterministic allocations, so the partial count is a
// deterministic function of the seed in normal AND -race builds, and identical
// across them. DSTBIGLIVE (MB) optionally pins a large live set so the
// GOGC-scaled target (not the floor) governs. Prints "<partial> <total>".
func DSTGCPerCycle() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	bigMB, _ := strconv.Atoi(os.Getenv("DSTBIGLIVE"))
	var partial, total uint64
	simulation.Run(n, func() {
		var big []*dstFinObj
		const dstFinObjSize = 256 // sizeof(dstFinObj): struct{ b [256]byte }
		for m := 0; m < bigMB*(1<<20)/dstFinObjSize; m++ {
			o := &dstFinObj{}
			o.b[0] = byte(m)
			big = append(big, o)
		}
		const N, K = 120000, 512
		ring := make([]*dstFinObj, K)
		for i := 0; i < N; i++ {
			o := &dstFinObj{}
			o.b[0] = byte(i)
			runtime.SetFinalizer(o, func(p *dstFinObj) { _ = p.b[0] })
			ring[i%K] = o // evicted entries die with a finalizer set
			if i == N/2 {
				partial = dstBubbleFinqFP() // per-cycle: discovered by the mid-run cycles
			}
		}
		total = dstBubbleFinqFP()
		runtime.KeepAlive(big)
	})
	os.Stdout.WriteString(strconv.FormatUint(partial, 10) + " " +
		strconv.FormatUint(total, 10) + "\n")
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

//go:noinline
func dstMakeFinActiveSender(ch chan int, active *atomic.Bool) {
	o := &dstFinObj{}
	runtime.SetFinalizer(o, func(p *dstFinObj) {
		active.Store(dstRuntimeActive())
		ch <- 42
	})
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

// dstMakeFinBlocked allocates two finalizable objects: one whose finalizer
// blocks receiving a value from ch, and one with a plain finalizer. The
// receive targets a value (non-nil sudog elem), so a spurious driver wake of
// the blocked drain is loud: releaseSudog throws "sudog with non-nil elem"
// instead of silently completing the receive with a zero value.
//
//go:noinline
func dstMakeFinBlocked(ch chan int, ranA, ranB *atomic.Bool) {
	a := &dstFinObj{}
	runtime.SetFinalizer(a, func(*dstFinObj) {
		v := <-ch
		ranA.Store(v == 1)
	})
	b := &dstFinObj{}
	runtime.SetFinalizer(b, func(*dstFinObj) {
		ranB.Store(true)
	})
}

// DSTFinBlockedDrain exercises the drain wake guard: a finalizer that
// blocks on a bubble channel parks the drain goroutine inside the channel wait,
// and a later quiescence with finalizer work still pending must NOT goready the
// drain there — it is woken by the goroutine that sends on the channel (design
// D4), never by the driver. Before the guard, the driver's wake corrupted the
// channel wait queue ("fatal error: runtime: sudog with non-nil elem"). Prints
// "done" when both finalizers ran and the Run completed.
func DSTFinBlockedDrain() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var ranA, ranB atomic.Bool
	simulation.Run(n, func() {
		ch := make(chan int)
		dstMakeFinBlocked(ch, &ranA, &ranB)
		runtime.GC() // queue both finalizers
		go func() {
			time.Sleep(2 * time.Millisecond)
			ch <- 1 // completes the blocked finalizer; the drain then loops and finishes
		}()
		// Quiescence 1 (this sleep): the drain is woken and blocks on ch inside
		// the blocking finalizer.
		time.Sleep(time.Millisecond)
		// Quiescence 2: finalizer work is still pending (the blocked one is
		// mid-run) and the drain is parked in the channel wait — the driver
		// must skip the wake.
		time.Sleep(time.Millisecond)
		// t+2ms: the send ran; the drain finished the rest of the queue.
		time.Sleep(time.Millisecond)
	})
	if !ranA.Load() || !ranB.Load() {
		os.Stdout.WriteString("missing finalizers\n")
		return
	}
	os.Stdout.WriteString("done\n")
}

// dstMakeFinGoexit allocates a finalizable object whose finalizer calls
// runtime.Goexit, killing the drain goroutine.
//
//go:noinline
func dstMakeFinGoexit() {
	a := &dstFinObj{}
	runtime.SetFinalizer(a, func(*dstFinObj) {
		runtime.Goexit()
	})
}

// dstMakeFinChanTouch allocates a finalizable object whose finalizer sends on a
// bubble channel.
//
//go:noinline
func dstMakeFinChanTouch(ch chan struct{}) {
	b := &dstFinObj{}
	runtime.SetFinalizer(b, func(*dstFinObj) {
		ch <- struct{}{}
	})
}

// DSTFinGoexitDrain exercises drain death by runtime.Goexit: the
// dying drain must be cleared so it is never goready'd again, and callbacks
// queued afterward — including bubble-channel-touching ones — must be
// deterministically discarded in-run rather than leaked to bubble-less async
// workers after deactivation. Before the fix, the next quiescence wake hit a
// dead g ("fatal error: bad g->status in ready"). Prints "done".
func DSTFinGoexitDrain() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	simulation.Run(n, func() {
		dstMakeFinGoexit()
		runtime.GC()
		time.Sleep(time.Millisecond) // quiescence: the finalizer Goexits the drain
		// The drain is dead. A later callback-bearing object that touches a
		// bubble channel must be discarded, not run on fing after the Run.
		ch := make(chan struct{}, 1)
		dstMakeFinChanTouch(ch)
		runtime.GC()
		time.Sleep(time.Millisecond) // quiescence with a dead drain: discard, don't wake
	})
	// Settle outside the run: if the bubble-channel finalizer leaked past
	// deactivation instead of being discarded, fing runs it here and fatals
	// ("send on synctest channel from outside bubble") before "done" prints.
	for range 3 {
		runtime.GC()
	}
	time.Sleep(200 * time.Millisecond)
	os.Stdout.WriteString("done\n")
}

// dstFinReadLedger returns the process-cumulative finalizer queued/executed
// counters. Read inside the bubble, where no async finalizer activity exists,
// so deltas around an in-run scenario are deterministic.
func dstFinReadLedger() (queued, executed uint64) {
	samples := []metrics.Sample{
		{Name: "/gc/finalizers/queued:finalizers"},
		{Name: "/gc/finalizers/executed:finalizers"},
	}
	metrics.Read(samples)
	return samples[0].Value.Uint64(), samples[1].Value.Uint64()
}

// dstMakeFinGoexitBatch allocates count finalizable objects in one batch (one
// GC cycle, one finBlock): the FIRST allocated gets a finalizer that calls
// runtime.Goexit, the rest are plain. Sequential allocation of same-sized
// objects fills a span in address order, and finalizer discovery walks specials
// in address order, so the Goexit entry sits first in the block — and
// runFinqBlocks runs entries last-to-first, so the plain finalizers run BEFORE
// the Goexit one. That ordering gives the ledger test its teeth: entries
// already run when the drain dies must already be accounted (per-entry), or
// queued != executed forever.
//
//go:noinline
func dstMakeFinGoexitBatch(count int, ran *atomic.Int64) {
	o := &dstFinObj{}
	runtime.SetFinalizer(o, func(*dstFinObj) {
		runtime.Goexit()
	})
	for i := 1; i < count; i++ {
		p := &dstFinObj{}
		runtime.SetFinalizer(p, func(*dstFinObj) {
			ran.Add(1)
		})
	}
}

// DSTFinGoexitLedger verifies the drain's finalizer queue ledger stays exact
// across a mid-block drain death: several plain finalizers run, then one calls
// runtime.Goexit, killing the drain with the block partially run. The
// already-run entries must be accounted per-entry and the unrun remainder
// accounted by the discard, so queued == executed afterwards — otherwise
// finPending() never clears and the Run-end fixpoint cannot terminate. Prints
// "done" plus the in-run ledger deltas.
func DSTFinGoexitLedger() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	const batch = 8
	var ran atomic.Int64
	var dq, de uint64
	simulation.Run(n, func() {
		q0, e0 := dstFinReadLedger()
		dstMakeFinGoexitBatch(batch, &ran)
		runtime.GC()
		time.Sleep(time.Millisecond) // quiescence: plain finalizers run, then Goexit kills the drain
		time.Sleep(time.Millisecond) // quiescence: dead drain — remainder discarded, ledger closed
		q1, e1 := dstFinReadLedger()
		dq, de = q1-q0, e1-e0
	})
	if dq != batch || de != batch {
		os.Stdout.WriteString("ledger mismatch: queued " + strconv.FormatUint(dq, 10) +
			" executed " + strconv.FormatUint(de, 10) + " ran " + strconv.FormatInt(ran.Load(), 10) + "\n")
		return
	}
	if ran.Load() != batch-1 {
		// The scenario's teeth require the plain finalizers to run BEFORE the
		// Goexit one (mid-block death with already-run entries). The ledger
		// check alone is order-independent; if the allocation/discovery order
		// assumption in dstMakeFinGoexitBatch ever drifts, fail loudly here
		// instead of letting the test go vacuous.
		os.Stdout.WriteString("order drift: ran " + strconv.FormatInt(ran.Load(), 10) +
			", want " + strconv.Itoa(batch-1) + "\n")
		return
	}
	os.Stdout.WriteString("done\n")
}

// DSTFinAbandonedChainReuse: run 1 ends with the drain parked forever inside a
// blocking finalizer (recovered deadlock panic), abandoning its published
// draining finalizer chain without dying. Run 2 then kills the drain from a
// CLEANUP with no finalizer work queued — the one death shape where the stale
// finalizer-chain pointer is never republished first — and the drain-death
// discard must not splice run 1's abandoned chain into run 2's ledger: that
// would leave finPending() true forever and hang run 2's end-of-run fixpoint.
// Prints "done".
func DSTFinAbandonedChainReuse() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	func() {
		defer func() { recover() }() // the deterministic synctest deadlock panic
		simulation.Run(n, func() {
			ch := make(chan int)
			var sink atomic.Bool
			dstMakeFinBlocked(ch, &sink, &sink) // nothing ever sends on ch
			runtime.GC()
			time.Sleep(time.Millisecond) // the drain blocks in the finalizer forever
		})
	}()
	simulation.Run(n+1, func() {
		dstMakeCleanupGoexit()
		runtime.GC()
		time.Sleep(time.Millisecond) // cleanup Goexit → discard; must not touch run 1's chain
	})
	os.Stdout.WriteString("done\n")
}

// dstMakeCleanupGoexit allocates an object whose cleanup calls runtime.Goexit,
// killing the drain goroutine from the cleanup (not finalizer) phase — the one
// death shape where dstDrainFinq found no work first and so never republished
// the (possibly stale) finalizer-chain pointer.
//
//go:noinline
func dstMakeCleanupGoexit() {
	o := &dstFinObj{}
	runtime.AddCleanup(o, func(int) { runtime.Goexit() }, 0)
}

// DSTFinStuckDrainRunEnd: a finalizer blocks forever on a bubble channel, so
// the drain is still parked inside the channel wait at Run end. The driver
// must NOT goready it there (that corrupts the channel wait queue); it leaves
// the drain parked and the total != 1 check reports the blocked bubble as a
// deterministic deadlock panic instead.
func DSTFinStuckDrainRunEnd() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	simulation.Run(n, func() {
		ch := make(chan int)
		var sink atomic.Bool
		dstMakeFinBlocked(ch, &sink, &sink) // nothing ever sends on ch
		runtime.GC()
		time.Sleep(time.Millisecond) // quiescence: the drain blocks in the finalizer forever
	})
	os.Stdout.WriteString("unreachable: Run returned with a stuck drain\n")
}

// DSTFinStuckDrainResidue: the drain is stuck forever inside a blocking
// finalizer at Run end, and the run has ALSO queued a bubble-channel-touching
// finalizer the stuck drain never reached. The Run-end path must discard that
// residue before deactivation reopens the async workers' gates — otherwise
// fing runs the bubble-stamped finalizer after the Run and fatals. Expects the
// deterministic deadlock panic from the stuck drain, then prints "done".
func DSTFinStuckDrainResidue() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	func() {
		defer func() { recover() }() // the deterministic synctest deadlock panic
		simulation.Run(n, func() {
			ch := make(chan int)
			var sink atomic.Bool
			dstMakeFinBlocked(ch, &sink, &sink) // nothing ever sends on ch
			runtime.GC()
			time.Sleep(time.Millisecond) // the drain blocks in the finalizer forever
			// Queue a bubble-channel finalizer the stuck drain will never run.
			ch2 := make(chan struct{}, 1)
			dstMakeFinChanTouch(ch2)
			runtime.GC()
			time.Sleep(time.Millisecond)
		})
	}()
	// Settle: a leaked bubble-stamped finalizer would fatal on fing here.
	for range 3 {
		runtime.GC()
	}
	time.Sleep(200 * time.Millisecond)
	os.Stdout.WriteString("done\n")
}

// dstMakeFinCryptoReader allocates a finalizable object whose finalizer reads
// 16 deterministic crypto/rand bytes (from the DRAIN goroutine's per-g stream)
// and sends them out hex-encoded.
//
//go:noinline
func dstMakeFinCryptoReader(out chan string) {
	o := &dstFinObj{}
	runtime.SetFinalizer(o, func(*dstFinObj) {
		b := make([]byte, 16)
		crand.Read(b)
		out <- hex.EncodeToString(b)
	})
}

// DSTBubbleStreamIsolation: the second goroutine the SUT main spawns must NOT
// share a per-g RNG stream with the finalizer drain. Without the salted
// bubble re-root, bubble.main replays the run caller's draw sequence (whose
// first two draws seeded bubble.main and the drain), so main's second child
// derives a stream bit-identical to the drain's: its first crypto/rand bytes
// equal the bytes a finalizer reads. Prints "done" when the streams differ.
func DSTBubbleStreamIsolation() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var g2hex, finhex string
	simulation.Run(n, func() {
		done1 := make(chan struct{})
		go func() { close(done1) }() // main's first child (draw #1)
		<-done1
		finCh := make(chan string, 1)
		dstMakeFinCryptoReader(finCh)
		g2res := make(chan string, 1)
		go func() { // main's second child (draw #2)
			b := make([]byte, 16)
			crand.Read(b)
			g2res <- hex.EncodeToString(b)
		}()
		g2hex = <-g2res
		runtime.GC()
		time.Sleep(time.Millisecond) // quiescence: the drain runs the finalizer
		finhex = <-finCh
	})
	if g2hex == "" || finhex == "" {
		os.Stdout.WriteString("missing streams\n")
		return
	}
	if g2hex == finhex {
		os.Stdout.WriteString("collision: " + g2hex + "\n")
		return
	}
	os.Stdout.WriteString("done\n")
}

// dstSchedFingerprint runs a fixed concurrent workload under simulation.Run
// and returns a string derived from its goroutine interleaving.
func dstSchedFingerprint(seed uint64) string {
	var out []byte
	simulation.Run(seed, func() {
		ch := make(chan int, 64)
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for r := 0; r < 8; r++ {
					ch <- id
					time.Sleep(time.Duration(id+1) * time.Microsecond)
				}
			}(i)
		}
		wg.Wait()
		close(ch)
		for v := range ch {
			out = append(out, byte('0'+v))
		}
	})
	return string(out)
}

// DSTForeignBubbleIsolation: a plain (non-simulation) synctest bubble running
// concurrently in the process — including bubbles CREATED while the simulation
// is live — must not perturb the simulation's schedule: foreign-bubble
// goroutines are scheduled RNG-free as infrastructure and a foreign
// synctest.Run must not claim the simulation's re-root path. Compares the
// schedule fingerprint with foreign bubbles churning against the fingerprint
// without. Prints "done" when equal.
func DSTForeignBubbleIsolation() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Repeated short-lived foreign bubbles: some are created while the
			// simulation is mid-run. Foreign bubbles are scheduled BEFORE the
			// simulation's goroutines (system-first, RNG-free), so the real
			// sleep between bubbles is what yields the P and lets the two
			// workloads actually overlap - a foreign bubble that is always
			// runnable would serialize ahead of the simulation instead.
			synctest.Run(func() {
				for i := 0; i < 20; i++ {
					time.Sleep(10 * time.Microsecond)
				}
			})
			time.Sleep(200 * time.Microsecond) // real sleep: outside any bubble
		}
	}()
	withForeign := dstSchedFingerprint(n)
	withForeign2 := dstSchedFingerprint(n)
	close(stop)
	<-done
	alone := dstSchedFingerprint(n)
	if withForeign != alone || withForeign2 != alone {
		os.Stdout.WriteString("schedule perturbed by foreign bubble\nwith1= " + withForeign +
			"\nwith2= " + withForeign2 + "\nalone= " + alone + "\n")
		return
	}
	os.Stdout.WriteString("done\n")
}

var dstAllocSink []byte

// dstOutsideAllocPump allocates a megabyte on a non-bubble goroutine for every
// ping received on an UNBUBBLED channel. A simulation goroutine sends the
// pings, so the outside allocations deterministically interleave with the run
// (a timer-driven outside loop would not: real timers are not serviced while
// the simulation has runnable work).
func dstOutsideAllocPump(ping chan struct{}, done *sync.WaitGroup) {
	defer done.Done()
	for range ping {
		dstAllocSink = make([]byte, 1<<20)
	}
}

// DSTNonBubbleAllocTrigger: allocations by goroutines outside the simulation
// bubble must not advance the deterministic GC trigger - otherwise NumGC and
// which cycle discovers a finalizer depend on unrelated process activity.
// Runs the same allocation-heavy SUT twice with an outside allocator churning
// and once without; all three NumGC deltas must be equal. Prints "done".
func DSTNonBubbleAllocTrigger() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	gcs := func(ping chan struct{}) uint32 {
		var delta uint32
		simulation.Run(n, func() {
			var m0, m1 runtime.MemStats
			runtime.ReadMemStats(&m0)
			for i := 0; i < 200; i++ {
				dstAllocSink = make([]byte, 64<<10)
				if ping != nil {
					ping <- struct{}{} // non-durable op on an unbubbled buffered channel
				}
				if i%20 == 0 {
					time.Sleep(time.Millisecond)
				}
			}
			runtime.ReadMemStats(&m1)
			delta = m1.NumGC - m0.NumGC
		})
		return delta
	}
	ping := make(chan struct{}, 256)
	var wg sync.WaitGroup
	wg.Add(1)
	go dstOutsideAllocPump(ping, &wg)
	d1 := gcs(ping)
	d2 := gcs(ping)
	close(ping)
	wg.Wait()
	alone := gcs(nil)
	if d1 != alone || d2 != alone {
		os.Stdout.WriteString("numgc perturbed: with=" + strconv.FormatUint(uint64(d1), 10) +
			"," + strconv.FormatUint(uint64(d2), 10) +
			" alone=" + strconv.FormatUint(uint64(alone), 10) + "\n")
		return
	}
	os.Stdout.WriteString("done\n")
}

// DSTGOMAXPROCSAutoRestore: in a process whose GOMAXPROCS is in container-aware
// auto mode, the simulation pin must set the custom flag for the duration of
// the run (that is what blocks the sysmon auto-updater) and restore AUTO mode
// afterward - not leave the process pinned to a stale snapshot forever.
// Prints "custom" (parent skips) when the process starts in custom mode.
func DSTGOMAXPROCSAutoRestore() {
	if !dstGOMAXPROCSAutoFP() {
		os.Stdout.WriteString("custom\n")
		return
	}
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	inRunAuto := true
	simulation.Run(n, func() {
		inRunAuto = dstGOMAXPROCSAutoFP()
	})
	os.Stdout.WriteString("inrun=" + strconv.FormatBool(inRunAuto) +
		" after=" + strconv.FormatBool(dstGOMAXPROCSAutoFP()) + "\n")
}

// DSTIdentityGroups exercises the simulated group list and the minimal
// simulated user/group database: inside a run, os.Getgroups is exactly the
// simulated gid; os/user lookups resolve the simulated user/group by name and
// id and report anything else deterministically unknown; GroupIds of the
// simulated user is its single group. After the run the host values return.
func DSTIdentityGroups() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	hostGroups, _ := os.Getgroups()
	fail := func(msg string) {
		os.Stdout.WriteString(msg + "\n")
	}
	okAll := true
	simulation.Run(n, func() {
		check := func(cond bool, msg string) {
			if !cond {
				okAll = false
				fail(msg)
			}
		}
		gids, err := os.Getgroups()
		check(err == nil && len(gids) == 1 && gids[0] == 7777, "getgroups not [7777]")
		u, err := user.Lookup("sim")
		check(err == nil && u != nil && u.Uid == "7777" && u.HomeDir == "/home/sim", "Lookup(sim) wrong")
		_, err = user.Lookup("nosuchuser")
		var uue user.UnknownUserError
		check(errors.As(err, &uue), "Lookup(nosuchuser) not UnknownUserError")
		u2, err := user.LookupId("7777")
		check(err == nil && u2 != nil && u2.Username == "sim", "LookupId(7777) wrong")
		var uuie user.UnknownUserIdError
		_, err = user.LookupId("1000")
		check(errors.As(err, &uuie), "LookupId(1000) not UnknownUserIdError")
		g, err := user.LookupGroup("sim")
		check(err == nil && g != nil && g.Gid == "7777", "LookupGroup(sim) wrong")
		var uge user.UnknownGroupError
		_, err = user.LookupGroup("wheel")
		check(errors.As(err, &uge), "LookupGroup(wheel) not UnknownGroupError")
		g2, err := user.LookupGroupId("7777")
		check(err == nil && g2 != nil && g2.Name == "sim", "LookupGroupId(7777) wrong")
		if u != nil {
			ids, err := u.GroupIds()
			check(err == nil && len(ids) == 1 && ids[0] == "7777", "GroupIds(sim) wrong")
		}
		other := &user.User{Username: "app", Gid: "1000"}
		ids2, err := other.GroupIds()
		check(err == nil && len(ids2) == 1 && ids2[0] == "1000", "GroupIds(other) not primary gid")
	})
	after, _ := os.Getgroups()
	restored := len(after) == len(hostGroups)
	if restored {
		for i := range after {
			if after[i] != hostGroups[i] {
				restored = false
				break
			}
		}
	}
	if !restored {
		fail("host groups not restored")
		return
	}
	if okAll {
		os.Stdout.WriteString("done\n")
	}
}

// DSTRunNoTag exercises the documented build-constraint panic: in a binary
// built WITHOUT -tags dst, simulation.Run must refuse to start (the map hash
// key cannot be made deterministic at runtime). Prints the recovered panic.
func DSTRunNoTag() {
	defer func() {
		if v := recover(); v != nil {
			if s, ok := v.(string); ok {
				os.Stdout.WriteString("panic: " + s + "\n")
				return
			}
			os.Stdout.WriteString("panic: non-string\n")
			return
		}
		os.Stdout.WriteString("no panic\n")
	}()
	simulation.Run(1, func() {})
}

// DSTFinProfile takes a goroutine profile from inside a finalizer running on the
// DST drain. The drain is already a user goroutine, so it must not also be counted
// through fingRunningFinalizer's synthetic profile adjustment. Prints "ok" iff
// the profile count matches the number of populated records.
func DSTFinProfile() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var ok bool
	simulation.Run(n, func() {
		ch := make(chan bool, 1)
		o := &dstFinObj{}
		runtime.SetFinalizer(o, func(p *dstFinObj) {
			n, _ := runtime.GoroutineProfile(nil)
			records := make([]runtime.StackRecord, n)
			n, profOK := runtime.GoroutineProfile(records)
			populated := 0
			for i := 0; i < n && i < len(records); i++ {
				if len(records[i].Stack()) != 0 {
					populated++
				}
			}
			ch <- profOK && populated == n
		})
		runtime.KeepAlive(o)
		ok = <-ch
	})
	if ok {
		os.Stdout.WriteString("ok\n")
	} else {
		os.Stdout.WriteString("overcount\n")
	}
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
		// would let the Run-end closeout drain run any missed finalizers and launder
		// the count.
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

//go:noinline
func dstMakeCleanupActiveSender(ch chan int, active *atomic.Bool) {
	o := &dstFinObj{}
	runtime.AddCleanup(o, func(c chan int) {
		active.Store(dstRuntimeActive())
		c <- 42
	}, ch)
}

//go:noinline
func dstMakeCleanupChainLen(n int, ch chan int) {
	var next *dstFinObj
	for i := n - 1; i >= 0; i-- {
		cur := &dstFinObj{}
		if i == n-1 {
			runtime.AddCleanup(cur, func(c chan int) { c <- 99 }, ch)
		} else {
			hold := next
			runtime.AddCleanup(cur, func(p *dstFinObj) { runtime.KeepAlive(p) }, hold)
		}
		next = cur
	}
	runtime.KeepAlive(next)
}

//go:noinline
func dstMakeCleanupStoreChainLen(n int, done, active *atomic.Bool) {
	var next *dstFinObj
	for i := n - 1; i >= 0; i-- {
		cur := &dstFinObj{}
		if i == n-1 {
			runtime.AddCleanup(cur, func(b *atomic.Bool) {
				active.Store(dstRuntimeActive())
				b.Store(true)
			}, done)
		} else {
			hold := next
			runtime.AddCleanup(cur, func(p *dstFinObj) { runtime.KeepAlive(p) }, hold)
		}
		next = cur
	}
	runtime.KeepAlive(next)
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

// DSTCleanupLongChain is the cleanup analogue of DSTFinLongChain.
var dstCleanupLongChainDone atomic.Bool
var dstCleanupLongChainActive atomic.Bool

func DSTCleanupLongChain() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	dstCleanupLongChainDone.Store(false)
	dstCleanupLongChainActive.Store(false)
	simulation.Run(n, func() {
		dstMakeCleanupStoreChainLen(300, &dstCleanupLongChainDone, &dstCleanupLongChainActive)
	})
	if dstCleanupLongChainDone.Load() && dstCleanupLongChainActive.Load() {
		os.Stdout.WriteString("ok\n")
	} else {
		os.Stdout.WriteString("tail-missing\n")
	}
}

// DSTCleanupProfile is the cleanup analogue of DSTFinProfile. A cleanup running
// on synctestGCDrain must not be counted both as the drain goroutine and as a
// synthetic running cleanup goroutine.
func DSTCleanupProfile() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var ok bool
	simulation.Run(n, func() {
		ch := make(chan bool, 1)
		o := &dstFinObj{}
		runtime.AddCleanup(o, func(c chan bool) {
			n, _ := runtime.GoroutineProfile(nil)
			records := make([]runtime.StackRecord, n)
			n, profOK := runtime.GoroutineProfile(records)
			populated := 0
			for i := 0; i < n && i < len(records); i++ {
				if len(records[i].Stack()) != 0 {
					populated++
				}
			}
			c <- profOK && populated == n
		}, ch)
		runtime.KeepAlive(o)
		ok = <-ch
	})
	if ok {
		os.Stdout.WriteString("ok\n")
	} else {
		os.Stdout.WriteString("overcount\n")
	}
}

var dstCleanupRunCount atomic.Uint64
var dstCleanupRunSum atomic.Uint64

// DSTCleanupRunSet is the cleanup analogue of DSTFinRunSet (invariant
// DST-CLEANUP-2): the set of cleanups run by the in-bubble drain is deterministic
// and they actually run. Counters are read inside f after an intervening
// quiescence so the Run-end closeout drain cannot launder the count. Prints
// "count sumHex".
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

// dstPreBubbleCleanupHead/Tail record whether the cleanup chain callbacks in
// DSTCleanupPreBubble have run yet, and the Active flags record whether they
// observed dstActive while running.
var dstPreBubbleCleanupHead atomic.Bool
var dstPreBubbleCleanupTail atomic.Bool
var dstPreBubbleCleanupHeadActive atomic.Bool
var dstPreBubbleCleanupActive atomic.Bool

//go:noinline
func dstMakePreBubbleCleanupChain() {
	tail := &dstFinObj{}
	runtime.AddCleanup(tail, func(int) {
		dstPreBubbleCleanupActive.Store(dstRuntimeActive())
		dstPreBubbleCleanupTail.Store(true)
	}, 0)
	head := &dstFinObj{}
	runtime.AddCleanup(head, func(p *dstFinObj) {
		dstPreBubbleCleanupHeadActive.Store(dstRuntimeActive())
		dstPreBubbleCleanupHead.Store(true)
		runtime.KeepAlive(p)
	}, tail)
	runtime.KeepAlive(head)
}

// DSTCleanupPreBubble checks that cleanups queued before a run do not execute in
// that run's bubble. The head cleanup keeps the tail alive; the tail may run
// before the run or after it, but neither callback may flip during the in-run
// quiescence or observe dstActive. Prints "headStart tailStart headAfter
// tailAfter headActive tailActive".
func DSTCleanupPreBubble() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	dstMakePreBubbleCleanupChain() // dead before simulation.Run's entry GC
	var headStart, tailStart, headAfter, tailAfter, headActive, tailActive bool
	simulation.Run(n, func() {
		headStart = dstPreBubbleCleanupHead.Load()
		tailStart = dstPreBubbleCleanupTail.Load()
		time.Sleep(time.Millisecond)
		headAfter = dstPreBubbleCleanupHead.Load()
		tailAfter = dstPreBubbleCleanupTail.Load()
		headActive = dstPreBubbleCleanupHeadActive.Load()
		tailActive = dstPreBubbleCleanupActive.Load()
	})
	os.Stdout.WriteString(strconv.FormatBool(headStart) + " " +
		strconv.FormatBool(tailStart) + " " +
		strconv.FormatBool(headAfter) + " " +
		strconv.FormatBool(tailAfter) + " " +
		strconv.FormatBool(headActive) + " " +
		strconv.FormatBool(tailActive) + "\n")
}

var dstPreBubbleReleaseCh chan struct{}
var dstPreBubbleFinReleaseCount atomic.Uint64
var dstPreBubbleCleanupReleaseCount atomic.Uint64

//go:noinline
func dstMakeBlockingPreBubbleFinalizers(n int) {
	ch := dstPreBubbleReleaseCh
	for i := 0; i < n; i++ {
		o := &dstFinObj{}
		runtime.SetFinalizer(o, func(p *dstFinObj) {
			<-ch
			dstPreBubbleFinReleaseCount.Add(1)
		})
	}
}

//go:noinline
func dstMakeBlockingPreBubbleCleanups(n int) {
	ch := dstPreBubbleReleaseCh
	for i := 0; i < n; i++ {
		o := &dstFinObj{}
		runtime.AddCleanup(o, func(c chan struct{}) {
			<-c
			dstPreBubbleCleanupReleaseCount.Add(1)
		}, ch)
	}
}

func dstWaitAtomicCount(c *atomic.Uint64, want uint64) bool {
	for i := 0; i < 1000; i++ {
		if c.Load() == want {
			return true
		}
		runtime.GC()
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	return c.Load() == want
}

// DSTFinPreBubbleRelease checks that finalizers detached before dstActive are
// released back to fing after dstDeactivate. The dstPreparing gate keeps these
// callbacks from starting during activation, so reaching the count requires the
// deferred blocks to be linked back after the run.
func DSTFinPreBubbleRelease() {
	const nfinalizers = 32
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	dstPreBubbleReleaseCh = make(chan struct{})
	dstPreBubbleFinReleaseCount.Store(0)
	dstMakeBlockingPreBubbleFinalizers(nfinalizers)
	simulation.Run(n, func() {})
	close(dstPreBubbleReleaseCh)
	if dstWaitAtomicCount(&dstPreBubbleFinReleaseCount, nfinalizers) {
		os.Stdout.WriteString("ok\n")
	} else {
		os.Stdout.WriteString("count=" + strconv.FormatUint(dstPreBubbleFinReleaseCount.Load(), 10) + "\n")
	}
}

// DSTCleanupPreBubbleRelease is the cleanup analogue of DSTFinPreBubbleRelease.
func DSTCleanupPreBubbleRelease() {
	const ncleanups = 32
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	dstPreBubbleReleaseCh = make(chan struct{})
	dstPreBubbleCleanupReleaseCount.Store(0)
	dstMakeBlockingPreBubbleCleanups(ncleanups)
	simulation.Run(n, func() {})
	close(dstPreBubbleReleaseCh)
	if dstWaitAtomicCount(&dstPreBubbleCleanupReleaseCount, ncleanups) {
		os.Stdout.WriteString("ok\n")
	} else {
		os.Stdout.WriteString("count=" + strconv.FormatUint(dstPreBubbleCleanupReleaseCount.Load(), 10) + "\n")
	}
}

// DSTFinPreBubbleInFlight checks that a finalizer already running on fing before
// simulation.Run does not keep the run-end drain spinning on process-global
// finqueued/finexecuted counts.
func DSTFinPreBubbleInFlight() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	o := &dstFinObj{}
	runtime.SetFinalizer(o, func(p *dstFinObj) {
		started <- struct{}{}
		<-release
	})
	runtime.KeepAlive(o)
	runtime.GC()
	<-started
	simulation.Run(n, func() {})
	close(release)
	os.Stdout.WriteString("ok\n")
}

// DSTCleanupPreBubbleInFlight is the cleanup analogue of
// DSTFinPreBubbleInFlight.
func DSTCleanupPreBubbleInFlight() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	o := &dstFinObj{}
	runtime.AddCleanup(o, func(c chan struct{}) {
		started <- struct{}{}
		<-c
	}, release)
	runtime.KeepAlive(o)
	runtime.GC()
	<-started
	simulation.Run(n, func() {})
	close(release)
	os.Stdout.WriteString("ok\n")
}

// DSTFinInFlightReleaseDuringRun checks that an already-running async fing
// callback released during a Run parks before dequeuing in-run work. Without the
// worker-loop gate, fing can steal the in-run finalizer after release and fatal on
// the bubble channel send.
func DSTFinInFlightReleaseDuringRun() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	o := &dstFinObj{}
	runtime.SetFinalizer(o, func(p *dstFinObj) {
		started <- struct{}{}
		<-release
	})
	runtime.KeepAlive(o)
	runtime.GC()
	<-started
	var got bool
	simulation.Run(n, func() {
		ch := make(chan int, 1)
		var active atomic.Bool
		dstMakeFinActiveSender(ch, &active)
		runtime.GC()
		close(release)
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case v := <-ch:
			got = v == 42 && active.Load()
		default:
		}
	})
	if got {
		os.Stdout.WriteString("ok\n")
	} else {
		os.Stdout.WriteString("miss\n")
	}
}

// DSTCleanupInFlightReleaseDuringRun is the cleanup analogue of
// DSTFinInFlightReleaseDuringRun.
func DSTCleanupInFlightReleaseDuringRun() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	o := &dstFinObj{}
	runtime.AddCleanup(o, func(c chan struct{}) {
		started <- struct{}{}
		<-c
	}, release)
	runtime.KeepAlive(o)
	runtime.GC()
	<-started
	var got bool
	simulation.Run(n, func() {
		ch := make(chan int, 1)
		var active atomic.Bool
		dstMakeCleanupActiveSender(ch, &active)
		runtime.GC()
		close(release)
		runtime.Gosched()
		time.Sleep(time.Millisecond)
		select {
		case v := <-ch:
			got = v == 42 && active.Load()
		default:
		}
	})
	if got {
		os.Stdout.WriteString("ok\n")
	} else {
		os.Stdout.WriteString("miss\n")
	}
}

// dstPreBubbleFinHead/Tail record whether the finalizer chain callbacks in
// DSTFinPreBubble have run yet, and the Active flags record whether they observed
// dstActive while running.
var dstPreBubbleFinHead atomic.Bool
var dstPreBubbleFinTail atomic.Bool
var dstPreBubbleFinHeadActive atomic.Bool
var dstPreBubbleFinActive atomic.Bool

//go:noinline
func dstMakePreBubbleFinChain() {
	tail := &dstFinObj{}
	runtime.SetFinalizer(tail, func(p *dstFinObj) {
		dstPreBubbleFinActive.Store(dstRuntimeActive())
		dstPreBubbleFinTail.Store(true)
	})
	head := &dstFinObj{}
	runtime.SetFinalizer(head, func(p *dstFinObj) {
		dstPreBubbleFinHeadActive.Store(dstRuntimeActive())
		dstPreBubbleFinHead.Store(true)
		runtime.KeepAlive(tail)
	})
	runtime.KeepAlive(head)
}

// DSTFinPreBubble checks that finalizers queued before a run do not execute in
// that run's bubble. The head finalizer keeps the tail alive; the tail may run
// before the run or after it, but neither callback may flip during the in-run
// quiescence or observe dstActive. Prints "headStart tailStart headAfter
// tailAfter headActive tailActive".
func DSTFinPreBubble() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	dstMakePreBubbleFinChain() // pre-bubble object: dead before simulation.Run's entry GC
	var headStart, tailStart, headAfter, tailAfter, headActive, tailActive bool
	simulation.Run(n, func() {
		headStart = dstPreBubbleFinHead.Load()
		tailStart = dstPreBubbleFinTail.Load()
		time.Sleep(time.Millisecond)
		headAfter = dstPreBubbleFinHead.Load()
		tailAfter = dstPreBubbleFinTail.Load()
		headActive = dstPreBubbleFinHeadActive.Load()
		tailActive = dstPreBubbleFinActive.Load()
	})
	os.Stdout.WriteString(strconv.FormatBool(headStart) + " " +
		strconv.FormatBool(tailStart) + " " +
		strconv.FormatBool(headAfter) + " " +
		strconv.FormatBool(tailAfter) + " " +
		strconv.FormatBool(headActive) + " " +
		strconv.FormatBool(tailActive) + "\n")
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

var dstPooledFinalizerPool sync.Pool
var dstPooledCleanupPool sync.Pool

type dstPooledCallback struct {
	ch     chan int
	active *atomic.Bool
	done   *atomic.Bool
}

//go:noinline
func dstPutPooledFinalizer(cb dstPooledCallback) {
	o := &dstFinObj{}
	runtime.SetFinalizer(o, func(p *dstFinObj) {
		cb.active.Store(dstRuntimeActive())
		cb.ch <- 1
		cb.done.Store(true)
	})
	dstPooledFinalizerPool.Put(o)
}

//go:noinline
func dstPutPooledFinalizerThenPut(cb dstPooledCallback) {
	o := &dstFinObj{}
	runtime.SetFinalizer(o, func(p *dstFinObj) {
		dstPutPooledFinalizer(cb)
	})
	dstPooledFinalizerPool.Put(o)
}

//go:noinline
func dstPutPooledCleanup(cb dstPooledCallback) {
	o := &dstFinObj{}
	runtime.AddCleanup(o, func(cb dstPooledCallback) {
		cb.active.Store(dstRuntimeActive())
		cb.ch <- 1
		cb.done.Store(true)
	}, cb)
	dstPooledCleanupPool.Put(o)
}

//go:noinline
func dstPutPooledCleanupThenPut(cb dstPooledCallback) {
	o := &dstFinObj{}
	runtime.AddCleanup(o, func(cb dstPooledCallback) {
		dstPutPooledCleanup(cb)
	}, cb)
	dstPooledCleanupPool.Put(o)
}

//go:noinline
func dstPutPooledFinalizerFromFinalizer(cb dstPooledCallback) {
	o := &dstFinObj{}
	runtime.SetFinalizer(o, func(p *dstFinObj) {
		dstPutPooledFinalizerThenPut(cb)
	})
}

//go:noinline
func dstPutPooledCleanupFromCleanup(cb dstPooledCallback) {
	o := &dstFinObj{}
	runtime.AddCleanup(o, func(cb dstPooledCallback) {
		dstPutPooledCleanupThenPut(cb)
	}, cb)
}

// DSTPooledFinalizerRunEnd has a run-end finalizer put a pooled finalizer that
// itself puts another pooled finalizer. The tail finalizer touches a bubble
// channel, so dstStopGCDrain must reset its two-generation pool-reap window after
// the intermediate pooled callback runs.
func DSTPooledFinalizerRunEnd() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var active, done atomic.Bool
	simulation.Run(n, func() {
		ch := make(chan int, 1)
		dstPutPooledFinalizerFromFinalizer(dstPooledCallback{ch: ch, active: &active, done: &done})
	})
	if done.Load() && active.Load() {
		os.Stdout.WriteString("ok\n")
	} else {
		os.Stdout.WriteString("done=" + strconv.FormatBool(done.Load()) +
			" active=" + strconv.FormatBool(active.Load()) + "\n")
	}
}

// DSTPooledCleanupRunEnd is the cleanup analogue of DSTPooledFinalizerRunEnd.
func DSTPooledCleanupRunEnd() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	var active, done atomic.Bool
	simulation.Run(n, func() {
		ch := make(chan int, 1)
		dstPutPooledCleanupFromCleanup(dstPooledCallback{ch: ch, active: &active, done: &done})
	})
	if done.Load() && active.Load() {
		os.Stdout.WriteString("ok\n")
	} else {
		os.Stdout.WriteString("done=" + strconv.FormatBool(done.Load()) +
			" active=" + strconv.FormatBool(active.Load()) + "\n")
	}
}

// DSTPoolAcrossRuns exercises a sync.Pool of channels reused across two simulation.Run
// calls — a pattern (a pool of channels reused across runs) that fatals under plain
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

func dstPanicText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case error:
		return x.Error()
	default:
		return "non-string panic"
	}
}

func dstPanicContains(want string, f func()) (ok bool) {
	defer func() {
		if v := recover(); v != nil {
			ok = strings.Contains(dstPanicText(v), want)
		}
	}()
	f()
	return false
}

// DSTRunNestedGuard verifies a nested Run is rejected before mutating the outer
// run's global DST state. Without the preflight guard, the inner Run activates DST
// and then synctest.Run panics; its defer deactivates DST while the outer bubble
// continues.
func DSTRunNestedGuard() {
	var nestedOK, active bool
	var pid int
	simulation.Run(1, func() {
		nestedOK = dstPanicContains("testing/simulation: Run called from within a synctest bubble", func() {
			simulation.Run(2, func() {})
		})
		active = dstRuntimeActive()
		pid = os.Getpid()
	})
	os.Stdout.WriteString("nested=" + strconv.FormatBool(nestedOK) +
		" active=" + strconv.FormatBool(active) +
		" pid=" + strconv.Itoa(pid) + "\n")
}

// DSTRunOverlapGuard verifies a second top-level Run cannot overlap an active
// Run and corrupt its process-global DST state.
func DSTRunOverlapGuard() {
	started := make(chan struct{})
	done := make(chan bool, 1)
	var active atomic.Bool
	go func() {
		<-started
		done <- dstPanicContains("testing/simulation: Run called while another simulation operation is active", func() {
			simulation.Run(2, func() {})
		})
	}()
	simulation.Run(1, func() {
		close(started)
		time.Sleep(time.Millisecond)
		active.Store(dstRuntimeActive())
	})
	os.Stdout.WriteString("overlap=" + strconv.FormatBool(<-done) +
		" active=" + strconv.FormatBool(active.Load()) + "\n")
}

func DSTRunGOMAXPROCSPinned() {
	var oldSet, afterSet, afterDefault int
	var autoAfterDefault bool
	before := runtime.GOMAXPROCS(0)
	simulation.Run(1, func() {
		oldSet = runtime.GOMAXPROCS(2)
		afterSet = runtime.GOMAXPROCS(0)
		runtime.SetDefaultGOMAXPROCS()
		afterDefault = runtime.GOMAXPROCS(0)
		autoAfterDefault = dstGOMAXPROCSAutoFP()
	})
	after := runtime.GOMAXPROCS(0)
	os.Stdout.WriteString("before=" + strconv.Itoa(before) +
		" old=" + strconv.Itoa(oldSet) +
		" afterSet=" + strconv.Itoa(afterSet) +
		" afterDefault=" + strconv.Itoa(afterDefault) +
		" auto=" + strconv.FormatBool(autoAfterDefault) +
		" restored=" + strconv.Itoa(after) + "\n")
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

var dstForeignFinRan, dstForeignCleanupRan atomic.Int64
var dstSimFinRan, dstSimCleanupRan atomic.Bool

// dstForeignRegisterPump registers a finalizer and a cleanup on garbage objects
// from a NON-bubble goroutine, one round per ping, acking each round so the
// simulation knows the registration (and the objects' death) happened before it
// continues. The callbacks must NOT run during the run (ownership: they were
// registered outside the simulation bubble, so they are process-level work the
// drain must not execute) and ALL of them must run after it (released to the
// async pool — exact counts, so losing part of the deferred chain, e.g. an
// unflushed partial block, is caught).
func dstForeignRegisterPump(ping, ack chan struct{}, done *sync.WaitGroup) {
	defer done.Done()
	for range ping {
		obj := new([64]byte)
		runtime.SetFinalizer(obj, func(*[64]byte) { dstForeignFinRan.Add(1) })
		obj2 := new([64]byte)
		runtime.AddCleanup(obj2, func(struct{}) { dstForeignCleanupRan.Add(1) }, struct{}{})
		ack <- struct{}{}
	}
}

// DSTForeignCallbackDeferred: finalizers/cleanups registered MID-RUN by
// goroutines outside the simulation bubble are discovered by the simulation's
// GCs but must be deferred past the run (run on the ordinary async workers
// afterward), never executed on the bubble drain — while the simulation's own
// mid-run registrations still run on the drain. Prints "done".
func DSTForeignCallbackDeferred() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	ping := make(chan struct{})
	ack := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go dstForeignRegisterPump(ping, ack, &wg)
	fail := ""
	simulation.Run(n, func() {
		for i := 0; i < 30; i++ {
			ping <- struct{}{} // unbubbled rendezvous: non-durable, serviced in real time
			<-ack
			// Allocation burst: cross the deterministic GC trigger so mid-run
			// GCs sweep the pump's dead objects and queue their callbacks.
			for j := 0; j < 16; j++ {
				dstMemSink = make([]byte, 256<<10)
			}
			time.Sleep(time.Millisecond) // quiescence: the drain runs what may run
		}
		// The simulation's own registrations run on the drain in-run (control:
		// proves deferral is ownership-based, not a blanket mid-run deferral).
		simObj := new([64]byte)
		runtime.SetFinalizer(simObj, func(*[64]byte) { dstSimFinRan.Store(true) })
		simObj2 := new([64]byte)
		runtime.AddCleanup(simObj2, func(struct{}) { dstSimCleanupRan.Store(true) }, struct{}{})
		simObj, simObj2 = nil, nil
		runtime.GC()
		time.Sleep(time.Millisecond)
		if !dstSimFinRan.Load() || !dstSimCleanupRan.Load() {
			fail = "simulation-owned callbacks did not run on the drain"
		}
		if dstForeignFinRan.Load() != 0 || dstForeignCleanupRan.Load() != 0 {
			fail = "foreign callback ran during the run"
		}
	})
	close(ping)
	wg.Wait()
	if fail != "" {
		os.Stdout.WriteString(fail + "\n")
		return
	}
	// Every deferred foreign callback was released at deactivation and ran on
	// the ordinary async workers — exact counts, not just "some ran".
	for i := 0; i < 400 && !(dstForeignFinRan.Load() == 30 && dstForeignCleanupRan.Load() == 30); i++ {
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
	}
	if fins, cleanups := dstForeignFinRan.Load(), dstForeignCleanupRan.Load(); fins != 30 || cleanups != 30 {
		os.Stdout.WriteString("deferred foreign callbacks incomplete after the run: fins=" +
			strconv.FormatInt(fins, 10) + "/30 cleanups=" + strconv.FormatInt(cleanups, 10) + "/30\n")
		return
	}
	os.Stdout.WriteString("done\n")
}

// dstOverflowFingerprint runs a workload with more simultaneously-runnable
// goroutines than the local run-queue ring holds (256), so the DST
// order-preserving overflow path is exercised, and returns the interleaving
// fingerprint plus the overflow-put count observed inside the run.
func dstOverflowFingerprint(seed uint64) (string, uint64) {
	var fp []byte
	var ovf uint64
	simulation.Run(seed, func() {
		var mu sync.Mutex
		order := make([]int, 0, 420)
		var wg sync.WaitGroup
		// Gosched'd goroutines keep the global runq populated during the
		// burst, so an overflow rerouted to the global tail (instead of the
		// ring-extension queue) lands AFTER them and is caught as a reorder.
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for r := 0; r < 4; r++ {
					runtime.Gosched()
				}
				mu.Lock()
				order = append(order, 1000+id)
				mu.Unlock()
			}(i)
		}
		// Sleepers: durably blocked during the burst, woken one by one by the
		// first burst workers to run. Each wake is a put that lands while the
		// overflow queue is still non-empty — the boundary the order contract
		// must hold at: a woken goroutine must queue BEHIND the overflowed
		// stragglers (ring extension), not refill a freed ring slot ahead of
		// them, or its position becomes a function of foreign ring occupancy.
		const sleepers = 64
		chs := make([]chan struct{}, sleepers)
		for i := 0; i < sleepers; i++ {
			ch := make(chan struct{})
			chs[i] = ch
			wg.Add(1)
			go func(id int, ch chan struct{}) {
				defer wg.Done()
				<-ch
				mu.Lock()
				order = append(order, 2000+id)
				mu.Unlock()
			}(i, ch)
		}
		// The burst: spawned without yielding, so the ring fills and overflows.
		for i := 0; i < 320; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				if id < sleepers {
					chs[id] <- struct{}{}
				}
				mu.Lock()
				order = append(order, id)
				mu.Unlock()
			}(i)
		}
		wg.Wait()
		ovf = dstSchedOvfPutsFP()
		for _, id := range order {
			fp = append(fp, byte(id), byte(id>>8))
		}
	})
	return string(fp), ovf
}

// DSTRunqOverflowOrder: when the runnable set exceeds the local ring, the
// overflow must be order-preserving — foreign (non-bubble) goroutines occupying
// ring slots must not shift WHICH simulation goroutines spill nor where they
// re-enter the enumeration, so the schedule fingerprint with foreign churn must
// equal the alone run's. Prints "done".
func DSTRunqOverflowOrder() {
	n, _ := strconv.ParseUint(os.Getenv("DSTSEED"), 10, 64)
	stop := make(chan struct{})
	var cwg sync.WaitGroup
	// Foreign churners: woken by short real timers, they enter the same local
	// ring as the simulation's goroutines at unpredictable wall-clock points,
	// shifting its occupancy while the burst overflows.
	for c := 0; c < 4; c++ {
		cwg.Add(1)
		go func() {
			defer cwg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					time.Sleep(50 * time.Microsecond)
				}
			}
		}()
	}
	with1, ovf1 := dstOverflowFingerprint(n)
	with2, ovf2 := dstOverflowFingerprint(n)
	close(stop)
	cwg.Wait()
	alone, ovfAlone := dstOverflowFingerprint(n)
	if ovf1 == 0 || ovf2 == 0 || ovfAlone == 0 {
		os.Stdout.WriteString("vacuous: overflow path never fired\n")
		return
	}
	if with1 != alone || with2 != alone {
		os.Stdout.WriteString("schedule perturbed by overflow under foreign churn\n")
		return
	}
	os.Stdout.WriteString("done\n")
}

//go:linkname dstDeactivate runtime.dstDeactivate
func dstDeactivate()

// DSTOvfFlushAtDeactivate: goroutines still sitting in the DST ring-overflow
// queue when DST deactivates must be handed back to the normal scheduler
// (which never looks at the overflow queue) — otherwise they are stranded
// forever. White-box: activates DST at GOMAXPROCS=1, fills the ring past
// capacity WITHOUT yielding (so the overflow is provably non-empty), then
// deactivates immediately and waits for every spawned goroutine to run.
// Prints "done"; hangs (parent timeout) if the flush is missing.
func DSTOvfFlushAtDeactivate() {
	runtime.GOMAXPROCS(1)
	var wg sync.WaitGroup
	dstActivate(1)
	for i := 0; i < 320; i++ {
		wg.Add(1)
		go func() { wg.Done() }()
	}
	ovf := dstSchedOvfPutsFP()
	dstDeactivate()
	if ovf == 0 {
		os.Stdout.WriteString("vacuous: overflow path never fired\n")
		return
	}
	wg.Wait()
	os.Stdout.WriteString("done\n")
}

// DSTWhiteBoxCleanupChurnP4: the cleanup deferral on the white-box dstActivate
// path at GOMAXPROCS>1 — where every cleanup carries stamp 0 (no bubble) and
// so every one defers, and where a background GC latched just before
// activation sweeps lazily across the boundary, so concurrent sweepers can
// reach the (finlock-serialized) deferral simultaneously. The churn runs
// BEFORE, ACROSS, and AFTER the activation boundary to give that straddling
// cycle real allocators to race with. Pins the behavioral contract: nothing
// runs while active, and the EXACT count survives deferral and release (a
// lost partial-pointer update loses a block of cleanups forever). Prints
// "done".
func DSTWhiteBoxCleanupChurnP4() {
	runtime.GOMAXPROCS(4)
	var ran atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var sink []byte // per-goroutine: the churn must not race on a shared global
			for i := 0; i < 400; i++ {
				obj := new([4096]byte)
				runtime.AddCleanup(obj, func(struct{}) { ran.Add(1) }, struct{}{})
				sink = make([]byte, 64<<10)
			}
			_ = sink
		}()
	}
	close(start) // churn is already running when activation begins
	dstActivate(7)
	wg.Wait()
	runtime.GC()
	dstDeactivate()
	for i := 0; i < 400 && ran.Load() != 1600; i++ {
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
	}
	if got := ran.Load(); got != 1600 {
		os.Stdout.WriteString("cleanups lost: " + strconv.FormatInt(got, 10) + "/1600\n")
		return
	}
	os.Stdout.WriteString("done\n")
}

// DSTGOMAXPROCSEntryRace: a foreign GOMAXPROCS call racing simulation.Run
// entry must never produce a silently nondeterministic run. The dstActive
// gate in the setters is check-then-act, so a call that passed it can land
// its stop-the-world inside the pin->activate window; the runtime re-checks
// dstActive under every setter's STW (dropping the update mid-run) and Run
// verifies the pin held after activation (failing loud). Outcome per run:
// success with GOMAXPROCS==1 observed throughout, or the loud entry panic —
// never an in-run observation of GOMAXPROCS != 1. Prints "done".
func DSTGOMAXPROCSEntryRace() {
	stop := make(chan struct{})
	var cwg sync.WaitGroup
	cwg.Add(1)
	go func() {
		defer cwg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				// Set-only (never back to 1): a toggling churner can undo its
				// own mid-window resize before activation samples it,
				// self-healing the very race the test must catch. Repeated
				// sets at the same value skip the STW (n == ret fast path),
				// so each pin opens exactly one effective resize attempt.
				runtime.GOMAXPROCS(2)
				// Real sleep: while a run is active the setter returns at
				// the cheap dstActive gate without blocking, and an
				// always-runnable foreign goroutine would starve the
				// simulation under system-first scheduling (it never yields
				// the only P).
				time.Sleep(20 * time.Microsecond)
			}
		}
	}()
	violations, panics := 0, 0
	var garbage [][]byte
	for i := 0; i < 300; i++ {
		// Pre-run garbage widens the pin->activate window: the activation's
		// preparation GCs then do real sweep work, giving the churner's
		// stop-the-world a real interval to land fully inside (the case only
		// the post-activation pin verification can catch).
		garbage = nil
		for j := 0; j < 32; j++ {
			garbage = append(garbage, make([]byte, 128<<10))
		}
		garbage = nil // drop blocks and backing array: dead at Run entry, swept by the prep GCs
		func() {
			defer func() {
				if r := recover(); r != nil {
					if s, ok := r.(string); ok && strings.Contains(s, "GOMAXPROCS changed during simulation entry") {
						panics++
						return
					}
					panic(r)
				}
			}()
			simulation.Run(uint64(i+1), func() {
				done := make(chan struct{})
				go func() { close(done) }()
				<-done
				// Sample across real wall time, not just fake-clock points: a
				// foreign setter whose gate passed before activation can have
				// its stop-the-world land mid-SUT (after the entry
				// verification), and only a sample taken after that landing
				// observes the resize.
				var sink []byte
				for k := 0; k < 3000; k++ {
					if runtime.GOMAXPROCS(0) != 1 {
						violations++
						break
					}
					sink = make([]byte, 1024)
				}
				_ = sink
				time.Sleep(time.Millisecond)
				if runtime.GOMAXPROCS(0) != 1 {
					violations++
				}
			})
		}()
	}
	close(stop)
	cwg.Wait()
	if violations != 0 {
		os.Stdout.WriteString("in-run GOMAXPROCS violations: " + strconv.Itoa(violations) +
			" (entry panics: " + strconv.Itoa(panics) + ")\n")
		return
	}
	os.Stdout.WriteString("done\n")
}

// DSTGOMAXPROCSDelayedSTW: the deterministic form of the entry race — a
// setter whose not-active gate passed BEFORE activation but whose
// stop-the-world lands only AFTER (in production, a contended
// computeMaxProcsLock — sysmon reads cgroup files under it — creates exactly
// this delay). The runtime's post-STW dstActive re-check must drop the
// update: GOMAXPROCS stays 1 while the simulation is active, for both
// GOMAXPROCS and SetDefaultGOMAXPROCS. White-box (no bubble needed — the
// re-check keys on dstActive alone). Prints "done".
func DSTGOMAXPROCSDelayedSTW() {
	runtime.GOMAXPROCS(1)
	check := func(name string, call func()) bool {
		reached := make(chan struct{})
		gate := make(chan struct{})
		dstSetGOMAXPROCSSTWHook(func() {
			close(reached)
			<-gate
		})
		done := make(chan struct{})
		go func() {
			defer close(done)
			call()
		}()
		<-reached // the setter passed its not-active gate and is held pre-STW
		dstActivate(7)
		dstSetGOMAXPROCSSTWHook(nil) // the held call is already past the hook read
		close(gate)                  // its STW proceeds; the re-check must drop the update
		<-done
		ok := true
		if got := runtime.GOMAXPROCS(0); got != 1 {
			os.Stdout.WriteString(name + ": delayed setter resized mid-simulation: GOMAXPROCS=" + strconv.Itoa(got) + "\n")
			ok = false
		}
		dstDeactivate()
		runtime.GOMAXPROCS(1)
		return ok
	}
	if !check("GOMAXPROCS", func() { runtime.GOMAXPROCS(2) }) {
		return
	}
	if !check("SetDefaultGOMAXPROCS", func() { runtime.SetDefaultGOMAXPROCS() }) {
		return
	}
	os.Stdout.WriteString("done\n")
}
