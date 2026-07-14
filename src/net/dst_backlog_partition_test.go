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

// backlogParkPartition drives the backlog-park reproducer: fill a listener's
// accept backlog, park a dial on the full queue, cut the link (per cut), free
// a slot with an Accept, and observe the parked dial. It returns the dial's
// error (nil = completed) and whether the dial was still pending 300ms of
// virtual time after the slot freed under the active cut.
func backlogParkPartition(t *testing.T, cut func(), heal func()) (dialErr error, heldDuringCut bool) {
	t.Helper()
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: time.Second}}
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		acceptNow := make(chan struct{})
		accepted := make(chan struct{})
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				<-acceptNow
				if _, err := ln.Accept(); err != nil {
					t.Errorf("accept: %v", err)
				}
				close(accepted)
				<-done
				ln.Close()
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			addr := simulation.HostIP("srv") + ":" + p
			conns := make([]Conn, 0, 128)
			for i := 0; i < 128; i++ { // fill the accept backlog
				c, err := Dial("tcp", addr)
				if err != nil {
					t.Fatalf("backlog fill dial %d: %v", i, err)
				}
				conns = append(conns, c)
			}
			res := make(chan error, 1)
			go func() {
				_, err := Dial("tcp", addr) // parks: the backlog is full
				res <- err
			}()
			time.Sleep(10 * time.Millisecond) // the dial is parked on the full backlog
			cut()
			close(acceptNow) // the server frees a slot DURING the cut
			<-accepted
			time.Sleep(300 * time.Millisecond) // well before the 1s horizon
			select {
			case err := <-res:
				// The parked send landed across an active cut — the
				// reproducer's false negative.
				dialErr = err
			default:
				heldDuringCut = true
				if heal != nil {
					heal()
				}
				dialErr = <-res
			}
			close(done)
			for _, c := range conns {
				c.Close()
			}
		})
	})
	return dialErr, heldDuringCut
}

// TestDSTNetBacklogParkPartitionTimesOut closes the backlog-park partition
// hole: a dial parked on a full accept backlog models the SYN's
// undeliverability — while the handshake path is cut, an Accept freeing a
// slot must NOT complete this dial (production's retransmitted SYN is dropped
// for the cut's whole duration), and a PERMANENT cut ends in ETIMEDOUT at the
// retransmit horizon, never success. Pinned in both one-way orientations plus
// the symmetric cut: either direction's loss prevents the handshake.
// Mutation: dropping the park loop's cut re-check lets the freed slot commit
// the parked send and the dial succeeds across the cut (heldDuringCut false).
func TestDSTNetBacklogParkPartitionTimesOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	cuts := map[string]func(){
		"cli-to-srv": func() { simulation.PartitionOneWay("cli", "srv") }, // the SYN's direction
		"srv-to-cli": func() { simulation.PartitionOneWay("srv", "cli") }, // the SYN-ACK's direction
		"symmetric":  func() { simulation.Partition("cli", "srv") },
	}
	for name, cut := range cuts {
		dialErr, held := backlogParkPartition(t, cut, nil)
		if !held {
			t.Errorf("%s: the parked dial completed while the cut was active (err=%v); a slot freed during a partition must not land the SYN", name, dialErr)
			continue
		}
		if !errors.Is(dialErr, syscall.ETIMEDOUT) {
			t.Errorf("%s: parked dial across a permanent cut = %v, want ETIMEDOUT at the retransmit horizon", name, dialErr)
		}
	}
}

