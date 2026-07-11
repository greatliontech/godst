// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"errors"
	"io"
	"strconv"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

// dstBindTestTarget declares host "srv" with a listener at an EXPLICIT port
// and returns its dialable address plus a closer the test calls before the run
// body ends (a dial completes via the listener's backlog; no Accept loop is
// needed). The explicit port keeps the per-run :0 listener-port counter
// untouched, so a test's own :0 allocation genuinely starts at 10000.
func dstBindTestTarget() (addr string, cleanup func()) {
	var ln Listener
	ready := make(chan struct{})
	simulation.Host("srv", simulation.HostConfig{}, func() {
		ln, _ = Listen("tcp", simulation.HostIP("srv")+":20000")
		close(ready)
	})
	<-ready
	return simulation.HostIP("srv") + ":20000", func() { ln.Close() }
}

// TestDSTNetDialLocalBindListenerPortEADDRINUSE: an explicit Dialer.LocalAddr
// binds without SO_REUSEADDR, so a live LISTENER's 2-tuple on the dialer's own
// host fails the bind EADDRINUSE — exact and wildcard listeners alike (a
// wildcard listener covers every local IP at its port).
func TestDSTNetDialLocalBindListenerPortEADDRINUSE(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var exactErr, wildErr error
	simulation.Run(1, func() {
		target, cleanup := dstBindTestTarget()
		defer cleanup()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			me := ParseIP(simulation.HostIP("cli"))

			exact, _ := Listen("tcp", simulation.HostIP("cli")+":33000") // occupies cli:33000 exactly
			d := Dialer{LocalAddr: &TCPAddr{IP: me, Port: 33000}}
			_, exactErr = d.Dial("tcp", target)
			exact.Close()

			wild, _ := Listen("tcp", ":33001") // wildcard: covers cli's IP at 33001
			d = Dialer{LocalAddr: &TCPAddr{IP: me, Port: 33001}}
			_, wildErr = d.Dial("tcp", target)
			wild.Close()
		})
	})
	if !errors.Is(exactErr, syscall.EADDRINUSE) {
		t.Errorf("dial bound to an exact listener 2-tuple = %v, want EADDRINUSE", exactErr)
	}
	if !errors.Is(wildErr, syscall.EADDRINUSE) {
		t.Errorf("dial bound to a wildcard listener's port = %v, want EADDRINUSE", wildErr)
	}
}

// TestDSTNetEphemeralDialSkipsListenerPort: the ephemeral allocator skips a
// port a listener occupies, exactly as it skips a live conn's port — a fresh
// run's counter starts at 40000, so a listener there forces 40001.
func TestDSTNetEphemeralDialSkipsListenerPort(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var gotPort string
	var dialErr error
	simulation.Run(1, func() {
		target, cleanup := dstBindTestTarget()
		defer cleanup()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			squat, _ := Listen("tcp", simulation.HostIP("cli")+":40000") // the allocator's first pick
			defer squat.Close()
			c, err := Dial("tcp", target)
			dialErr = err
			if err == nil {
				_, gotPort, _ = SplitHostPort(c.LocalAddr().String())
				c.Close()
			}
		})
	})
	if dialErr != nil {
		t.Fatalf("ephemeral dial with 40000 squatted: %v", dialErr)
	}
	if gotPort != "40001" {
		t.Errorf("ephemeral local port = %s, want 40001 (the allocator must skip the listener's 40000)", gotPort)
	}
}

// TestDSTNetListenConnPortEADDRINUSE: a live DIALER-end conn (no SO_REUSEADDR)
// blocks a new listener on its local port — specific and wildcard listens
// alike; and the listener-port allocator skips such a port.
func TestDSTNetListenConnPortEADDRINUSE(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var exactErr, wildErr error
	var allocPort string
	simulation.Run(1, func() {
		target, cleanup := dstBindTestTarget()
		defer cleanup()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			me := ParseIP(simulation.HostIP("cli"))
			// Occupy cli:10000 with a dialer-end conn — inside the LISTENER
			// allocator's range, so the allocation probe below must skip it.
			d := Dialer{LocalAddr: &TCPAddr{IP: me, Port: 10000}}
			c, err := d.Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			defer c.Close()
			_, exactErr = Listen("tcp", simulation.HostIP("cli")+":10000")
			_, wildErr = Listen("tcp", ":10000")
			// The per-run :0 counter is untouched (the target listener is
			// explicit), so this allocation starts at the conn's 10000 and the
			// probe must skip to 10001.
			ln, err := Listen("tcp", ":0")
			if err != nil {
				panic(err)
			}
			defer ln.Close()
			_, allocPort, _ = SplitHostPort(ln.Addr().String())
		})
	})
	if !errors.Is(exactErr, syscall.EADDRINUSE) {
		t.Errorf("specific listen on a live conn's local 2-tuple = %v, want EADDRINUSE", exactErr)
	}
	if !errors.Is(wildErr, syscall.EADDRINUSE) {
		t.Errorf("wildcard listen on a live conn's local port = %v, want EADDRINUSE", wildErr)
	}
	if allocPort != "10001" {
		t.Errorf("allocated listener port = %s, want 10001 (the allocator must skip the conn's 10000)", allocPort)
	}
}

