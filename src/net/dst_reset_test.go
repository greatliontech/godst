// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"errors"
	"io"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

// These tests exercise connection-reset targeting (simulation.Reset host-pair,
// ResetProcess). Invariants:
//   - a reset delivers ECONNRESET to BOTH ends of every targeted conn;
//   - each end is a SURVIVOR (no process died) and receives the RST as a real
//     kernel would: bytes already DELIVERED to its receive queue drain first
//     (tcp_recvmsg reports pending data before the socket error), then reads
//     fail ECONNRESET and writes fail immediately — DST-FAULT-SOUND;
//   - bytes still IN FLIGHT are dropped (the RST beat them to the socket, one
//     of the real orderings an injected RST produces) — DST-FAULT-SOUND;
//   - it touches exactly the victim's conns, no leak onto other pairs/processes
//     (DST-FAULT-VICTIM);
//   - it replays exactly for a given seed (DST-FAULT-REPLAY).

// TestDSTNetResetPair: Reset(A,B) tears down the A-B conn — both the server's
// blocked read and the client's next read see ECONNRESET.
func TestDSTNetResetPair(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var serverErr, clientErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				_, serverErr = c.Read(make([]byte, 16)) // blocked until the reset
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			time.Sleep(10 * time.Millisecond) // let the server park in Read
			simulation.Reset("A", "B")
			_, clientErr = c.Read(make([]byte, 16))
			<-done
			c.Close()
		})
	})
	if !errors.Is(serverErr, syscall.ECONNRESET) {
		t.Errorf("server read after reset = %v, want ECONNRESET", serverErr)
	}
	if !errors.Is(clientErr, syscall.ECONNRESET) {
		t.Errorf("client read after reset = %v, want ECONNRESET", clientErr)
	}
}

// TestDSTNetResetDropsInFlight: bytes in flight (buffered behind a link latency)
// when the conn is reset are dropped — the peer sees ECONNRESET, not the buffered
// data (a real RST discards in-flight; DST-FAULT-SOUND).
func TestDSTNetResetDropsInFlight(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var readErr error
	var gotData bool
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				n, err := c.Read(make([]byte, 16))
				gotData = n > 0
				readErr = err
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			c.Write([]byte("msg"))     // in flight: deliverable only 100ms later
			simulation.Reset("A", "B") // reset now, before delivery
			<-done
			c.Close()
		})
	})
	if gotData {
		t.Errorf("peer received in-flight bytes across a reset, want them dropped")
	}
	if !errors.Is(readErr, syscall.ECONNRESET) {
		t.Errorf("read after reset = %v, want ECONNRESET", readErr)
	}
}

// TestDSTNetResetDropsInFlightEvenAfterDelay: in-flight bytes die AT the
// reset instant, not merely before the survivor's next wake — virtual time
// passing beyond their would-be delivery after the reset must never
// resurrect them (the injected RST destroyed the connection state that would
// have accepted them; a late read still finds an empty queue and the reset
// error).
func TestDSTNetResetDropsInFlightEvenAfterDelay(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var n int
	var readErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		resetDone := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-resetDone // read only after the reset AND the would-be delivery time
				n, readErr = c.Read(make([]byte, 16))
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			c.Write([]byte("msg"))             // in flight: deliverable only 100ms later
			simulation.Reset("A", "B")         // reset now, before delivery
			time.Sleep(200 * time.Millisecond) // let virtual time pass the would-be deliverAt
			close(resetDone)
			<-done
			c.Close()
		})
	})
	if n != 0 {
		t.Errorf("read %d bytes after the reset's would-be delivery time, want 0: dead in-flight bytes must not resurrect", n)
	}
	if !errors.Is(readErr, syscall.ECONNRESET) {
		t.Errorf("late read after reset = %v, want ECONNRESET", readErr)
	}
}

