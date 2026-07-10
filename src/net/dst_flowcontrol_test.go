// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
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

// TestDSTNetSmallWriteHorizonKillsConn: a write that FITS in the send buffer
// against a partitioned link succeeds immediately (TCP's async send — the
// bytes buffer), but it never succeeds-and-forgets: the bytes are
// undeliverable, a real sender's retransmissions exhaust, and the conn is
// dead at the horizon — the next operation fails ETIMEDOUT. Before the
// watchdog, ten small writes over ten virtual minutes into a permanent cut
// produced zero errors.
func TestDSTNetSmallWriteHorizonKillsConn(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: time.Second}}
	var firstN int
	var firstErr, secondErr error
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-done
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			simulation.Partition("srv", "cli")
			firstN, firstErr = c.Write([]byte("hi")) // fits: buffers and returns
			time.Sleep(2 * time.Second)              // the 1s horizon passes with the cut permanent
			_, secondErr = c.Write([]byte("again"))
			close(done)
			c.Close()
		})
	})
	if firstN != 2 || firstErr != nil {
		t.Errorf("small write into a fresh cut = (%d, %v), want (2, nil): TCP's send buffers async", firstN, firstErr)
	}
	if !errors.Is(secondErr, syscall.ETIMEDOUT) {
		t.Errorf("write after the horizon killed the conn = %v, want ETIMEDOUT: undeliverable bytes never succeed-and-forget", secondErr)
	}
}

// TestDSTNetWriteThenReadHorizonTimesOut: the death surfaces on a BLOCKED
// operation too — a small write into a permanent cut, then a deadline-less
// read, fails ETIMEDOUT at the horizon instead of hanging forever.
func TestDSTNetWriteThenReadHorizonTimesOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: time.Second}}
	var readErr error
	var readDur time.Duration
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-done
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			simulation.Partition("srv", "cli")
			c.Write([]byte("hi")) // buffers; undeliverable
			t0 := time.Now()
			_, readErr = c.Read(make([]byte, 8)) // blocks; the outbound horizon kills the end
			readDur = time.Since(t0)
			close(done)
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ETIMEDOUT) {
		t.Errorf("blocked read on a conn with dying outbound bytes = %v, want ETIMEDOUT", readErr)
	}
	if readDur != time.Second {
		t.Errorf("blocked read failed after %v, want the 1s retransmit horizon", readDur)
	}
}

// TestDSTNetHorizonHealDisarms: a heal that delivers the held bytes before the
// horizon disarms the watchdog — the conn lives, the peer receives the bytes,
// and no spurious ETIMEDOUT fires after the original deadline passes.
func TestDSTNetHorizonHealDisarms(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: time.Second}}
	var got string
	var lateN int
	var lateErr error
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		received := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				buf := make([]byte, 8)
				n, _ := c.Read(buf)
				got = string(buf[:n])
				close(received)
				<-done
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			simulation.Partition("srv", "cli")
			c.Write([]byte("hi")) // held at the cut; watchdog armed
			time.Sleep(500 * time.Millisecond)
			simulation.Heal("srv", "cli") // before the 1s horizon: bytes flush
			<-received
			time.Sleep(2 * time.Second) // well past the original deadline
			lateN, lateErr = c.Write([]byte("ok"))
			close(done)
			c.Close()
		})
	})
	if got != "hi" {
		t.Errorf("peer received %q after the heal, want %q (held bytes flush)", got, "hi")
	}
	if lateN != 2 || lateErr != nil {
		t.Errorf("write after a disarming heal = (%d, %v), want (2, nil): no spurious horizon death", lateN, lateErr)
	}
}