// TestDSTNetDialerTimeWaitEADDRINUSE: an ACTIVE FIN-close of a dialer end
// holds its local 2-tuple for 60s of simulated time (Linux's TCP_TIMEWAIT_LEN,
// the kernel's fixed 2·MSL) — an explicit-LocalAddr re-dial inside the hold
// fails EADDRINUSE, production's bind(2)-without-SO_REUSEADDR answer for a
// TIME_WAIT tuple, and succeeds once the hold expires.
func TestDSTNetDialerTimeWaitEADDRINUSE(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var heldErr, expiredErr error
	simulation.Run(1, func() {
		target, cleanup := dstBindTestTarget()
		defer cleanup()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			me := ParseIP(simulation.HostIP("cli"))
			d := Dialer{LocalAddr: &TCPAddr{IP: me, Port: 33000}}
			c, err := d.Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			c.Close() // nothing unread, peer has not closed: the active FIN close
			_, heldErr = d.Dial("tcp", target)
			time.Sleep(61 * time.Second) // simulated: crosses the 60s hold
			c2, err := d.Dial("tcp", target)
			expiredErr = err
			if err == nil {
				c2.Close()
			}
		})
	})
	if !errors.Is(heldErr, syscall.EADDRINUSE) {
		t.Errorf("re-dial of a TIME_WAIT 2-tuple = %v, want EADDRINUSE", heldErr)
	}
	if expiredErr != nil {
		t.Errorf("re-dial after the 2·MSL hold = %v, want success", expiredErr)
	}
}

// TestDSTNetListenerBindsOverTimeWait: a listener binds with SO_REUSEADDR, so
// a TIME_WAIT hold — unlike a LIVE dialer conn — does not block it.
func TestDSTNetListenerBindsOverTimeWait(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var listenErr error
	simulation.Run(1, func() {
		target, cleanup := dstBindTestTarget()
		defer cleanup()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			me := ParseIP(simulation.HostIP("cli"))
			d := Dialer{LocalAddr: &TCPAddr{IP: me, Port: 33000}}
			c, err := d.Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			c.Close() // active close: cli:33000 enters TIME_WAIT
			ln, err := Listen("tcp", simulation.HostIP("cli")+":33000")
			listenErr = err
			if err == nil {
				ln.Close()
			}
		})
	})
	if listenErr != nil {
		t.Errorf("listen on a TIME_WAIT 2-tuple = %v, want success (SO_REUSEADDR binds over TIME_WAIT)", listenErr)
	}
}

// TestDSTNetAcceptedEndTimeWaitEADDRINUSE: an ACTIVE close of an ACCEPTED
// end holds its local 2-tuple (the listener's port) too, exactly as
// production does — visible only to a later non-SO_REUSEADDR bind of that
// port, here an explicit-LocalAddr dial from the server host after the
// listener is gone. Same-host conns exercise the wire path like any other.
func TestDSTNetAcceptedEndTimeWaitEADDRINUSE(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var heldErr error
	simulation.Run(1, func() {
		target, cleanup := dstBindTestTarget()
		defer cleanup()
		done := make(chan struct{})
		simulation.Host("app", simulation.HostConfig{}, func() {
			me := ParseIP(simulation.HostIP("app"))
			self := simulation.HostIP("app")
			ln, err := Listen("tcp", self+":21000")
			if err != nil {
				panic(err)
			}
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close() // ACTIVE close of the accepted end: app:21000 enters TIME_WAIT
				close(done)
			}()
			c, err := Dial("tcp", self+":21000") // ephemeral dialer end, same host
			if err != nil {
				panic(err)
			}
			<-done
			io.ReadAll(c) // drain the FIN
			c.Close()     // passive: the dialer end holds nothing
			ln.Close()    // the port's LISTENER is gone; only the hold remains
			d := Dialer{LocalAddr: &TCPAddr{IP: me, Port: 21000}}
			_, heldErr = d.Dial("tcp", target)
		})
	})
	if !errors.Is(heldErr, syscall.EADDRINUSE) {
		t.Errorf("dial bound to an accepted end's TIME_WAIT tuple = %v, want EADDRINUSE", heldErr)
	}
}

