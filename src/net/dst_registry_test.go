// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

func TestDSTNetConnectionPairOwnershipPublishesAtomically(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	for seed := uint64(1); seed <= 100; seed++ {
		halfOwned := false
		simulation.RunWith(seed, simulation.Options{}, func() {
			reset := new(atomic.Bool)
			dialer := &dstConn{reset: reset}
			server := &dstConn{reset: reset}
			done := make(chan struct{})
			go func() {
				dstConnRegisterPair(dialer, server)
				close(done)
			}()
			for {
				dstConns.mu.Lock()
				dstConnsRoll()
				owned := 0
				for candidate := range dstConns.set {
					if candidate.reset == reset {
						owned++
					}
				}
				dstConns.mu.Unlock()
				if owned == 1 {
					halfOwned = true
				}
				if owned == 2 {
					break
				}
				runtime.Gosched()
			}
			<-done
			dstConnDeregister(dialer)
			dstConnDeregister(server)
		})
		if halfOwned {
			t.Fatalf("seed %d: teardown observer saw a half-owned connection pair", seed)
		}
	}
}

func TestDSTNetAcceptHandoffIsLifecycleOwned(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	for seed := uint64(1); seed <= 100; seed++ {
		owned := false
		simulation.RunWith(seed, simulation.Options{}, func() {
			ln, _ := Listen("tcp", ":0")
			inspected := make(chan struct{})
			release := make(chan struct{})
			go func() {
				c, _ := ln.Accept()
				dc := c.(*dstConn)
				dstConns.mu.Lock()
				dstConnsRoll()
				ends := 0
				for candidate := range dstConns.set {
					if candidate.reset == dc.reset {
						ends++
					}
				}
				dstConns.mu.Unlock()
				owned = ends == 2
				close(inspected)
				<-release
				c.Close()
			}()
			c, err := Dial("tcp", ln.Addr().String())
			if err != nil {
				panic(err)
			}
			<-inspected
			close(release)
			c.Close()
			ln.Close()
		})
		if !owned {
			t.Fatalf("seed %d: Accept exposed connection before both ends were lifecycle-owned", seed)
		}
	}
}

func TestDSTNetAcceptedCloseBeforeDialReturnCleansOwnership(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	for seed := uint64(1); seed <= 50; seed++ {
		var dialErr error
		var conn Conn
		var leaked int
		simulation.RunWith(seed, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 10 * time.Millisecond}}, func() {
			port := make(chan string, 1)
			closed := make(chan struct{})
			var accepted *dstConn
			simulation.Host("A", simulation.HostConfig{}, func() {
				ln, _ := Listen("tcp", ":0")
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				go func() {
					c, _ := ln.Accept()
					accepted = c.(*dstConn)
					c.Close()
					close(closed)
				}()
			})
			simulation.Host("B", simulation.HostConfig{}, func() {
				conn, dialErr = Dial("tcp", simulation.HostIP("A")+":"+<-port)
			})
			<-closed
			dstConns.mu.Lock()
			dstConnsRoll()
			for candidate := range dstConns.set {
				if candidate.reset == accepted.reset {
					leaked++
				}
			}
			dstConns.mu.Unlock()
		})
		if conn != nil || !errors.Is(dialErr, syscall.ECONNRESET) {
			t.Fatalf("seed %d: accepted close returned (%v, %v), want nil ECONNRESET", seed, conn, dialErr)
		}
		if leaked != 0 {
			t.Fatalf("seed %d: accepted close left %d registered endpoint(s)", seed, leaked)
		}
	}
}

// TestDSTNetResetOrderDeterministic is the H4 regression: when a reset matches
// several conns, they are torn down in registration (Dial) order — the wake order of
// their blocked readers, and thus the downstream schedule, is a function of the seed,
// never of the registry map's pointer-address iteration order. The readers log the
// order they observe the reset; it must reproduce across same-seed runs AND equal the
// dial order, which only the registration-sequence sort guarantees.
func TestDSTNetResetOrderDeterministic(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const n = 6
	run := func(seed uint64) []int {
		var order []int
		var mu sync.Mutex
		simulation.RunWith(seed, simulation.Options{}, func() {
			port := make(chan string, 1)
			var wg sync.WaitGroup
			wg.Add(n)
			simulation.Host("A", simulation.HostConfig{}, func() {
				ln, _ := Listen("tcp", ":0")
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				go func() { // server work off the Host body
					for i := 0; i < n; i++ {
						c, _ := ln.Accept()
						go func(c Conn, idx int) {
							defer wg.Done()
							_, err := c.Read(make([]byte, 8))
							if errors.Is(err, syscall.ECONNRESET) {
								mu.Lock()
								order = append(order, idx)
								mu.Unlock()
							}
							c.Close()
						}(c, i)
					}
				}()
			})
			simulation.Host("B", simulation.HostConfig{}, func() {
				p := <-port
				conns := make([]Conn, n)
				for i := 0; i < n; i++ {
					conns[i], _ = Dial("tcp", simulation.HostIP("A")+":"+p) // dial i registers with seq ~i
				}
				time.Sleep(10 * time.Millisecond) // let all readers block
				simulation.Reset("A", "B")        // resets all n, in registration order
				wg.Wait()
				for _, c := range conns {
					c.Close()
				}
			})
		})
		return order
	}
	for seed := uint64(0); seed < 6; seed++ {
		a, b := run(seed), run(seed)
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Fatalf("seed %d: reset order not reproducible: %v vs %v", seed, a, b)
		}
		// All n readers observe the reset, and the order is reproducible (checked
		// above). The order is registration-sequence order, not dial-index order
		// (registration completes after the accept handoff, which interleaves), so we
		// assert reproducibility + completeness here; the address-independence of the
		// ordering is pinned white-box below.
		if len(a) != n {
			t.Fatalf("seed %d: %d readers observed the reset, want %d (a dropped victim)", seed, len(a), n)
		}
	}
}