// TestDSTNetBacklogParkPartitionHealCompletes: the same park completes when
// the cut HEALS before the horizon — the retransmitted SYN reaches the freed
// slot, exactly the front-door blackhole loop's heal-or-horizon contract.
// Both one-way orientations.
func TestDSTNetBacklogParkPartitionHealCompletes(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	cuts := map[string]struct{ cut, heal func() }{
		"cli-to-srv": {func() { simulation.PartitionOneWay("cli", "srv") }, func() { simulation.Heal("cli", "srv") }},
		"srv-to-cli": {func() { simulation.PartitionOneWay("srv", "cli") }, func() { simulation.Heal("srv", "cli") }},
	}
	for name, fns := range cuts {
		dialErr, held := backlogParkPartition(t, fns.cut, fns.heal)
		if !held {
			t.Errorf("%s: the parked dial completed while the cut was active", name)
			continue
		}
		if dialErr != nil {
			t.Errorf("%s: parked dial after heal-before-horizon = %v, want success", name, dialErr)
		}
	}
}

// synackPartition drives the SYN-ACK leg: on a 100ms link, cut the named
// direction 150ms into a dial — after the SYN landed, while the SYN-ACK is in
// flight — and observe the dial: held-then-ETIMEDOUT for a permanent
// returning-direction cut, completion on heal, and completion DESPITE a cut
// of the forward direction (connect(2) completes on receiving the SYN-ACK;
// the final ACK's loss is the server child's problem, not the dialer's).
func synackPartition(t *testing.T, cut func(), heal func()) (dialErr error, heldDuringCut bool) {
	t.Helper()
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHostLatency:  100 * time.Millisecond,
		RetransmitTimeout: time.Second,
	}}
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				<-done
				ln.Close() // unblocks the accept loop with ErrClosed
			}()
			go func() {
				for {
					if _, err := ln.Accept(); err != nil {
						return
					}
				}
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			addr := simulation.HostIP("srv") + ":" + p
			res := make(chan error, 1)
			go func() {
				_, err := Dial("tcp", addr)
				res <- err
			}()
			time.Sleep(150 * time.Millisecond) // SYN landed (100ms); SYN-ACK mid-flight
			cut()
			time.Sleep(300 * time.Millisecond) // past the un-cut completion instant (200ms), well before the horizon
			select {
			case err := <-res:
				dialErr = err
			default:
				heldDuringCut = true
				if heal != nil {
					heal()
				}
				dialErr = <-res
			}
			close(done)
		})
	})
	return dialErr, heldDuringCut
}

func TestDSTNetSYNACKPartitionTimesOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	// Permanent cut of the RETURNING direction: the SYN-ACK retransmits into
	// the void; connect fails ETIMEDOUT at the horizon, never succeeds.
	dialErr, held := synackPartition(t, func() { simulation.PartitionOneWay("srv", "cli") }, nil)
	if !held {
		t.Fatalf("dial completed across a cut of the SYN-ACK direction (err=%v)", dialErr)
	}
	if !errors.Is(dialErr, syscall.ETIMEDOUT) {
		t.Errorf("dial with the SYN-ACK direction permanently cut = %v, want ETIMEDOUT", dialErr)
	}
}

func TestDSTNetSYNACKPartitionHealCompletes(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	dialErr, held := synackPartition(t, func() { simulation.PartitionOneWay("srv", "cli") }, func() { simulation.Heal("srv", "cli") })
	if !held {
		t.Fatal("dial completed across a cut of the SYN-ACK direction")
	}
	if dialErr != nil {
		t.Errorf("dial after the SYN-ACK cut healed = %v, want success", dialErr)
	}
}

func TestDSTNetSYNACKForwardCutStillCompletes(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	// A cut of the FORWARD direction landing after the SYN traversed must not
	// hold the dialer: connect(2) completes on receiving the SYN-ACK, which
	// travels the un-cut returning direction (the lost final ACK is the
	// server child's failure surface, not the dialer's).
	dialErr, held := synackPartition(t, func() { simulation.PartitionOneWay("cli", "srv") }, nil)
	if held {
		t.Fatal("dial held by a forward-direction cut; connect completes on SYN-ACK receipt")
	}
	if dialErr != nil {
		t.Errorf("dial with the forward direction cut post-SYN = %v, want success", dialErr)
	}
}