// TestDSTNetResetUnderPartitionDropsHeldBytes: an injected reset during a
// partition kills exactly what a real kernel would lose — bytes delivered
// to the survivor's receive queue BEFORE the cut drain (they arrived; no
// RST can destroy them), while bytes the cut is holding die even though
// their nominal delivery time has passed (they never reached the receive
// queue; the connection state that would have accepted them is gone).
func TestDSTNetResetUnderPartitionDropsHeldBytes(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var n int
	var buf [16]byte
	var firstErr, secondErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 10 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		reset := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-reset
				n, firstErr = c.Read(buf[:])
				_, secondErr = c.Read(make([]byte, 16))
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			c.Write([]byte("pre"))
			time.Sleep(20 * time.Millisecond) // "pre" is delivered to A's receive queue
			c.Write([]byte("held"))
			simulation.Partition("A", "B") // cut before "held" arrives
			time.Sleep(50 * time.Millisecond)
			// "held"'s nominal delivery time has long passed, but the cut
			// kept it out of A's receive queue. The reset must not deliver
			// it — not even after the link heals (the heal would flush held
			// bytes on a LIVE conn; this one died under the cut).
			simulation.Reset("A", "B")
			simulation.Heal("A", "B")
			close(reset)
			<-done
			c.Close()
		})
	})
	if n != 3 || string(buf[:3]) != "pre" || firstErr != nil {
		t.Errorf("first read after reset-under-cut = (%d, %q, %v), want (3, %q, nil): pre-cut-delivered bytes drain", n, buf[:n], firstErr, "pre")
	}
	if !errors.Is(secondErr, syscall.ECONNRESET) {
		t.Errorf("second read = %v, want ECONNRESET with no data: cut-held bytes died with the reset", secondErr)
	}
}

// TestDSTNetResetDrainsDeliveredThenResets: bytes already DELIVERED to a
// survivor's receive queue before an injected Reset drain first — a real RST
// cannot destroy what the receiver's kernel already holds (tcp_recvmsg
// reports pending data before the socket error) — and only then do reads
// fail ECONNRESET; a write after the reset fails ECONNRESET immediately.
func TestDSTNetResetDrainsDeliveredThenResets(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var n int
	var buf [16]byte
	var firstErr, secondErr, writeErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 10 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		reset := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-reset
				n, firstErr = c.Read(buf[:])
				_, secondErr = c.Read(make([]byte, 16))
				_, writeErr = c.Write([]byte("after"))
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			c.Write([]byte("msg"))
			time.Sleep(20 * time.Millisecond) // past the link latency: delivered to A's receive queue
			simulation.Reset("A", "B")
			close(reset)
			<-done
			c.Close()
		})
	})
	if n != 3 || string(buf[:3]) != "msg" || firstErr != nil {
		t.Errorf("first read after reset = (%d, %q, %v), want (3, %q, nil): delivered bytes drain before the reset error", n, buf[:n], firstErr, "msg")
	}
	if !errors.Is(secondErr, syscall.ECONNRESET) {
		t.Errorf("second read after drain = %v, want ECONNRESET", secondErr)
	}
	if !errors.Is(writeErr, syscall.ECONNRESET) {
		t.Errorf("write after reset = %v, want ECONNRESET (the pending socket error fails sends immediately)", writeErr)
	}
}

// TestDSTNetResetProcessOwnEndDrains: ResetProcess(p) resets p's connections,
// not p itself — p stays alive, so p's OWN end is a survivor too and drains
// the bytes already delivered to it before failing ECONNRESET.
func TestDSTNetResetProcessOwnEndDrains(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var n int
	var buf [16]byte
	var firstErr, secondErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 10 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				c.Write([]byte("hi"))
				<-done
				c.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			simulation.Process("worker", func() {
				p := <-port
				c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
				time.Sleep(20 * time.Millisecond) // the server's "hi" is delivered to the worker's queue
				simulation.ResetProcess("worker")
				n, firstErr = c.Read(buf[:])
				_, secondErr = c.Read(make([]byte, 16))
				close(done)
				c.Close()
			})
		})
	})
	if n != 2 || string(buf[:2]) != "hi" || firstErr != nil {
		t.Errorf("worker's first read after ResetProcess = (%d, %q, %v), want (2, %q, nil): the live process drains its delivered bytes", n, buf[:n], firstErr, "hi")
	}
	if !errors.Is(secondErr, syscall.ECONNRESET) {
		t.Errorf("worker's second read = %v, want ECONNRESET", secondErr)
	}
}

