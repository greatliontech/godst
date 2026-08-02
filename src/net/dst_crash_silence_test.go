// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package net

// The crash-as-silence contract's conformance suite: a powered-off machine
// emits no packet, so a surviving peer of a host crash observes SILENCE —
// its delivered bytes drain, then nothing — until a modeled law surfaces
// the death: retransmission exhaustion for outstanding bytes, keepalive
// exhaustion for idle connections, or the RST a REBOOTED kernel answers the
// survivor's traffic with. A process crash keeps its immediate RST (the
// kernel survives and answers) — except across a blackhole cut of the
// RST's own direction, which no kernel-emitted segment can traverse; heal
// lets the survivor's probes meet the CLOSED socket's RST.

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

// dstCrashServer starts host "victim" with process "srv": listen, accept,
// optionally write greet, then park until crashed or done.
func dstCrashServer(port chan<- string, done <-chan struct{}, greet string) {
	simulation.Host("victim", simulation.HostConfig{}, func() {
		go simulation.Process("srv", func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				panic(err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			c, err := ln.Accept()
			if err != nil {
				panic(err)
			}
			if greet != "" {
				if _, err := c.Write([]byte(greet)); err != nil {
					panic(err)
				}
			}
			<-done
			c.Close()
			ln.Close()
		})
	})
}

// TestDSTNetCrashHostSurvivorSilence: power loss emits nothing — the
// survivor drains what was delivered before the crash, then its reads see
// pure silence (its own deadline, never a fabricated ECONNRESET or EOF).
func TestDSTNetCrashHostSurvivorSilence(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var greet string
	var silentErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		defer close(done)
		dstCrashServer(port, done, "pre")
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			c, err := (&Dialer{KeepAlive: -1}).Dial("tcp", simulation.HostIP("victim")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 8)
			n, err := c.Read(buf)
			if err != nil {
				panic(err)
			}
			greet = string(buf[:n])
			simulation.CrashHost("victim")
			c.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, silentErr = c.Read(buf)
			c.Close()
		})
	})
	if greet != "pre" {
		t.Errorf("pre-crash delivery = %q, want pre", greet)
	}
	if !errors.Is(silentErr, os.ErrDeadlineExceeded) {
		t.Errorf("read after the host crash = %v, want the survivor's own deadline (silence — power loss emits no packet)", silentErr)
	}
}

// TestDSTNetCrashHostKeepaliveDeath: the keepalive law's dead-host arm — an
// idle survivor with keepalive enabled detects the silent machine at the
// probe schedule's exhaustion, ETIMEDOUT (the grpc-recipe scenario the
// modeling exists for).
func TestDSTNetCrashHostKeepaliveDeath(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var elapsed time.Duration
	var readErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		defer close(done)
		dstCrashServer(port, done, "x")
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			d := Dialer{KeepAliveConfig: KeepAliveConfig{Enable: true, Idle: 10 * time.Second, Interval: 5 * time.Second, Count: 2}}
			c, err := d.Dial("tcp", simulation.HostIP("victim")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 1)
			if _, err := c.Read(buf); err != nil { // the greet: activity anchor
				panic(err)
			}
			start := time.Now()
			simulation.CrashHost("victim")
			_, readErr = c.Read(buf)
			elapsed = time.Since(start)
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ETIMEDOUT) {
		t.Fatalf("idle read against the crashed host = %v, want ETIMEDOUT (keepalive exhaustion)", readErr)
	}
	// First probe at idle (10s) after the greet, death at the fire with two
	// probes out: ~20s.
	if elapsed < 19*time.Second || elapsed > 23*time.Second {
		t.Errorf("keepalive death after %v, want ~20s", elapsed)
	}
}

// TestDSTNetCrashHostOutstandingTimesOut: bytes toward the dead machine are
// permanently unacknowledged — the retransmit horizon kills the conn even
// with NO partition configured (host death arms it as a cut does), with the
// one-shot ETIMEDOUT ladder.
func TestDSTNetCrashHostOutstandingTimesOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var elapsed time.Duration
	var readErr, lateWriteErr error
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: 2 * time.Second}}
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		defer close(done)
		dstCrashServer(port, done, "x")
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			c, err := (&Dialer{KeepAlive: -1}).Dial("tcp", simulation.HostIP("victim")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 1)
			if _, err := c.Read(buf); err != nil {
				panic(err)
			}
			simulation.CrashHost("victim")
			start := time.Now()
			if _, err := c.Write([]byte("held")); err != nil {
				panic(err) // the send buffers, as TCP's async send does
			}
			_, readErr = c.Read(buf)
			elapsed = time.Since(start)
			_, lateWriteErr = c.Write([]byte("x"))
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ETIMEDOUT) {
		t.Fatalf("read holding dead bytes = %v, want ETIMEDOUT (retransmission exhaustion, no partition needed)", readErr)
	}
	if elapsed < 2*time.Second || elapsed > 4*time.Second {
		t.Errorf("death after %v, want ~2s (the horizon)", elapsed)
	}
	if !errors.Is(lateWriteErr, syscall.EPIPE) {
		t.Errorf("write after the one-shot = %v, want EPIPE", lateWriteErr)
	}
}