// TestDSTNetHorizonDeathDrainsDeliveredData: a horizon-killed end still
// returns data the network already delivered before failing — tcp_recvmsg
// reports pending data first, then the socket error.
func TestDSTNetHorizonDeathDrainsDeliveredData(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: time.Second}}
	var firstN int
	var firstErr, secondErr error
	var buf [8]byte
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		sent := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				c.Write([]byte("pre")) // delivered before any cut
				close(sent)
				<-done
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			<-sent
			time.Sleep(10 * time.Millisecond) // let "pre" arrive (instant link, but order the schedule)
			simulation.Partition("srv", "cli")
			c.Write([]byte("hi"))       // undeliverable: arms the watchdog
			time.Sleep(2 * time.Second) // horizon kills the end
			firstN, firstErr = c.Read(buf[:])
			_, secondErr = c.Read(make([]byte, 8))
			close(done)
			c.Close()
		})
	})
	if firstN != 3 || string(buf[:3]) != "pre" || firstErr != nil {
		t.Errorf("first read on the killed end = (%d, %q, %v), want (3, %q, nil): delivered data drains before the error", firstN, buf[:firstN], firstErr, "pre")
	}
	if !errors.Is(secondErr, syscall.ETIMEDOUT) {
		t.Errorf("second read on the killed end = %v, want ETIMEDOUT", secondErr)
	}
}

// TestDSTNetInFlightBytesCutThenReadTimesOut: bytes already IN FLIGHT when the
// cut begins (written on a live 100ms link, partitioned before delivery) are
// undeliverable too — a blocked read observing them arms the horizon and fails
// ETIMEDOUT; before the read-side arm, this hung forever (the write predated
// the cut, so nothing armed the watchdog).
func TestDSTNetInFlightBytesCutThenReadTimesOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHostLatency:  100 * time.Millisecond,
		RetransmitTimeout: time.Second,
	}}
	var readErr error
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-done
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			c.Write([]byte("hi"))              // live link: in flight for 100ms
			simulation.Partition("srv", "cli") // cut before delivery: the bytes are held
			_, readErr = c.Read(make([]byte, 8))
			close(done)
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ETIMEDOUT) {
		t.Errorf("blocked read with in-flight bytes caught by the cut = %v, want ETIMEDOUT at the horizon", readErr)
	}
}

// TestDSTNetHorizonHealRecutAnchorsAtObservation: a heal-then-recut while the
// stale watchdog is still pending starts a NEW undeliverable episode — the
// window re-anchors at the check's own observation, never at the new cut's
// start (which can predate the episode's bytes by arbitrarily long and would
// kill them before their own horizon: a premature, sim-only ETIMEDOUT). Here
// the second episode's bytes are healed well inside their true window, so the
// conn must live.
func TestDSTNetHorizonHealRecutAnchorsAtObservation(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: time.Second}}
	var lateN int
	var lateErr error
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				buf := make([]byte, 16)
				for {
					if _, err := c.Read(buf); err != nil {
						break
					}
				}
				<-done
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			simulation.Partition("srv", "cli") // cut 1 at t=0
			c.Write([]byte("w1"))              // arms; anchor t=0
			time.Sleep(100 * time.Millisecond)
			simulation.Heal("srv", "cli") // t=0.1: w1 flushes; watchdog still pending until t=1.0
			time.Sleep(100 * time.Millisecond)
			simulation.Partition("srv", "cli") // cut 2 at t=0.2
			time.Sleep(700 * time.Millisecond)
			c.Write([]byte("w2")) // t=0.9: armHorizon no-ops (still armed from episode 1)
			// Stale check fires at t=1.0: new episode → re-anchor at NOW (1.0),
			// never at cut 2's start (0.2), which would kill at t=1.2.
			time.Sleep(400 * time.Millisecond)
			simulation.Heal("srv", "cli") // t=1.3: w2 (undeliverable for only 0.4s) flushes
			time.Sleep(100 * time.Millisecond)
			lateN, lateErr = c.Write([]byte("ok")) // t=1.4: the conn must be alive
			close(done)
			c.Close()
		})
	})
	if lateN != 2 || lateErr != nil {
		t.Errorf("write after a heal-recut episode healed inside its own window = (%d, %v), want (2, nil): the re-anchor must be the observation time, not the cut start", lateN, lateErr)
	}
}