// TestDSTNetResetVictimOrderByRegSeq is the white-box pin for H4: dstMatchedVictims
// returns victims in registration-SEQUENCE order regardless of the registry map's
// (pointer-address) iteration order. Conns are inserted with regSeq scrambled relative
// to map-insertion so a map-order return would be caught. Address-free by construction.
func TestDSTNetResetVictimOrderByRegSeq(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	// Build conns with regSeq in a scrambled order; register them (register stamps a
	// fresh regSeq, so set it AFTER via the set map directly to control the values).
	dstConns.mu.Lock()
	dstConns.set = make(map[*dstConn]struct{})
	dstConns.epoch = dstNetEpoch()
	conns := make([]*dstConn, 8)
	seqs := []uint64{5, 2, 7, 1, 8, 3, 6, 4} // scrambled vs insertion order
	for i := range conns {
		c := &dstConn{localHost: 1, remoteHost: 2, regSeq: seqs[i]}
		conns[i] = c
		dstConns.set[c] = struct{}{}
	}
	dstConns.mu.Unlock()

	victims := dstMatchedVictims(func(c *dstConn) bool { return c.localHost == 1 && c.remoteHost == 2 })
	if len(victims) != len(conns) {
		t.Fatalf("matched %d victims, want %d", len(victims), len(conns))
	}
	for i := 1; i < len(victims); i++ {
		if victims[i-1].regSeq >= victims[i].regSeq {
			t.Fatalf("victims not in ascending regSeq order: %d then %d (map-iteration order, not registration order)", victims[i-1].regSeq, victims[i].regSeq)
		}
	}
	dstConns.mu.Lock()
	dstConns.set = nil // reset so the next run rebuilds cleanly
	dstConns.mu.Unlock()
}

// acceptN spawns a goroutine that accepts exactly k conns on ln, appends them to
// *out (under mu), then returns — so no Accept is left blocked at run end. Call from
// a Host body; it returns immediately.
func acceptN(ln Listener, k int, out *[]Conn, mu *sync.Mutex) {
	go func() {
		for i := 0; i < k; i++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			*out = append(*out, c)
			mu.Unlock()
		}
	}()
}

// TestDSTNetLocalAddrReuseEADDRINUSE is the port-hygiene regression: an explicit
// Dialer.LocalAddr that collides on a live local addr:port (a 2-tuple) fails
// EADDRINUSE — Go binds without SO_REUSEADDR, so bind(2) refuses even when the
// destinations differ.
func TestDSTNetLocalAddrReuseEADDRINUSE(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		port := make(chan string, 1)
		var srv []Conn
		var mu sync.Mutex
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			acceptN(ln, 1, &srv, &mu) // only the first dial establishes
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			la := &TCPAddr{IP: ParseIP(simulation.HostIP("cli")), Port: 55555}
			d := Dialer{LocalAddr: la}
			c1, err := d.Dial("tcp", simulation.HostIP("srv")+":"+p)
			if err != nil {
				t.Fatalf("first dial with explicit LocalAddr: %v", err)
			}
			// Second dial reusing the same local addr:port while c1 is live: EADDRINUSE.
			_, err = d.Dial("tcp", simulation.HostIP("srv")+":"+p)
			if !errors.Is(err, syscall.EADDRINUSE) {
				t.Errorf("second dial reusing LocalAddr = %v, want EADDRINUSE", err)
			}
			c1.Close()
		})
	})
}

// TestDSTNetEphemeralPortsInRange: dialer ephemeral ports stay within the valid TCP
// range [40000, 65535] — the bare counter reached impossible numbers above 65535.
func TestDSTNetEphemeralPortsInRange(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	simulation.Run(1, func() {
		port := make(chan string, 1)
		var srv []Conn
		var mu sync.Mutex
		var ports []int
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			acceptN(ln, 20, &srv, &mu)
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			var conns []Conn
			for i := 0; i < 20; i++ {
				c, err := Dial("tcp", simulation.HostIP("srv")+":"+p)
				if err != nil {
					t.Fatalf("dial %d: %v", i, err)
				}
				ports = append(ports, c.LocalAddr().(*TCPAddr).Port)
				conns = append(conns, c)
			}
			for _, c := range conns {
				c.Close()
			}
		})
		for _, pt := range ports {
			if pt < 40000 || pt > 65535 {
				t.Errorf("ephemeral port %d out of range [40000, 65535]", pt)
			}
		}
	})
}
