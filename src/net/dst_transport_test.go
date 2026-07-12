// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"internal/synctest"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/simulation"
	"time"
)

// These tests exercise the always-wire transport contract (docs/dst/design.md
// "In-memory deterministic network", Transport model): every conn is wire-backed
// (no rendezvous net.Pipe), reads are a coalescing byte stream, no wakeup is lost
// with concurrent readers, and a partition holds only bytes not yet delivered at
// the cut (already-delivered bytes stay readable).

// dialLoopback dials a same-host listener and returns the accepted server end and
// the dialer end — the co-located (same-host) path that used to be net.Pipe.
func dialLoopback(t *testing.T) (server, client Conn) {
	t.Helper()
	ln, err := Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	_, port, _ := SplitHostPort(ln.Addr().String())
	accepted := make(chan Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			t.Errorf("Accept: %v", err)
		}
		accepted <- c
	}()
	client, err = Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return <-accepted, client
}

func TestDSTConcurrentReadersAllObserveEOF(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	for _, withData := range []bool{false, true} {
		name := "empty"
		if withData {
			name = "final-buffer"
		}
		t.Run(name, func(t *testing.T) {
			for seed := uint64(1); seed <= 20; seed++ {
				simulation.Run(seed, func() {
					server, client := dialLoopback(t)
					defer server.Close()
					server.SetReadDeadline(time.Now().Add(time.Second))
					type result struct {
						n   int
						err error
					}
					results := make(chan result, 2)
					started := make(chan struct{}, 2)
					for i := 0; i < 2; i++ {
						go func() {
							started <- struct{}{}
							b := make([]byte, 1)
							n, err := server.Read(b)
							results <- result{n, err}
						}()
					}
					<-started
					<-started
					synctest.Wait() // both readers are parked in Read before FIN/data arrives
					if withData {
						if _, err := client.Write([]byte("x")); err != nil {
							t.Fatal(err)
						}
					}
					if err := client.Close(); err != nil {
						t.Fatal(err)
					}
					a, b := <-results, <-results
					if withData {
						if !((a.n == 1 && a.err == nil && b.n == 0 && b.err == io.EOF) || (b.n == 1 && b.err == nil && a.n == 0 && a.err == io.EOF)) {
							t.Fatalf("reader results = (%d,%v), (%d,%v); want data and EOF", a.n, a.err, b.n, b.err)
						}
					} else if a.n != 0 || a.err != io.EOF || b.n != 0 || b.err != io.EOF {
						t.Fatalf("reader results = (%d,%v), (%d,%v); want two EOFs", a.n, a.err, b.n, b.err)
					}
				})
			}
		})
	}
}

// TestDSTNetSameHostMutualWrite is the H3 regression: two co-located peers each
// write BEFORE either reads. A rendezvous net.Pipe would deadlock both writes (a
// write blocks until the peer reads); a real TCP send buffer accepts both instantly.
// The wire-backed same-host conn must complete — a deadlock here is a sim-only false
// positive.
func TestDSTNetSameHostMutualWrite(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var got1, got2 string
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			server, client := dialLoopback(t)
			// Both ends write before reading — the mutual-push deadlock shape.
			if _, err := client.Write([]byte("ping")); err != nil {
				t.Errorf("client write: %v", err)
			}
			if _, err := server.Write([]byte("pong")); err != nil {
				t.Errorf("server write: %v", err)
			}
			buf := make([]byte, 4)
			if _, err := io.ReadFull(server, buf); err != nil {
				t.Errorf("server read: %v", err)
			}
			got1 = string(buf)
			if _, err := io.ReadFull(client, buf); err != nil {
				t.Errorf("client read: %v", err)
			}
			got2 = string(buf)
			client.Close()
			server.Close()
		})
	})
	if got1 != "ping" {
		t.Errorf("server read %q, want ping", got1)
	}
	if got2 != "pong" {
		t.Errorf("client read %q, want pong", got2)
	}
}

// TestDSTNetReadsCoalesce is the M9 regression: reads are a byte stream, not framed.
// Two separate Writes must be readable in a single Read spanning both — TCP gives no
// write-boundary framing, and preserving it would let a SUT that assumes
// one-read-per-write pass under simulation and break in production.
func TestDSTNetReadsCoalesce(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var n int
	var got string
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			server, client := dialLoopback(t)
			client.Write([]byte("aaa"))
			client.Write([]byte("bbb"))
			// Give both writes time to become deliverable, then a single large Read
			// must return all 6 bytes at once (coalesced across the two writes).
			time.Sleep(time.Millisecond)
			buf := make([]byte, 32)
			n, _ = server.Read(buf)
			got = string(buf[:n])
			client.Close()
			server.Close()
		})
	})
	if n != 6 || got != "aaabbb" {
		t.Errorf("coalesced Read = %q (n=%d), want aaabbb (6) — write boundaries must not frame reads", got, n)
	}
}