// TestDSTNetOneWayCutInFlightReadTimesOut: a ONE-WAY outbound cut catches
// in-flight bytes while the inbound direction stays live — a blocked read
// (which is not itself cut) must still fail at the horizon: the sender's ACKs
// never return through the cut, so its retransmissions exhaust. Before the
// hoisted arm, this read hung forever.
func TestDSTNetOneWayCutInFlightReadTimesOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHostLatency:  100 * time.Millisecond,
		RetransmitTimeout: time.Second,
	}}
	var readErr error
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-done
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			c.Write([]byte("hi"))                    // live link: in flight for 100ms
			simulation.PartitionOneWay("cli", "srv") // outbound-only cut catches them; inbound stays live
			_, readErr = c.Read(make([]byte, 8))     // not cut itself — must still die at the horizon
			close(done)
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ETIMEDOUT) {
		t.Errorf("blocked read under a one-way outbound cut with dying in-flight bytes = %v, want ETIMEDOUT", readErr)
	}
}

// TestDSTNetCutAfterReadBlockedTimesOut: the third geometry — the read parks
// on a LIVE link first, then the cut lands and catches the in-flight bytes.
// The partition change must wake the parked reader so it re-evaluates and
// arms the outbound watchdog; before the wake case, the reader stranded past
// the horizon (a permanent hang the spec forbids).
func TestDSTNetCutAfterReadBlockedTimesOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHostLatency:  100 * time.Millisecond,
		RetransmitTimeout: time.Second,
	}}
	var readErr error
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		readDone := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-done
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			go func() {
				c.Write([]byte("hi")) // live link: in flight for 100ms
				_, readErr = c.Read(make([]byte, 8))
				close(readDone)
			}()
			time.Sleep(10 * time.Millisecond)  // the read is parked on the live link
			simulation.Partition("srv", "cli") // the cut catches the in-flight bytes
			<-readDone
			close(done)
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ETIMEDOUT) {
		t.Errorf("read parked before the cut landed = %v, want ETIMEDOUT at the horizon (the partition change must wake it to arm)", readErr)
	}
}

// TestDSTNetSameHostBackpressure: same-host connections carry the same
// bounded send buffer as cross-host ones — loopback TCP has finite socket
// buffers too. A co-located writer far exceeding the buffer blocks until the
// reader drains, and the bytes flow end-to-end intact.
func TestDSTNetSameHostBackpressure(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const total = 100 << 10
	opts := simulation.Options{Network: simulation.NetworkConfig{SendBuffer: 4 << 10}}
	var gotN int
	var gotSum, wantSum uint64
	simulation.RunWith(1, opts, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			ln, err := Listen("tcp", "127.0.0.1:0")
			if err != nil {
				panic(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				c, err := ln.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 8<<10)
				for {
					n, err := c.Read(buf)
					for i := 0; i < n; i++ {
						gotSum += uint64(buf[i])
					}
					gotN += n
					if err != nil {
						return
					}
				}
			}()
			c, err := Dial("tcp", ln.Addr().String()) // same host: loopback
			if err != nil {
				panic(err)
			}
			data := make([]byte, total)
			for i := range data {
				data[i] = byte(i % 251)
				wantSum += uint64(data[i])
			}
			if n, err := c.Write(data); n != total || err != nil {
				panic(err)
			}
			c.Close()
			<-done
			ln.Close()
		})
	})
	if gotN != total || gotSum != wantSum {
		t.Errorf("same-host bounded transfer = %d bytes (sum %d), want %d (sum %d)", gotN, gotSum, total, wantSum)
	}
}

// TestDSTNetSameHostWriteWriteDeadlocks: the fidelity the bound buys — two
// co-located peers that each write past the send buffer BEFORE reading
// deadlock in production (both loopback socket buffers fill; neither read
// runs). The simulation reproduces it as a loud bubble deadlock instead of
// completing both writes into an unbounded sim-only buffer that masks the bug.
func TestDSTNetSameHostWriteWriteDeadlocks(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{SendBuffer: 4 << 10}}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("co-located write-write past both send buffers completed; want the production deadlock, reproduced loudly")
		}
		if !strings.Contains(fmt.Sprint(r), "deadlock") {
			panic(r) // not the bubble-deadlock diagnostic: repanic
		}
	}()
	simulation.RunWith(1, opts, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", "127.0.0.1:0")
			defer ln.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Write(make([]byte, 64<<10)) // fills its 4 KiB buffer, blocks
				c.Close()
			}()
			c, err := Dial("tcp", ln.Addr().String())
			if err != nil {
				panic(err)
			}
			c.Write(make([]byte, 64<<10)) // both sides writing, nobody reading
			c.Close()
			<-done
		})
	})
}