// TestDSTNetPassiveCloseSkipsTimeWait: the PASSIVE closer (the peer closed
// first) goes to CLOSED without TIME_WAIT — after draining the peer's FIN to
// EOF and closing, the same 2-tuple re-dials immediately.
func TestDSTNetPassiveCloseSkipsTimeWait(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var redialErr error
	simulation.Run(1, func() {
		var ln Listener
		ready := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ = Listen("tcp", simulation.HostIP("srv")+":20000")
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close() // the server closes first: its FIN reaches the dialer
			}()
			close(ready)
		})
		<-ready
		defer ln.Close()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			me := ParseIP(simulation.HostIP("cli"))
			target := simulation.HostIP("srv") + ":20000"
			d := Dialer{LocalAddr: &TCPAddr{IP: me, Port: 33000}}
			c, err := d.Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			io.ReadAll(c) // drain to EOF: the peer's close has happened
			c.Close()     // passive close: no TIME_WAIT
			c2, err := d.Dial("tcp", target)
			redialErr = err
			if err == nil {
				c2.Close()
			}
		})
	})
	if redialErr != nil {
		t.Errorf("re-dial after a PASSIVE close = %v, want success (no TIME_WAIT for the passive closer)", redialErr)
	}
}

// TestDSTNetRSTCloseSkipsTimeWait: an end that answers with RST (closed with
// unread received data — the close(2) conditional's RST arm) goes to CLOSED
// without TIME_WAIT; the same 2-tuple re-dials immediately.
func TestDSTNetRSTCloseSkipsTimeWait(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var redialErr error
	simulation.Run(1, func() {
		var ln Listener
		ready := make(chan struct{})
		sent := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ = Listen("tcp", simulation.HostIP("srv")+":20000")
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Write([]byte{1}) // a byte the dialer never reads
				close(sent)
			}()
			close(ready)
		})
		<-ready
		defer ln.Close()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			me := ParseIP(simulation.HostIP("cli"))
			target := simulation.HostIP("srv") + ":20000"
			d := Dialer{LocalAddr: &TCPAddr{IP: me, Port: 33000}}
			c, err := d.Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			<-sent
			c.Close() // unread inbound: the RST arm — no TIME_WAIT
			c2, err := d.Dial("tcp", target)
			redialErr = err
			if err == nil {
				c2.Close()
			}
		})
	})
	if redialErr != nil {
		t.Errorf("re-dial after an RST close = %v, want success (RST skips TIME_WAIT)", redialErr)
	}
}

// TestDSTNetEphemeralChurnOutlivesTimeWait: the ephemeral allocator ignores
// TIME_WAIT holds — production's connect-time selection is 4-tuple-aware with
// tcp_tw_reuse, so a dial/close churn loop never exhausts the port range even
// though every active close holds its tuple. More iterations than the whole
// ephemeral span, in zero simulated time, so a hold-respecting allocator
// would exhaust and fail EADDRNOTAVAIL partway.
func TestDSTNetEphemeralChurnOutlivesTimeWait(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var churnErr error
	simulation.Run(1, func() {
		var ln Listener
		ready := make(chan struct{})
		simulation.Host("sink", simulation.HostConfig{}, func() {
			ln, _ = Listen("tcp", simulation.HostIP("sink")+":20000")
			go func() {
				// Drain the backlog but KEEP the accepted ends open, so the
				// dialer is always the ACTIVE closer and every dialer port
				// genuinely enters TIME_WAIT.
				for {
					if _, err := ln.Accept(); err != nil {
						return
					}
				}
			}()
			close(ready)
		})
		<-ready
		defer ln.Close()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			target := simulation.HostIP("sink") + ":20000"
			const span = dstDialEphemeralEnd - dstDialEphemeralStart + 1
			for i := 0; i < span+100; i++ {
				c, err := Dial("tcp", target)
				if err != nil {
					churnErr = err
					return
				}
				c.Close()
			}
		})
	})
	if churnErr != nil {
		t.Errorf("ephemeral churn across the port span = %v, want success (the allocator must not respect TIME_WAIT holds)", churnErr)
	}
}