// TestDSTNetCrashHostRebootAnswersRST: the machine reboots (a fresh Host
// declaration) — its fresh kernel knows nothing of the old connection, so
// the survivor's next traffic is answered with RST: the one-shot ECONNRESET,
// never a timeout and never silent success.
func TestDSTNetCrashHostRebootAnswersRST(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var readErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		defer close(done)
		dstCrashServer(port, done, "x")
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			c, err := (&Dialer{KeepAlive: -1}).Dial("tcp", simulation.HostIP("victim")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 1)
			if _, err := c.Read(buf); err != nil {
				panic(err)
			}
			simulation.CrashHost("victim")
			time.Sleep(5 * time.Second) // silent while off
			simulation.Host("victim", simulation.HostConfig{}, func() {}) // reboot
			if _, err := c.Write([]byte("probe")); err != nil {
				panic(err) // the send itself succeeds; the RST answers it
			}
			_, readErr = c.Read(buf)
			c.Close()
		})
	})
	if !errors.Is(readErr, syscall.ECONNRESET) {
		t.Errorf("read after probing the rebooted kernel = %v, want ECONNRESET", readErr)
	}
}

// TestDSTNetCrashHostUnderCutStaysSilent: a crash behind an active
// blackhole cut is indistinguishable from the cut itself — no RST reaches
// the survivor even after the cut heals (the machine is off; a heal
// delivers nothing), and held bytes die at the retransmit horizon.
func TestDSTNetCrashHostUnderCutStaysSilent(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var deadlineErr, horizonErr error
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: 20 * time.Second}}
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		defer close(done)
		dstCrashServer(port, done, "x")
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			c, err := (&Dialer{KeepAlive: -1}).Dial("tcp", simulation.HostIP("victim")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 1)
			if _, err := c.Read(buf); err != nil {
				panic(err)
			}
			simulation.Partition("survivor", "victim")
			c.Write([]byte("held"))
			simulation.CrashHost("victim")
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, deadlineErr = c.Read(buf) // silence through cut+crash: the deadline fires first
			simulation.Heal("survivor", "victim")
			c.SetReadDeadline(time.Time{})
			_, horizonErr = c.Read(buf) // the machine is still off: held bytes exhaust at the horizon
			c.Close()
		})
	})
	if !errors.Is(deadlineErr, os.ErrDeadlineExceeded) {
		t.Errorf("read behind cut+crash = %v, want the deadline (no RST can traverse, none was emitted)", deadlineErr)
	}
	if !errors.Is(horizonErr, syscall.ETIMEDOUT) {
		t.Errorf("read after heal with the machine off = %v, want ETIMEDOUT (retransmission exhaustion)", horizonErr)
	}
}

// TestDSTNetCrashHostAppClosedEndDrainsToEOF: a victim end the application
// closed BEFORE the power loss already has its data and FIN on the wire —
// the survivor drains and reads a clean EOF, exactly as the pre-crash
// teardown left it (no packet exists for the crash to add).
func TestDSTNetCrashHostAppClosedEndDrainsToEOF(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var got string
	var eofErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		closed := make(chan struct{})
		done := make(chan struct{})
		defer close(done)
		simulation.Host("victim", simulation.HostConfig{}, func() {
			go simulation.Process("srv", func() {
				ln, _ := Listen("tcp", ":0")
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				c, err := ln.Accept()
				if err != nil {
					panic(err)
				}
				c.Write([]byte("bye"))
				c.Close() // FIN on the wire before the power loss
				close(closed)
				<-done
				ln.Close()
			})
		})
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			c, err := (&Dialer{KeepAlive: -1}).Dial("tcp", simulation.HostIP("victim")+":"+p)
			if err != nil {
				panic(err)
			}
			<-closed
			simulation.CrashHost("victim")
			buf := make([]byte, 8)
			n, err := c.Read(buf)
			if err != nil {
				panic(err)
			}
			got = string(buf[:n])
			_, eofErr = c.Read(buf)
			c.Close()
		})
	})
	if got != "bye" {
		t.Errorf("drained %q, want bye", got)
	}
	if eofErr != io.EOF {
		t.Errorf("read past the drained FIN = %v, want io.EOF", eofErr)
	}
}