// TestDSTNetConcurrentReadersNoLostWakeup is the M8 regression: two goroutines each
// blocked in Read on one conn end must both be served when data arrives — the cap-1
// wakeup channel cannot buffer a second ping, so a delivery leaving further
// deliverable bytes must re-signal. Without the re-wake a reader strands while its
// data sits delivered, a harness-manufactured hang (net.Conn is concurrency-safe).
func TestDSTNetConcurrentReadersNoLostWakeup(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	got := make([]string, 2)
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			server, client := dialLoopback(t)
			var wg sync.WaitGroup
			// Two readers, each wants 4 bytes.
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					buf := make([]byte, 4)
					if _, err := io.ReadFull(server, buf); err != nil {
						t.Errorf("reader %d: %v", i, err)
						return
					}
					got[i] = string(buf)
				}(i)
			}
			// Let both readers block, then deliver 8 bytes in ONE write — one push, one
			// wake ping. The reader that consumes the ping fills its 4-byte buffer from
			// the 8-byte segment and leaves the rest deliverable; it MUST re-signal or
			// the second reader strands on the cap-1 ready channel (a lost wakeup). A
			// single write makes this independent of interleaving: only one ping ever
			// exists, so the re-wake is the only path that serves the second reader.
			time.Sleep(time.Millisecond)
			client.Write([]byte("00001111"))
			wg.Wait()
			client.Close()
			server.Close()
		})
	})
	// Order between the two readers is scheduler-determined; both must be served.
	total := got[0] + got[1]
	if len(total) != 8 || !(strings.Contains(total, "0000") && strings.Contains(total, "1111")) {
		t.Errorf("concurrent readers got %q + %q, want both 0000 and 1111 delivered", got[0], got[1])
	}
}

// TestDSTNetConcurrentReadersInFlightTail is the end-to-end M8-residual check: two
// concurrent readers on one conn, fed two bandwidth-serialized segments (distinct
// delivery times), must both be served — a second reader must not strand on an
// in-flight tail. (The exact strand is pinned scheduler-free by the white-box
// TestDSTWirePopRemainInFlightTail; this is the integration cross-check.)
func TestDSTNetConcurrentReadersInFlightTail(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	got := make([]string, 2)
	// 800 bytes/sec => a 4-byte segment occupies the link 5ms; two back-to-back 4-byte
	// writes deliver at +5ms and +10ms from one ping.
	opts := simulation.Options{Network: simulation.NetworkConfig{CrossHostBandwidth: 800}}
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() { // server: two readers
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() { // server work off the Host body so Host("A") returns and B runs
				c, _ := ln.Accept()
				var wg sync.WaitGroup
				for i := 0; i < 2; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						buf := make([]byte, 4)
						if _, err := io.ReadFull(c, buf); err != nil {
							t.Errorf("reader %d: %v", i, err)
							return
						}
						got[i] = string(buf)
					}(i)
				}
				wg.Wait()
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() { // client: two back-to-back writes
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			time.Sleep(time.Millisecond) // let both readers block first
			c.Write([]byte("0000"))      // no sleep between: one ping, bandwidth separates delivery
			c.Write([]byte("1111"))
			<-done
			c.Close()
		})
	})
	total := got[0] + got[1]
	if len(total) != 8 || !(strings.Contains(total, "0000") && strings.Contains(total, "1111")) {
		t.Errorf("concurrent readers with an in-flight tail got %q + %q, want both 0000 and 1111 (second reader stranded on an in-flight segment?)", got[0], got[1])
	}
}

// TestDSTNetPartitionPreDeliveredReadable is the M5 regression: bytes DELIVERED
// before a partition sit in the receiver's buffer and stay readable during the cut;
// only in-flight and after-cut bytes are held. Blackholing already-delivered bytes
// would be a sim-only read failure real kernels never produce.
func TestDSTNetPartitionPreDeliveredReadable(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var duringCut string
	var readErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		read := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() { // server
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-read // wait until the byte is delivered AND the link is cut
				buf := make([]byte, 8)
				c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				var n int
				n, readErr = c.Read(buf) // must return the pre-cut byte, not time out
				duringCut = string(buf[:n])
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() { // client
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			c.Write([]byte("early"))
			time.Sleep(10 * time.Millisecond) // "early" is delivered into A's buffer
			simulation.Partition("A", "B")    // cut AFTER delivery
			close(read)                       // now let the server read during the cut
			<-done
			c.Close()
		})
	})
	if readErr != nil {
		t.Errorf("read of pre-cut data during partition = %v, want the buffered bytes (not a timeout)", readErr)
	}
	if duringCut != "early" {
		t.Errorf("read during partition = %q, want %q (bytes delivered before the cut must stay readable)", duringCut, "early")
	}
}
