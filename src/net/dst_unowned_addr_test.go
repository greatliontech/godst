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
		simulation.Host("late", simulation.HostConfig{}, func() {
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

// TestDSTNetUnownedAddressLateImplicitProcess: the implicit-host arm of the
// late-declaration boot — a top-level Process (no Host) allocates an implicit
// dedicated host, which must relay host-up exactly like an explicit Host
// declaration, waking a dial parked on the previously unowned address.
// Mutation: dropping internHost's fresh-host relay leaves the parked dial
// asleep until the horizon.
func TestDSTNetUnownedAddressLateImplicitProcess(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: 10 * time.Second}}
	simulation.RunWith(1, opts, func() {
		res := make(chan error, 1)
		go func() {
			// The first interned host below gets id 1 → 10.0.0.1. Bounded
			// refused-retry, as in the explicit-Host test above.
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
		done := make(chan struct{})
		go simulation.Process("late", func() { // implicit dedicated host: id 1
			ln, err := Listen("tcp", ":12345")
			if err != nil {
				t.Errorf("listen on the implicit host: %v", err)
				return
			}
			go func() {
				if c, err := ln.Accept(); err == nil {
					c.Close()
				}
				ln.Close()
			}()
			<-done
		})
		if err := <-res; err != nil {
			t.Errorf("dial after the implicit-host Process was declared = %v, want eventual success", err)
		}
		close(done)
	})
}