// TestDSTNetCrashHostAckedBytesDoNotTimeOut: bytes the victim's kernel
// received and ACKed before the power loss — delivered, whether or not the
// dead application ever read them — are NOT outstanding: production
// retransmits nothing for them, so no retransmission-exhaustion death may
// fire. Only destroyed-unacknowledged bytes arm the horizon.
func TestDSTNetCrashHostAckedBytesDoNotTimeOut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var readErr error
	opts := simulation.Options{Network: simulation.NetworkConfig{RetransmitTimeout: 2 * time.Second}}
	simulation.RunWith(1, opts, func() {
		port := make(chan string, 1)
		accepted := make(chan struct{})
		done := make(chan struct{})
		defer close(done)
		simulation.Host("victim", simulation.HostConfig{}, func() {
			go simulation.Process("srv", func() {
				ln, err := Listen("tcp", ":0")
				if err != nil {
					panic(err)
				}
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				if _, err := ln.Accept(); err != nil { // never reads: the bytes sit ACKed in the receive queue
					panic(err)
				}
				close(accepted)
				<-done
				ln.Close()
			})
		})
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			c, err := (&Dialer{KeepAlive: -1}).Dial("tcp", simulation.HostIP("victim")+":"+p)
			if err != nil {
				panic(err)
			}
			<-accepted
			c.Write([]byte("acked"))          // delivered to the victim kernel's queue
			time.Sleep(10 * time.Millisecond) // …and ACKed, before the lights go out
			simulation.CrashHost("victim")
			c.SetReadDeadline(time.Now().Add(10 * time.Second)) // 5× the horizon
			_, readErr = c.Read(make([]byte, 1))
			c.Close()
		})
	})
	if !errors.Is(readErr, os.ErrDeadlineExceeded) {
		t.Errorf("read with only ACKed bytes behind the crash = %v, want the deadline (nothing outstanding — no retransmit death may fire)", readErr)
	}
}

// TestDSTNetCrashHostBacklogConnSilent: a connection still sitting in the
// crashed host's accept BACKLOG (dialed successfully, never accepted) goes
// silent at its dialer like every other conn the machine held — the
// listener teardown of a powered-off machine emits nothing; a fabricated
// backlog RST would be the same impossible packet as any other.
func TestDSTNetCrashHostBacklogConnSilent(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var writeErr, readErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		defer close(done)
		simulation.Host("victim", simulation.HostConfig{}, func() {
			go simulation.Process("srv", func() {
				ln, err := Listen("tcp", ":0")
				if err != nil {
					panic(err)
				}
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				<-done // never accepts; dies with the machine
				ln.Close()
			})
		})
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			c, err := (&Dialer{KeepAlive: -1}).Dial("tcp", simulation.HostIP("victim")+":"+p)
			if err != nil {
				panic(err)
			}
			simulation.CrashHost("victim")
			_, writeErr = c.Write([]byte("x")) // buffers, as TCP's async send does
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, readErr = c.Read(make([]byte, 1))
			c.Close()
		})
	})
	if writeErr != nil {
		t.Errorf("write to the backlogged conn after the crash = %v, want nil (the send buffers; no RST exists)", writeErr)
	}
	if !errors.Is(readErr, os.ErrDeadlineExceeded) {
		t.Errorf("read = %v, want the deadline (silence — a powered-off machine resets no backlog)", readErr)
	}
}

// TestDSTNetExitCloseRSTHeldByCut: a normal process EXIT closes a socket
// holding unread inbound data — the kernel answers the peer with RST — but
// that RST is a kernel-emitted segment: a blackhole cut of its direction
// swallows it, and the peer sees silence until the heal lets its probes
// meet the CLOSED socket's RST.
func TestDSTNetExitCloseRSTHeldByCut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var deadlineErr, resetErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		accepted := make(chan struct{})
		cut := make(chan struct{})
		simulation.Host("victim", simulation.HostConfig{}, func() {
			go simulation.Process("srv", func() {
				ln, err := Listen("tcp", ":0")
				if err != nil {
					panic(err)
				}
				defer ln.Close()
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				if _, err := ln.Accept(); err != nil { // never reads: inbound stays unread
					panic(err)
				}
				close(accepted)
				<-cut
				// The body returns: process EXIT — the kernel's close finds
				// unread inbound and would RST, but the cut swallows it.
			})
		})
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			c, err := (&Dialer{KeepAlive: -1}).Dial("tcp", simulation.HostIP("victim")+":"+p)
			if err != nil {
				panic(err)
			}
			<-accepted
			c.Write([]byte("unread")) // lands in the victim's receive queue, never read
			time.Sleep(10 * time.Millisecond)
			simulation.PartitionOneWay("victim", "survivor") // the RST's direction is cut
			close(cut)
			time.Sleep(10 * time.Millisecond) // the exit's close runs behind the cut
			// Outstanding bytes are what production keeps retransmitting —
			// a silent reader with NOTHING in flight elicits no RST even
			// after heal (it hangs on a real kernel too; the recorded
			// nothing-outstanding shape). This write is destroyed at the
			// dead socket, unacknowledged.
			c.Write([]byte("probe"))
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, deadlineErr = c.Read(make([]byte, 1))
			simulation.Heal("victim", "survivor")
			c.SetReadDeadline(time.Time{})
			_, resetErr = c.Read(make([]byte, 1))
			c.Close()
		})
	})
	if !errors.Is(deadlineErr, os.ErrDeadlineExceeded) {
		t.Errorf("read while the cut holds the exit RST = %v, want the deadline", deadlineErr)
	}
	if !errors.Is(resetErr, syscall.ECONNRESET) {
		t.Errorf("read after heal = %v, want ECONNRESET (the CLOSED socket answers the probe)", resetErr)
	}
}