// TestDSTNetTimeWaitDeadEndsHoldNothing: the two CLOSED-direct shapes without
// a deterministic dial-level driver — a retransmit-exhaustion death (the
// horizon fired) and an already-reset conn — hold nothing at Close. White-box:
// the states are forced directly and the hold registry is probed directly.
func TestDSTNetTimeWaitDeadEndsHoldNothing(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var timedOutHeld, resetHeld bool
	simulation.Run(1, func() {
		target, cleanup := dstBindTestTarget()
		defer cleanup()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			host, _ := dstNetCurrentNode()
			me := ParseIP(simulation.HostIP("cli"))

			d := Dialer{LocalAddr: &TCPAddr{IP: me, Port: 33000}}
			c, err := d.Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			c.(*dstConn).Conn.(*dstWireEnd).timedOut.Store(true) // the retransmit horizon fired
			c.Close()
			timedOutHeld = dstTimeWaitHeld(host, me, 33000)

			d = Dialer{LocalAddr: &TCPAddr{IP: me, Port: 33001}}
			c2, err := d.Dial("tcp", target)
			if err != nil {
				panic(err)
			}
			c2.(*dstConn).resetConn() // the conn was reset before the user's Close
			c2.Close()
			resetHeld = dstTimeWaitHeld(host, me, 33001)
		})
	})
	if timedOutHeld {
		t.Errorf("a retransmit-exhaustion-dead end held its tuple at Close, want no hold (production goes to CLOSED)")
	}
	if resetHeld {
		t.Errorf("an already-reset end held its tuple at Close, want no hold (RST skips TIME_WAIT)")
	}
}

// TestDSTNetRelistenWithAcceptedConns: accepted server-side ends inherit the
// listener's SO_REUSEADDR, so a server restarted while its old connections
// drain re-binds its port — the classic restart shape. Only DIALER ends (no
// SO_REUSEADDR) block a listener.
func TestDSTNetRelistenWithAcceptedConns(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var relistenErr error
	simulation.Run(1, func() {
		port := make(chan string, 1)
		dialed := make(chan struct{})
		result := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", simulation.HostIP("srv")+":30000")
			port <- "30000"
			go func() {
				c, _ := ln.Accept()
				ln.Close() // the server "stops"; its accepted conn stays open
				ln2, err := Listen("tcp", simulation.HostIP("srv")+":30000")
				relistenErr = err
				if err == nil {
					ln2.Close()
				}
				close(result)
				c.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			c, err := Dial("tcp", simulation.HostIP("srv")+":"+<-port)
			if err != nil {
				panic(err)
			}
			close(dialed)
			<-result
			c.Close()
		})
		<-dialed
		<-result
	})
	if relistenErr != nil {
		t.Errorf("re-listen on the restarted server's port with accepted conns draining = %v, want success (accepted ends inherit SO_REUSEADDR)", relistenErr)
	}
}

// TestDSTNetBacklogFullDialTimesOut: a full accept backlog drops the SYN
// (tcp_abort_on_overflow=0); a deadline-less dial retransmits into the
// saturated listener and fails ETIMEDOUT at the horizon instead of hanging
// forever. A dial whose retries are still running when a slot frees connects.
func TestDSTNetBacklogFullDialTimesOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: time.Second}}
	var fullErr error
	var fullDur time.Duration
	var freedErr error
	simulation.RunWith(1, opts, func() {
		var ln Listener
		ready := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ = Listen("tcp", simulation.HostIP("srv")+":20000")
			close(ready)
		})
		<-ready
		defer ln.Close()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			target := simulation.HostIP("srv") + ":20000"
			backlog := cap(ln.(*dstListener).accept)
			for i := 0; i < backlog; i++ { // saturate the backlog
				if _, err := Dial("tcp", target); err != nil {
					panic(err)
				}
			}
			t0 := time.Now()
			_, fullErr = Dial("tcp", target) // one past the backlog: the queue never drains
			fullDur = time.Since(t0)

			// Free one slot mid-retry: the next dial's "retransmitted SYN"
			// lands and the connect completes.
			dialDone := make(chan struct{})
			go func() {
				_, freedErr = Dial("tcp", target)
				close(dialDone)
			}()
			time.Sleep(200 * time.Millisecond) // the dial is parked on the full backlog
			if _, err := ln.Accept(); err != nil {
				panic(err)
			}
			<-dialDone
		})
	})
	if !errors.Is(fullErr, syscall.ETIMEDOUT) {
		t.Errorf("dial into a permanently full backlog = %v, want ETIMEDOUT (the SYN is dropped, retries exhaust)", fullErr)
	}
	if fullDur != time.Second {
		t.Errorf("full-backlog dial failed after %v, want the 1s retransmit horizon", fullDur)
	}
	if freedErr != nil {
		t.Errorf("dial with a slot freed mid-retry = %v, want success (the retransmitted SYN lands)", freedErr)
	}
}