// TestDSTNetBacklogParkRefuseCutRefuses: a REFUSE-mode cut landing while the
// dial is parked on the full backlog answers the retransmitted SYN with RST —
// the SYN_SENT ECONNREFUSED, immediately (the recorded refuse timing
// simplification), never a blackhole wait. Mutation: dropping the park loop's
// refuse arm falls through to the heal-or-horizon wait and the dial times out
// instead.
func TestDSTNetBacklogParkRefuseCutRefuses(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	dialErr, held := backlogParkPartition(t, func() { simulation.PartitionRefuse("cli", "srv") }, nil)
	if held {
		t.Fatal("refuse-mode cut left the parked dial waiting; a refuse cut answers the SYN with RST")
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Errorf("parked dial under a refuse-mode cut = %v, want ECONNREFUSED (the SYN_SENT identity)", dialErr)
	}
}

// TestDSTNetSYNACKRefuseCutRefuses: a refuse-mode cut active when the SYN-ACK
// would complete answers the handshake with RST — ECONNREFUSED, the SYN_SENT
// identity — instead of the blackhole wait. Mutation: dropping the ack gate's
// pure-refuse arm turns this into a horizon timeout.
func TestDSTNetSYNACKRefuseCutRefuses(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	dialErr, held := synackPartition(t, func() { simulation.PartitionRefuse("srv", "cli") }, nil)
	if held {
		t.Fatal("refuse-mode cut left the SYN-ACK wait parked; a refuse cut answers with RST")
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Errorf("SYN-ACK completion under a refuse-mode cut = %v, want ECONNREFUSED", dialErr)
	}
}

// TestDSTNetSYNACKHealObservesReset: the ack gate's success path re-checks
// the deciding observers exactly as dstConnectSYNACK's zero-latency arm does.
// Interleaving: the dial parks in the ack gate under a returning-direction
// cut; one fault goroutine then — without yielding — touches an UNRELATED
// pair (closing the fetched partition wake, which commits the parked select
// to its wake case before rstKill can), injects a Reset on the dialing pair,
// and heals the cut. The resumed gate sees no cut; it must observe the reset
// and abort with the SYN_SENT ECONNREFUSED — never return an already-reset
// conn as an established dial. Mutation: dropping the success-path re-check
// returns success and the assertion sees a nil error.
func TestDSTNetSYNACKHealObservesReset(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{
		CrossHostLatency:  100 * time.Millisecond,
		RetransmitTimeout: time.Second,
	}}
	var dialErr error
	gotResult := false
	simulation.RunWith(1, opts, func() {
		simulation.Host("x", simulation.HostConfig{}, func() {}) // the unrelated pair
		simulation.Host("y", simulation.HostConfig{}, func() {})
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("srv", simulation.HostConfig{}, func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				<-done
				ln.Close()
			}()
			go func() {
				for {
					if _, err := ln.Accept(); err != nil {
						return
					}
				}
			}()
		})
		simulation.Host("cli", simulation.HostConfig{}, func() {
			p := <-port
			addr := simulation.HostIP("srv") + ":" + p
			res := make(chan error, 1)
			go func() {
				_, err := Dial("tcp", addr)
				res <- err
			}()
			time.Sleep(150 * time.Millisecond) // SYN landed; SYN-ACK mid-flight
			simulation.PartitionOneWay("srv", "cli")
			time.Sleep(100 * time.Millisecond) // the dial is parked in the ack gate
			// No yield between the three ops: the unrelated wake commits the
			// parked select, then the reset and heal land before the dial runs.
			simulation.Partition("x", "y")
			simulation.Reset("srv", "cli")
			simulation.Heal("srv", "cli")
			select {
			case dialErr = <-res:
				gotResult = true
			case <-time.After(2 * time.Second):
			}
			close(done)
		})
	})
	if !gotResult {
		t.Fatal("dial did not resolve after the heal")
	}
	if dialErr == nil {
		t.Fatal("dial returned success on a connection reset while the ack gate was parked")
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		t.Errorf("dial after reset-under-cut = %v, want ECONNREFUSED (SYN_SENT identity)", dialErr)
	}
}
