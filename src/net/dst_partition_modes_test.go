// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

// TestDSTNetPartitionRefuseConnect is the refuse-connect-mode regression: a Dial
// across a PartitionRefuse cut fails FAST with ECONNREFUSED (the peer answers RST,
// "peer down"), where a plain Partition blackholes the dial (the SYN is dropped, the
// dial blocks until heal/deadline/horizon). Both are real TCP outcomes; the mode is
// the SUT's to choose.
func TestDSTNetPartitionRefuseConnect(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var dialErr error
	var dialDur time.Duration
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			// No Accept: a refused dial never reaches the backlog.
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			simulation.PartitionRefuse("srv", "cli") // refuse mode, before dialing
			t0 := time.Now()
			_, dialErr = Dial("tcp", simulation.HostIP("srv")+":"+p)
			dialDur = time.Since(t0)
		})
	})
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Errorf("dial across a refuse-partition = %v, want ECONNREFUSED (fast, peer-down)", dialErr)
	}
	if dialDur != 0 {
		t.Errorf("refuse-partition dial took %v, want 0 virtual time (fails fast, never blocks)", dialDur)
	}
}

// TestDSTNetPartitionOneWay is the asymmetric-partition regression: PartitionOneWay
// cuts ONLY from→to, so from's writes are held at the cut while to→from still
// delivers. A symmetric cut would hold both — this pins that exactly one direction is
// severed.
func TestDSTNetPartitionOneWay(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var srvGot, cliGot string
	var srvReadErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		cutDone := make(chan struct{}) // the cut is applied and cli has written its (held) byte
		srvDone := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				<-cutDone
				// cli→srv is cut: cli's write is held, so this read must time out.
				buf := make([]byte, 8)
				c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
				n, err := c.Read(buf)
				srvGot = string(buf[:n])
				srvReadErr = err
				// srv→cli is NOT cut: this write reaches cli.
				c.Write([]byte("y"))
				close(srvDone)
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("srv")+":"+p)
			simulation.PartitionOneWay("cli", "srv") // cut only cli→srv
			c.Write([]byte("x"))                     // held: never reaches srv
			close(cutDone)
			buf := make([]byte, 8)
			c.SetReadDeadline(time.Now().Add(time.Second))
			n, _ := c.Read(buf) // srv→cli still flows: receives "y"
			cliGot = string(buf[:n])
			<-srvDone
			c.Close()
		})
	})
	if !errors.Is(srvReadErr, os.ErrDeadlineExceeded) {
		t.Errorf("srv read of cli's cut-direction write = %q, %v; want a timeout (cli→srv is severed)", srvGot, srvReadErr)
	}
	if cliGot != "y" {
		t.Errorf("cli read of srv's write = %q, want %q (srv→cli still flows under a one-way cli→srv cut)", cliGot, "y")
	}
}

// TestDSTNetPartitionOneWayReverseDialFails pins the both-handshake-directions dial
// check: under a one-way A→B cut, a dial in the REVERSE direction (B→A) must still
// fail — its SYN reaches A but the SYN-ACK travels A→B, which is cut, so the handshake
// never completes. Without checking the target→dialer direction this reverse dial
// would wrongly succeed (a sim-only connect the real network cannot make).
func TestDSTNetPartitionOneWayReverseDialFails(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: time.Second}}
	var dialErr error
	simulation.RunWith(1, opts, func() {
		aPort := make(chan string, 1)
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			aPort <- p
			// A only listens: a reverse dial's SYN arrives, but its SYN-ACK (A→B) is dropped.
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-aPort
			simulation.PartitionOneWay("A", "B") // cut only A→B
			// B dials A: SYN B→A arrives, SYN-ACK A→B is dropped → blackhole → horizon.
			_, dialErr = Dial("tcp", simulation.HostIP("A")+":"+p)
		})
	})
	if !errors.Is(dialErr, syscall.ETIMEDOUT) {
		t.Errorf("dial B→A under a one-way A→B cut = %v; want ETIMEDOUT (the returning SYN-ACK A→B is dropped)", dialErr)
	}
}

// TestDSTNetPartitionRefuseWithIsolateBlackholes pins that BLACKHOLE DOMINATES REFUSE:
// a refuse-partition combined with an Isolate (a dropped-packet cut) on the same pair
// must BLACKHOLE the dial (time out), not refuse — an isolated host drops the SYN, so
// no RST can travel back, and reporting ECONNREFUSED would be a sim-only false failure
// the real dropped-packet path never produces.
func TestDSTNetPartitionRefuseWithIsolateBlackholes(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: time.Second}}
	var dialErr error
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			simulation.PartitionRefuse("srv", "cli") // refuse-mode cut...
			simulation.Isolate("srv")                // ...plus a dropped-packet cut on srv
			_, dialErr = Dial("tcp", simulation.HostIP("srv")+":"+p)
		})
	})
	if errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Errorf("dial into a refuse+isolated peer = %v; must NOT be ECONNREFUSED (the isolation drops the SYN, so no RST returns — it must blackhole)", dialErr)
	}
	if !errors.Is(dialErr, syscall.ETIMEDOUT) {
		t.Errorf("dial into a refuse+isolated peer = %v; want ETIMEDOUT (blackhole dominates refuse)", dialErr)
	}
}