// TestDSTNetResetBacklogBlockedDialFailsPromptly: an injected reset reaches
// a connection still in the accept backlog as the hard handshake abort a
// real kernel performs — a dial blocked on a FULL backlog (its SYN dropped,
// retransmitting) fails ECONNRESET at the reset, never stranding to the
// retransmit horizon's ETIMEDOUT: no receive queue exists on either side of
// a half-open connection, so there is nothing to drain and the blocked
// connect must be woken by the teardown itself.
func TestDSTNetResetBacklogBlockedDialFailsPromptly(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var dialErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				<-done // never Accept; the backlog fills and holds
				ln.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			addr := simulation.HostIP("A") + ":" + p
			conns := make([]Conn, 0, 128)
			for i := 0; i < 128; i++ { // fill the accept backlog
				c, err := Dial("tcp", addr)
				if err != nil {
					t.Errorf("backlog-filling dial %d: %v", i, err)
					close(done)
					return
				}
				conns = append(conns, c)
			}
			blocked := make(chan struct{})
			go func() {
				_, dialErr = Dial("tcp", addr) // SYN dropped: blocks on the full backlog
				close(blocked)
			}()
			time.Sleep(10 * time.Millisecond) // let the dial park
			simulation.Reset("A", "B")
			<-blocked
			for _, c := range conns {
				c.Close()
			}
			close(done)
		})
	})
	if !errors.Is(dialErr, syscall.ECONNRESET) {
		t.Errorf("dial blocked on a full backlog across a reset = %v, want ECONNRESET (prompt handshake abort, not a horizon ETIMEDOUT)", dialErr)
	}
}

// TestDSTNetResetFailsBlockedWrite: a write parked on a full send buffer
// when the reset lands fails ECONNRESET with only the pre-reset bytes
// written — a real kernel wakes a blocked sender with the socket error, and
// the destroyed connection can never carry the remainder. The adversarial
// interleaving is forced: the peer DRAINS (freeing send-buffer space, so the
// parked writer COMMITS to its freed-space wakeup) and injects the reset in
// the same scheduler slot, before the writer resumes — the resumed writer
// must observe the RST instead of pushing the remaining bytes, whatever
// wakeup it committed to.
func TestDSTNetResetFailsBlockedWrite(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var writeN int
	var writeErr error
	var n1 int
	var buf [8]byte
	var lateN int
	var lateErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{
		CrossHostLatency: 10 * time.Millisecond,
		SendBuffer:       4, // the 8-byte write below blocks after 4 bytes
	}}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		wrote := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				time.Sleep(30 * time.Millisecond) // first 4 bytes delivered; writer parked on the full buffer
				n1, _ = c.Read(buf[:4])           // drain: frees space, the parked writer commits to its wakeup
				simulation.Reset("A", "B")        // …and the RST lands before the writer resumes (no park between)
				<-wrote
				lateN, lateErr = c.Read(buf[4:]) // anything the woken writer pushed post-RST would land here
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			go func() {
				writeN, writeErr = c.Write([]byte("12345678")) // blocks at 4 bytes
				close(wrote)
			}()
			<-done
			c.Close()
		})
	})
	if n1 != 4 || string(buf[:4]) != "1234" {
		t.Fatalf("pre-reset drain = (%d, %q), want (4, %q)", n1, buf[:n1], "1234")
	}
	if writeN != 4 || !errors.Is(writeErr, syscall.ECONNRESET) {
		t.Errorf("blocked write across a reset = (%d, %v), want (4, ECONNRESET): the woken writer must observe the RST, never push the remainder", writeN, writeErr)
	}
	if lateN != 0 || !errors.Is(lateErr, syscall.ECONNRESET) {
		t.Errorf("post-reset read = (%d, %v), want (0, ECONNRESET): no post-RST byte may be delivered", lateN, lateErr)
	}
}

