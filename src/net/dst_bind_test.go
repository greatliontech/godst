// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"errors"
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