// TestDSTNetExitBacklogRSTHeldByCut: a closing listener resets its queued
// backlog connections — but that RST too is kernel-emitted: a blackhole cut
// of the listener→dialer direction swallows it, and the never-accepted
// dialer sees silence until the heal lets its outstanding bytes meet the
// CLOSED socket's RST.
func TestDSTNetExitBacklogRSTHeldByCut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var deadlineErr, resetErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		cut := make(chan struct{})
		simulation.Host("victim", simulation.HostConfig{}, func() {
			go simulation.Process("srv", func() {
				ln, err := Listen("tcp", ":0")
				if err != nil {
					panic(err)
				}
				_, p, _ := SplitHostPort(ln.Addr().String())
				port <- p
				<-cut
				ln.Close() // never accepted: the backlog conn's RST meets the cut
			})
		})
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			c, err := (&Dialer{KeepAlive: -1}).Dial("tcp", simulation.HostIP("victim")+":"+p)
			if err != nil {
				panic(err)
			}
			simulation.PartitionOneWay("victim", "survivor") // the RST's direction is cut
			close(cut)
			time.Sleep(10 * time.Millisecond) // the listener close runs behind the cut
			c.Write([]byte("probe"))          // destroyed unacknowledged at the frozen socket
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, deadlineErr = c.Read(make([]byte, 1))
			simulation.Heal("victim", "survivor")
			c.SetReadDeadline(time.Time{})
			_, resetErr = c.Read(make([]byte, 1))
			c.Close()
		})
	})
	if !errors.Is(deadlineErr, os.ErrDeadlineExceeded) {
		t.Errorf("read while the cut holds the backlog RST = %v, want the deadline", deadlineErr)
	}
	if !errors.Is(resetErr, syscall.ECONNRESET) {
		t.Errorf("read after heal = %v, want ECONNRESET (the CLOSED socket answers the probe)", resetErr)
	}
}

// TestDSTNetProcessCrashRSTHeldByCut: a process crash's RST is a
// kernel-emitted segment — a blackhole cut of its direction swallows it,
// and the survivor sees silence until the heal lets its probes meet the
// CLOSED socket's RST.
func TestDSTNetProcessCrashRSTHeldByCut(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var deadlineErr, resetErr error
	simulation.RunWith(1, simulation.Options{}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		defer close(done)
		dstCrashServer(port, done, "x")
		simulation.Host("survivor", simulation.HostConfig{}, func() {
			p := <-port
			c, err := (&Dialer{KeepAlive: -1}).Dial("tcp", simulation.HostIP("victim")+":"+p)
			if err != nil {
				panic(err)
			}
			buf := make([]byte, 1)
			if _, err := c.Read(buf); err != nil {
				panic(err)
			}
			simulation.PartitionOneWay("victim", "survivor") // the RST's direction is cut
			simulation.Crash("srv")
			c.Write([]byte("out")) // outbound is clear; the dead socket cannot answer through the cut
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, deadlineErr = c.Read(buf) // silence while the cut swallows the RST
			simulation.Heal("victim", "survivor")
			c.SetReadDeadline(time.Time{})
			_, resetErr = c.Read(buf) // the probe meets the CLOSED socket's RST
			c.Close()
		})
	})
	if !errors.Is(deadlineErr, os.ErrDeadlineExceeded) {
		t.Errorf("read while the cut holds the RST = %v, want the deadline (an RST cannot traverse a blackhole)", deadlineErr)
	}
	if !errors.Is(resetErr, syscall.ECONNRESET) {
		t.Errorf("read after heal = %v, want ECONNRESET (the CLOSED socket answers the probe)", resetErr)
	}
}
