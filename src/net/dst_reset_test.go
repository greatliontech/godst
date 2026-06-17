// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"errors"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

// These tests exercise connection-reset targeting (simulation.Reset host-pair,
// ResetProcess). Invariants:
//   - a reset delivers ECONNRESET to BOTH ends of every targeted conn;
//   - it drops in-flight buffered bytes (a real RST discards them) — DST-FAULT-SOUND;
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