// TestDSTNetCloseWithUnreadDataResetsPeer: the kernel's close(2) conditional
// on the USER-CALLED Close path — an end closed with unread received data
// answers the peer with RST: the peer's next read fails ECONNRESET WITHOUT
// draining (a reply the closer sent moments earlier is destroyed with the
// receive queue, the same no-drain rule every RST teardown follows).
func TestDSTNetCloseWithUnreadDataResetsPeer(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}
	var n int
	var readErr error
	simulation.RunWith(1, opts, func() {
		var ln Listener
		ready := make(chan struct{})
		closed := make(chan struct{})
		readDone := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ = Listen("tcp", simulation.HostIP("srv")+":20000")
			close(ready)
			go func() {
				c, _ := ln.Accept()
				time.Sleep(300 * time.Millisecond) // the client's "data" is delivered, UNREAD
				c.Write([]byte("resp"))            // in flight when the close lands
				c.Close()                          // unread inbound -> RST, not FIN
				close(closed)
			}()
		})
		<-ready
		defer ln.Close()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			c, err := Dial("tcp", simulation.HostIP("srv")+":20000")
			if err != nil {
				panic(err)
			}
			c.Write([]byte("data"))
			<-closed
			n, readErr = c.Read(make([]byte, 8))
			close(readDone)
			c.Close()
		})
		<-readDone
	})
	if n != 0 || !errors.Is(readErr, syscall.ECONNRESET) {
		t.Errorf("read after the peer closed with unread data = (%d, %v), want (0, ECONNRESET): the RST destroys the receive queue, nothing drains", n, readErr)
	}
}

// TestDSTNetCloseAfterDrainingFINs: the other arm of the conditional — an end
// that READ everything before closing FINs, and the peer drains buffered
// bytes to io.EOF. Guards the RST arm against over-firing.
func TestDSTNetCloseAfterDrainingFINs(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}
	var n int
	var firstErr, secondErr error
	var buf [8]byte
	simulation.RunWith(1, opts, func() {
		var ln Listener
		ready := make(chan struct{})
		closed := make(chan struct{})
		readDone := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ = Listen("tcp", simulation.HostIP("srv")+":20000")
			close(ready)
			go func() {
				c, _ := ln.Accept()
				b := make([]byte, 8)
				if _, err := c.Read(b); err != nil { // drain the client's data
					panic(err)
				}
				c.Write([]byte("resp"))
				c.Close() // nothing unread -> graceful FIN
				close(closed)
			}()
		})
		<-ready
		defer ln.Close()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			c, err := Dial("tcp", simulation.HostIP("srv")+":20000")
			if err != nil {
				panic(err)
			}
			c.Write([]byte("data"))
			<-closed
			n, firstErr = c.Read(buf[:])
			_, secondErr = c.Read(make([]byte, 8))
			close(readDone)
			c.Close()
		})
		<-readDone
	})
	if n != 4 || string(buf[:4]) != "resp" || firstErr != nil {
		t.Errorf("read after a graceful peer close = (%d, %q, %v), want (4, %q, nil): a FIN lets the peer drain", n, buf[:n], firstErr, "resp")
	}
	if secondErr != io.EOF {
		t.Errorf("second read after the drain = %v, want io.EOF", secondErr)
	}
}

