// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

// TestDSTNetBackpressureUnblocks is the M7 regression: with a bounded send buffer, a
// write far larger than the buffer does not buffer unbounded — it BLOCKS until the
// reader drains, then completes, the data flowing end-to-end gated by the reader. A
// pop frees send-buffer space (wakeWriter), so the writer resumes; a chunking bug in
// the write loop would drop or duplicate bytes and the checksum would not match.
func TestDSTNetBackpressureUnblocks(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const total = 100 << 10 // 100 KiB, far exceeding the 4 KiB send buffer
	// content byte i = i%251 so a dropped/duplicated/reordered byte changes the sum.
	want := make([]byte, total)
	var wantSum uint64
	for i := range want {
		want[i] = byte(i % 251)
		wantSum += uint64(want[i])
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{SendBuffer: 4 << 10}}
	var gotN int
	var gotSum uint64
	var writeN int
	var writeErr error
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				buf := make([]byte, 8<<10)
				for {
					n, err := c.Read(buf)
					for i := 0; i < n; i++ {
						gotSum += uint64(buf[i])
					}
					gotN += n
					if err != nil {
						break
					}
				}
				c.Close()
				close(done)
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			writeN, writeErr = c.Write(want)
			c.Close() // half-close: reader drains, then sees EOF
			<-done
		})
	})
	if writeErr != nil || writeN != total {
		t.Errorf("Write = %d, %v; want %d, nil (backpressure must still deliver every byte)", writeN, writeErr, total)
	}
	if gotN != total || gotSum != wantSum {
		t.Errorf("server received %d bytes sum %d; want %d bytes sum %d (backpressure lost/corrupted data)", gotN, gotSum, total, wantSum)
	}
}

// TestDSTNetWriteHorizonTimesOut is the horizon regression: a write whose bytes are
// held at a PARTITION fills the bounded send buffer and blocks; once the cut outlasts
// the retransmit horizon the write fails ETIMEDOUT in bounded virtual time (a kernel's
// exhausted retransmissions) rather than blocking forever or buffering unbounded. The
// horizon is partition-gated (see TestDSTNetWritePersistsWithoutPartition): a live peer
// that merely stopped reading is zero-window persist, not an ETIMEDOUT.
func TestDSTNetWriteHorizonTimesOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const total = 100 << 10
	opts := simulation.Options{Network: simulation.NetworkConfig{
		SendBuffer:        4 << 10,
		RetransmitTimeout: time.Second,
	}}
	var writeN int
	var writeErr error
	var writeDur time.Duration
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		writeDone := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-writeDone // the peer stays alive (open) but is cut off by the partition
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			simulation.Partition("srv", "cli") // cut the established conn: the write's bytes are undeliverable
			t0 := time.Now()
			writeN, writeErr = c.Write(make([]byte, total)) // fills the buffer, blocks, horizon fires
			writeDur = time.Since(t0)
			close(writeDone)
			c.Close()
		})
	})
	if !errors.Is(writeErr, syscall.ETIMEDOUT) {
		t.Errorf("write across a permanent partition = %d, %v; want ETIMEDOUT after the retransmit horizon", writeN, writeErr)
	}
	if writeN <= 0 || writeN >= total {
		t.Errorf("write accepted %d of %d bytes; want a partial count (only the send buffer's worth)", writeN, total)
	}
	if writeDur != time.Second {
		t.Errorf("partitioned write took %v to fail, want the 1s retransmit horizon", writeDur)
	}
}

// TestDSTNetWritePersistsWithoutPartition is the M1 regression: a full send buffer
// behind a LIVE peer that has merely stopped reading is TCP zero-window persist, NOT
// retransmit exhaustion — the write must NOT fire ETIMEDOUT (a real live peer never
// produces that). With a write deadline shorter than nothing but longer than the
// horizon, a correctly partition-gated write blocks on the deadline (os.ErrDeadlineExceeded);
// an un-gated horizon would wrongly fire ETIMEDOUT at 1s first.
func TestDSTNetWritePersistsWithoutPartition(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const total = 100 << 10
	opts := simulation.Options{Network: simulation.NetworkConfig{
		SendBuffer:        4 << 10,
		RetransmitTimeout: time.Second, // shorter than the write deadline below
	}}
	var writeErr error
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		writeDone := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-writeDone // alive and open, just never reads: zero-window persist, no partition
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			c.SetWriteDeadline(time.Now().Add(2 * time.Second)) // > the 1s horizon
			_, writeErr = c.Write(make([]byte, total))
			close(writeDone)
			c.Close()
		})
	})
	if errors.Is(writeErr, syscall.ETIMEDOUT) {
		t.Errorf("write behind a live non-reading peer = %v; must NOT be ETIMEDOUT (zero-window persist, not retransmit exhaustion — the horizon is partition-gated)", writeErr)
	}
	if !errors.Is(writeErr, os.ErrDeadlineExceeded) {
		t.Errorf("write behind a live non-reading peer = %v; want os.ErrDeadlineExceeded (it persists until the write deadline, never the horizon)", writeErr)
	}
}