// TestDSTNetResetKeepsIdentityPastRetransmitHorizon: an injected RST
// destroys the socket AND its retransmit timer — a partition-armed
// retransmit watchdog expiring after the RST must not flip the conn's error
// identity from ECONNRESET to ETIMEDOUT on later operations. The reachable
// shape is a HOST-CRASH survivor: its outbound bytes written into the cut
// stay held (only its inbound direction is truncated by the RST), so the
// watchdog still sees them at its horizon; a pair reset truncates both
// directions and the watchdog self-disarms.
func TestDSTNetResetKeepsIdentityPastRetransmitHorizon(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var firstErr, lateReadErr, lateWriteErr error
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{
		CrossHostLatency:  10 * time.Millisecond,
		RetransmitTimeout: time.Second,
	}}, func() {
		port := make(chan string, 1)
		accepted := make(chan struct{})
		simulation.Host("victim", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				ln.Accept()
				close(accepted)
				select {} // dies with the machine
			}()
		})
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("victim")+":"+p)
			<-accepted
			simulation.Partition("survivor", "victim")
			c.Write([]byte("held")) // undeliverable: arms the survivor's retransmit watchdog
			simulation.CrashHost("victim")
			_, firstErr = c.Read(make([]byte, 8))
			time.Sleep(3 * time.Second) // well past the watchdog's horizon
			_, lateReadErr = c.Read(make([]byte, 8))
			_, lateWriteErr = c.Write([]byte("x"))
			c.Close()
		})
	})
	if !errors.Is(firstErr, syscall.ECONNRESET) {
		t.Errorf("read after the crash's RST = %v, want ECONNRESET", firstErr)
	}
	if !errors.Is(lateReadErr, syscall.ECONNRESET) {
		t.Errorf("read past the retransmit horizon = %v, want ECONNRESET (the RST killed the timer with the socket)", lateReadErr)
	}
	if !errors.Is(lateWriteErr, syscall.ECONNRESET) {
		t.Errorf("write past the retransmit horizon = %v, want ECONNRESET", lateWriteErr)
	}
}

// TestDSTNetInjectRSTFreezesReceiveQueue: the injected RST freezes the
// receive queue at the RST instant, in PROGRAM order. The teardown loop
// injects a connection's two ends sequentially, so the not-yet-injected
// peer end can still push a byte after this end's RST already landed (its
// own rstArrived is not set yet); that byte must never become readable here
// — a real CLOSED socket answers a late segment with an RST of its own, it
// does not queue it. Conversely a byte pushed BEFORE the receiving end's
// injection is in (or bound for) its receive queue: at zero latency it
// carries deliverAt EQUAL to the freeze horizon and must stay drainable —
// program order, not timestamps, decides both directions, since virtual
// time does not advance across the interleaving. White-box at the wire
// layer so the interleaving is exact rather than schedule-dependent.
func TestDSTNetInjectRSTFreezesReceiveQueue(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	for _, latency := range []time.Duration{10 * time.Millisecond, 0} {
		var lateN, preN int
		var lateErr, preErr error
		simulation.Run(1, func() {
			a, b := dstWirePair(int64(latency), 0, 0, 0, 0, 1, 2)
			ea, eb := a.(*dstWireEnd), b.(*dstWireEnd)
			if _, err := ea.Write([]byte("pre")); err != nil { // toward b, before ANY injection
				t.Errorf("latency %v: pre-RST write = %v, want success", latency, err)
			}
			ea.injectRST()                                      // the RST lands at end a…
			if _, err := eb.Write([]byte("late")); err != nil { // …and its peer, not yet injected, pushes afterward
				t.Errorf("latency %v: peer write before its own injection = %v, want success", latency, err)
			}
			eb.injectRST()
			if latency > 0 {
				time.Sleep(2 * latency) // past the late segment's nominal delivery time
			}
			lateN, lateErr = ea.Read(make([]byte, 8))
			preN, preErr = eb.Read(make([]byte, 8))
		})
		if lateN != 0 || lateErr != io.ErrClosedPipe {
			t.Fatalf("latency %v: read on the reset end = (%d, %v), want (0, io.ErrClosedPipe): the receive queue froze at the RST; a post-RST segment is never delivered", latency, lateN, lateErr)
		}
		if latency == 0 {
			// Same-instant pre-RST byte: deliverAt == the freeze horizon —
			// it reached the receive queue and must drain before the error.
			if preN != 3 || preErr != nil {
				t.Fatalf("latency 0: pre-RST byte at the freeze boundary = (%d, %v), want (3, nil): deliverAt equal to the horizon is DELIVERED", preN, preErr)
			}
		} else {
			// At 10ms the pre-RST byte was still in flight at the freeze:
			// it died with the connection.
			if preN != 0 || preErr != io.ErrClosedPipe {
				t.Fatalf("latency %v: in-flight pre-RST byte = (%d, %v), want (0, io.ErrClosedPipe)", latency, preN, preErr)
			}
		}
	}
}

