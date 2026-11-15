// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

// TestDSTNetUnownedAddressDialTimesOut: a dial to a routable 10.x address NO
// declared host owns blackholes — nothing answers a SYN to an unowned
// address, and an RST needs a live kernel — so a deadline-less dial fails
// ETIMEDOUT at the retransmit horizon, never the ECONNREFUSED a live kernel
// would answer. (The peer-down/unreachable split: refuse requires a machine.)
// Mutation: dropping the declared-host check in dstDialCut returns the old
// instant ECONNREFUSED.
func TestDSTNetUnownedAddressDialTimesOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: time.Second}}
	var dialErr error
	var elapsed time.Duration
	simulation.RunWith(1, opts, func() {
		start := time.Now()
		_, dialErr = Dial("tcp", "10.0.0.42:9") // no host with id 42 is ever declared
		elapsed = time.Since(start)
	})
	if !errors.Is(dialErr, syscall.ETIMEDOUT) {
		t.Fatalf("dial to an unowned address = %v, want ETIMEDOUT at the retransmit horizon (nothing answers an unowned address's SYN)", dialErr)
	}
	if elapsed < time.Second {
		t.Errorf("dial failed after %v of virtual time, want the full 1s retransmit horizon (an instant failure is the refuse shape)", elapsed)
	}
}

// TestDSTNetUnownedAddressDialDeadline: the blackhole respects the dial
// context — a deadline shorter than the horizon fails with the deadline
// identity, exactly like a dial into a partition.
func TestDSTNetUnownedAddressDialDeadline(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var dialErr error
	simulation.Run(1, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		var d Dialer
		_, dialErr = d.DialContext(ctx, "tcp", "10.0.0.42:9")
	})
	if !errors.Is(dialErr, os.ErrDeadlineExceeded) && !errors.Is(dialErr, context.DeadlineExceeded) {
		t.Fatalf("deadline-bounded dial to an unowned address = %v, want the deadline identity", dialErr)
	}
}

// TestDSTNetUnownedAddressLateDeclaration: a machine declared at the address
// mid-dial is a machine BOOTING — the parked dial's retransmitted SYN reaches
// the fresh kernel (the declaration relays host-up, waking the blackhole
// wait), and the address becomes connectable: first ECONNREFUSED while no
// listener is up, then connect. The dialer retries as production clients do;
// it must reach the listener well before the retransmit horizon would have
// killed a still-unowned address.
func TestDSTNetUnownedAddressLateDeclaration(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: 10 * time.Second}}
	var connected bool
	simulation.RunWith(1, opts, func() {
		res := make(chan error, 1)
		go func() {
			// The first Host declared below gets id 1 → 10.0.0.1. Dial it
			// BEFORE it exists: the SYN blackholes. The refused-retry loop is
			// BOUNDED so a failing run reports instead of spinning virtual
			// time forever (durable sleeps would keep the wedge detector's
			// quiescence window fresh).
			for tries := 0; ; tries++ {
				_, err := Dial("tcp", "10.0.0.1:12345")
				if err == nil {
					res <- nil
					return
				}
				if errors.Is(err, syscall.ECONNREFUSED) && tries < 500 {
					// Booted, no listener yet: retry, as production does.
					time.Sleep(10 * time.Millisecond)
					continue
				}
				res <- err
				return
			}
		}()
		time.Sleep(500 * time.Millisecond) // the dial is parked on the unowned address
		simulation.Host("late", func() {
			ln, err := Listen("tcp", ":12345")
			if err != nil {
				t.Errorf("listen on the late host: %v", err)
				return
			}
			go func() {
				if c, err := ln.Accept(); err == nil {
					c.Close()
				}
				ln.Close()
			}()
		})
		if err := <-res; err != nil {
			t.Errorf("dial after the address's host was declared = %v, want eventual success (the SYN reaches the booted kernel)", err)
		} else {
			connected = true
		}
	})
	if !connected && !t.Failed() {
		t.Fatal("dial never completed")
	}
}

// TestDSTNetUnownedAddressBareProcessDoesNotClaimHostIP: a top-level Process
// (outside Host) runs on the unnamed root host (0) rather than allocating an
// implicit machine, so a dial parked on 10.0.0.1 stays blackholed across the
// bare Process declaration and wakes only when an explicit Host("late", ...)
// boots host 1 (10.0.0.1).
func TestDSTNetUnownedAddressBareProcessDoesNotClaimHostIP(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: 10 * time.Second}}
	simulation.RunWith(1, opts, func() {
		res := make(chan error, 1)
		go func() {
			for tries := 0; ; tries++ {
				_, err := Dial("tcp", "10.0.0.1:12345")
				if err == nil {
					res <- nil
					return
				}
				if errors.Is(err, syscall.ECONNREFUSED) && tries < 500 {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				res <- err
				return
			}
		}()
		time.Sleep(500 * time.Millisecond) // the dial is parked on the unowned address
		bareDone := make(chan struct{})
		go simulation.Process("late", func() {
			ln, err := Listen("tcp", ":12345")
			if err != nil {
				t.Errorf("listen on root process: %v", err)
				return
			}
			defer ln.Close()
			<-bareDone
		})
		time.Sleep(500 * time.Millisecond)
		select {
		case err := <-res:
			t.Fatalf("dial resolved (%v) from a bare Process before Host(\"late\") booted 10.0.0.1", err)
		default:
		}
		simulation.Host("late", func() {
			ln, err := Listen("tcp", ":12345")
			if err != nil {
				t.Errorf("listen on the late host: %v", err)
				return
			}
			go func() {
				if c, err := ln.Accept(); err == nil {
					c.Close()
				}
				ln.Close()
			}()
		})
		if err := <-res; err != nil {
			t.Errorf("dial after Host(\"late\") booted 10.0.0.1 = %v, want eventual success", err)
		}
		close(bareDone)
	})
}