// TestDSTNetCloseBeforeDeliveryStillResets: bytes still IN FLIGHT toward the
// closing end count as queued — the recorded collapse: the sim RSTs
// immediately, one of the two orderings the real close-vs-arrival race
// produces. A Close landing before the inbound data's delivery must RST (the
// peer reads ECONNRESET), never FIN (the peer would read io.EOF).
func TestDSTNetCloseBeforeDeliveryStillResets(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 100 * time.Millisecond}}
	var readErr error
	simulation.RunWith(1, opts, func() {
		var ln Listener
		ready := make(chan struct{})
		closed := make(chan struct{})
		readDone := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ = Listen("tcp", simulation.HostIP("srv")+":20000")
			close(ready)
			go func() {
				c, _ := ln.Accept()
				// Accept returns at t=100ms (the SYN's half-RTT); the client's
				// Dial returns at t=200ms and writes; delivery lands at
				// t=300ms. Close at t=250ms: written, in flight, undelivered.
				time.Sleep(150 * time.Millisecond)
				c.Close() // in-flight inbound counts as queued -> RST
				close(closed)
			}()
		})
		<-ready
		defer ln.Close()
		simulation.Host("cli", simulation.HostConfig{}, func() {
			c, err := Dial("tcp", simulation.HostIP("srv")+":20000")
			if err != nil {
				panic(err)
			}
			c.Write([]byte("data")) // at t=200ms; arrives t=300ms — after the close
			<-closed
			_, readErr = c.Read(make([]byte, 8))
			close(readDone)
			c.Close()
		})
		<-readDone
	})
	if !errors.Is(readErr, syscall.ECONNRESET) {
		t.Errorf("read after the peer closed with our bytes in flight = %v, want ECONNRESET: in-flight bytes count as queued (the recorded collapse)", readErr)
	}
}

// TestDSTNetListenPortAllocatorWrapsAndReclaims: the :0 listener allocator
// wraps within [10000, 65535] and reclaims closed ports on the next pass, as
// real kernels reuse freed ephemeral ports — a long-lived run can listen and
// close indefinitely (the unwrapped counter exhausted after ~55k listens with
// every port free).
func TestDSTNetListenPortAllocatorWrapsAndReclaims(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var ports []string
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			first, err := Listen("tcp", ":0") // closed below, reclaimed on wrap
			if err != nil {
				panic(err)
			}
			if _, p, _ := SplitHostPort(first.Addr().String()); p != "10000" {
				panic("first :0 allocation = " + p + ", want 10000 (the reclaim leg's premise)")
			}
			first.Close()

			dstNet.mu.Lock()
			dstNetRoll()
			dstNet.nextListenPort = 65535 // one candidate before the wrap
			dstNet.mu.Unlock()

			for i := 0; i < 2; i++ { // 65535, then wrap -> the reclaimed 10000
				ln, err := Listen("tcp", ":0")
				if err != nil {
					panic(err)
				}
				defer ln.Close()
				_, p, _ := SplitHostPort(ln.Addr().String())
				ports = append(ports, p)
			}
		})
	})
	want := []string{"65535", "10000"}
	for i, w := range want {
		if ports[i] != w {
			t.Fatalf("allocation %d = %s, want %s (wrap at 65535, reclaim the closed 10000)", i, ports[i], w)
		}
	}
}

// TestDSTNetListenPortExhaustionEADDRINUSE: a fully live listener range fails
// the next :0 allocation with EADDRINUSE — bind(2)'s exhaustion identity —
// carrying the requested address, never an unbounded scan or a bare string.
// Specific-IP listens keep every conflict probe O(1) (a wildcard listen's
// probe scans all keys, and 55k of those is quadratic).
func TestDSTNetListenPortExhaustionEADDRINUSE(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var exhaustErr error
	simulation.Run(1, func() {
		simulation.Host("h", simulation.HostConfig{}, func() {
			me := simulation.HostIP("h")
			lns := make([]Listener, 0, 65535-10000+1)
			for port := 10000; port <= 65535; port++ {
				ln, err := Listen("tcp4", me+":"+strconv.Itoa(port))
				if err != nil {
					panic(err)
				}
				lns = append(lns, ln)
			}
			_, exhaustErr = Listen("tcp4", me+":0")
			for _, ln := range lns {
				ln.Close()
			}
		})
	})
	if !errors.Is(exhaustErr, syscall.EADDRINUSE) {
		t.Fatalf("listen with the whole ephemeral range live = %v, want EADDRINUSE (bind exhaustion)", exhaustErr)
	}
	var op *OpError
	if !errors.As(exhaustErr, &op) || op.Addr == nil {
		t.Fatalf("exhaustion error = %#v, want an OpError carrying the requested address", exhaustErr)
	}
}