// TestDSTNetResetVictim: Reset(A,B) tears down only the A-B conn — a concurrent
// A-C conn keeps working (DST-FAULT-VICTIM, no leak onto a non-victim pair).
func TestDSTNetResetVictim(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var acMsg string
	var acErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		portB := make(chan string, 1)
		portC := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("B", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			portB <- p
			go func() {
				c, _ := ln.Accept()
				c.Read(make([]byte, 16)) // will be reset
				c.Close()
			}()
		})
		simulation.Host("C", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			portC <- p
			go func() {
				c, _ := ln.Accept()
				buf := make([]byte, 16)
				n, err := c.Read(buf) // must survive the A-B reset
				acMsg, acErr = string(buf[:n]), err
				c.Close()
				close(done)
			}()
		})
		simulation.Host("A", simulation.HostConfig{}, func() {
			pb, pc := <-portB, <-portC
			cb, _ := Dial("tcp", simulation.HostIP("B")+":"+pb)
			cc, _ := Dial("tcp", simulation.HostIP("C")+":"+pc)
			time.Sleep(10 * time.Millisecond)
			simulation.Reset("A", "B") // only A-B
			cc.Write([]byte("toC"))    // A-C must still deliver
			<-done
			cb.Close()
			cc.Close()
		})
	})
	if acErr != nil {
		t.Errorf("A-C read errored after an A-B reset: %v (leak onto a non-victim)", acErr)
	}
	if acMsg != "toC" {
		t.Errorf("A-C read = %q, want %q (A-C must be unaffected by the A-B reset)", acMsg, "toC")
	}
}

// TestDSTNetResetProcess: ResetProcess(p) tears down every conn process p owns an
// end of (DST-FAULT-VICTIM, the process leg — the per-process conn attribution),
// resetting BOTH ends so the peer's in-flight bytes are dropped too. The worker
// (dialer) owns the conn; the server end is the worker's peer, so it must reset as
// well — the worker's in-flight "msg" must not reach the server.
func TestDSTNetResetProcess(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var serverErr error
	var gotData bool
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				n, err := c.Read(make([]byte, 16))
				gotData = n > 0
				serverErr = err
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			simulation.Process("worker", func() {
				p := <-port
				c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
				c.Write([]byte("msg"))            // in flight (100ms latency)
				simulation.ResetProcess("worker") // resets BOTH ends -> server drops in-flight
				<-done
				c.Close()
			})
		})
	})
	if gotData {
		t.Errorf("server received the worker's in-flight bytes after ResetProcess, want them dropped (both ends must reset)")
	}
	if !errors.Is(serverErr, syscall.ECONNRESET) {
		t.Errorf("server read after ResetProcess = %v, want ECONNRESET", serverErr)
	}
}

func TestDSTNetResetProcessLeavesListenerOpen(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var dialErr, acceptErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		simulation.Process("worker", func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			simulation.ResetProcess("worker")
			accepted := make(chan struct{})
			go func() {
				c, err := ln.Accept()
				acceptErr = err
				if err == nil {
					c.Close()
				}
				close(accepted)
			}()
			c, err := Dial("tcp", "127.0.0.1:"+p)
			dialErr = err
			if err == nil {
				c.Close()
			}
			<-accepted
			ln.Close()
		})
	})
	if dialErr != nil {
		t.Fatalf("Dial after ResetProcess on listener owner = %v, want nil", dialErr)
	}
	if acceptErr != nil {
		t.Fatalf("Accept after ResetProcess on listener owner = %v, want nil", acceptErr)
	}
}

// TestDSTNetResetDeterminism: reset runs replay exactly for a given seed.
func TestDSTNetResetDeterminism(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	run := func(seed uint64) bool {
		var srvReset bool
		simulation.RunWith(seed, simulation.Options{}, func() {
			port := make(chan string, 1)
			done := make(chan struct{})
			simulation.Host("A", simulation.HostConfig{}, func() {
				ln, _ := Listen("tcp", ":0")
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				go func() {
					c, _ := ln.Accept()
					_, err := c.Read(make([]byte, 16))
					srvReset = errors.Is(err, syscall.ECONNRESET)
					c.Close()
					close(done)
				}()
			})
			simulation.Host("B", simulation.HostConfig{}, func() {
				p := <-port
				c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
				time.Sleep(10 * time.Millisecond)
				simulation.Reset("A", "B")
				<-done
				c.Close()
			})
		})
		return srvReset
	}
	for seed := uint64(0); seed < 8; seed++ {
		if r1, r2 := run(seed), run(seed); r1 != r2 || !r1 {
			t.Fatalf("seed %d: reset not reproducible/effective: %v vs %v", seed, r1, r2)
		}
	}
}