// TestDSTNetConcurrentWritersChainWake is the H1 regression: two writers blocked on a
// full send buffer must BOTH resume when a single drain frees enough space for both.
// The reader pings the cap-1 space channel once per pop, so a woken writer that leaves
// room must chain-wake the next; without that, one writer strands with buffer space
// free (a lost wakeup) and its Write never returns — the bubble deadlocks. net.Conn is
// documented concurrency-safe, so concurrent writers are a legal SUT shape.
func TestDSTNetConcurrentWritersChainWake(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const cap = 8 << 10
	const each = 4 << 10 // two of these (8 KiB) exactly refill the freed buffer
	opts := simulation.Options{Network: simulation.NetworkConfig{SendBuffer: cap}}
	var n1, n2 int
	var e1, e2 error
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		drain := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-drain                           // wait until the buffer is full and both writers are blocked
				io.ReadFull(c, make([]byte, cap)) // one drain of the whole buffer, freeing both writers at once
				<-done
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			c.Write(make([]byte, cap)) // fill the buffer exactly (returns; buffered == cap)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); n1, e1 = c.Write(make([]byte, each)) }() // both block: buffer full
			go func() { defer wg.Done(); n2, e2 = c.Write(make([]byte, each)) }()
			time.Sleep(time.Millisecond) // let both writers reach the durably-blocked state
			close(drain)                 // one 8 KiB drain frees space for BOTH
			wg.Wait()                    // a stranded writer never returns -> deadlock here
			close(done)
			c.Close()
		})
	})
	if n1 != each || e1 != nil || n2 != each || e2 != nil {
		t.Errorf("concurrent writers after one drain = (%d,%v),(%d,%v); want both (%d,nil) — a strand means no chain-wake", n1, e1, n2, e2, each)
	}
}

// TestDSTNetConnectPaysRTT is the M6 regression: a cross-host connect completes one
// full round trip after it began (SYN out + SYN-ACK back, each a one-way traversal of
// the link's base latency), not instantly. A zero-RTT connect would let a SUT's connect
// timeout pass under simulation on a link where it fails in production (unsound). The
// dialer is on an unstepped host, so its own clock reads base time.
func TestDSTNetConnectPaysRTT(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const L = 50 * time.Millisecond
	var dialDur time.Duration
	var dialErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: L}}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				<-done
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			t0 := time.Now()
			c, err := Dial("tcp", simulation.HostIP("srv")+":"+p)
			dialDur = time.Since(t0)
			dialErr = err
			close(done)
			if c != nil {
				c.Close()
			}
		})
	})
	if dialErr != nil {
		t.Fatalf("cross-host Dial: %v", dialErr)
	}
	if dialDur != 2*L {
		t.Errorf("cross-host connect took %v, want %v (one RTT: SYN + SYN-ACK, each %v)", dialDur, 2*L, L)
	}
}

// TestDSTNetDialPartitionHorizonTimesOut is the connect-timeout regression: a
// deadline-less dial across a permanent partition drops its SYNs (blackhole) and fails
// ETIMEDOUT once the retransmit horizon elapses in virtual time — a real kernel's
// exhausted SYN retries — rather than hanging forever (a sim-only deadlock).
func TestDSTNetDialPartitionHorizonTimesOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: time.Second}}
	var dialErr error
	var dialDur time.Duration
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			// No Accept: the dial is blackholed by the partition, never reaching the
			// backlog, so no goroutine lingers blocked when the bubble drains.
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			simulation.Partition("srv", "cli") // cut BEFORE dialing; never healed
			t0 := time.Now()
			_, dialErr = Dial("tcp", simulation.HostIP("srv")+":"+p) // no deadline
			dialDur = time.Since(t0)
		})
	})
	if !errors.Is(dialErr, syscall.ETIMEDOUT) {
		t.Errorf("deadline-less dial across a permanent partition = %v, want ETIMEDOUT at the horizon", dialErr)
	}
	if dialDur != time.Second {
		t.Errorf("dial blackhole took %v to fail, want the 1s retransmit horizon", dialDur)
	}
}
