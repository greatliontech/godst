// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"testing"
	"testing/simulation"
	"time"
)

// These tests exercise the simulated network's base cross-host link latency (the
// fake-clock-gated delivery-queue wire, dst_wire.go) and the connection host
// attribution it keys on. The base latency is the substrate every later network
// delivery fault perturbs; here it is the constant per-link delay.
//
// Invariants enforced:
//   - cross-host delivery takes exactly the configured latency; same-host/loopback
//     is instant (the attribution-keyed link matrix);
//   - delivery is in order (DST-NET-FIFO);
//   - delivery virtual-times replay exactly (DST-NET-LATENCY-DET);
//   - a read deadline shorter than the latency times out (the wire honors the
//     fake clock — soundness);
//   - latency is base-time, so a per-host clock skew — or a mid-flight clock step —
//     does not change the wire delay.

// dstPingPong dials host "A" (the server) from host "B" (the client) with opts,
// exchanges one request/response, and returns the measured one-way client→server
// virtual delay and the full round-trip delay. Hosts A and B have no clock skew,
// so time.Now reads universe base time on both.
func dstPingPong(t *testing.T, seed uint64, opts simulation.Options) (oneWay, rtt time.Duration) {
	t.Helper()
	var writeAt, serverReadAt, clientRespAt time.Time
	simulation.RunWith(seed, opts, func() {
		port := make(chan string, 1)
		simulation.Host("A", simulation.HostConfig{}, func() { // server
			ln, err := Listen("tcp", ":0")
			if err != nil {
				panic(err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 16)
				n, _ := c.Read(buf)
				serverReadAt = time.Now()
				c.Write(append([]byte("re:"), buf[:n]...))
				c.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() { // client
			p := <-port
			c, err := Dial("tcp", simulation.HostIP("A")+":"+p)
			if err != nil {
				panic(err)
			}
			writeAt = time.Now()
			c.Write([]byte("ping"))
			buf := make([]byte, 16)
			c.Read(buf)
			clientRespAt = time.Now()
			c.Close()
		})
	})
	return serverReadAt.Sub(writeAt), clientRespAt.Sub(writeAt)
}

// TestDSTNetCrossHostLatency: a cross-host connection delivers each byte exactly
// CrossHostLatency later (one-way), so a request/response round-trip is twice that
// — measured on the deterministic fake clock.
func TestDSTNetCrossHostLatency(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const L = 50 * time.Millisecond
	oneWay, rtt := dstPingPong(t, 1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: L}})
	if oneWay != L {
		t.Errorf("cross-host one-way delay = %v, want %v", oneWay, L)
	}
	if rtt != 2*L {
		t.Errorf("cross-host round-trip delay = %v, want %v", rtt, 2*L)
	}
}

// TestDSTNetSameHostInstant: with the same non-zero latency configured, a
// same-host (loopback) connection is instant — the latency matrix is keyed by
// host pair, so co-located peers pay nothing (and stay on the synchronous pipe).
func TestDSTNetSameHostInstant(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	var writeAt, serverReadAt time.Time
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 50 * time.Millisecond}}, func() {
		simulation.Host("solo", simulation.HostConfig{}, func() {
			ln, err := Listen("tcp", ":0")
			if err != nil {
				panic(err)
			}
			_, p, _ := SplitHostPort(ln.Addr().String())
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 16)
				c.Read(buf)
				serverReadAt = time.Now()
				c.Close()
			}()
			c, err := Dial("tcp", "127.0.0.1:"+p) // loopback: same host
			if err != nil {
				panic(err)
			}
			writeAt = time.Now()
			c.Write([]byte("ping"))
			c.Close()
		})
	})
	if d := serverReadAt.Sub(writeAt); d != 0 {
		t.Errorf("same-host loopback delay = %v, want 0 (latency must not apply within a host)", d)
	}
}

// TestDSTNetLatencyFIFO: two writes separated by a 10ms gap arrive in order and
// each delayed by the link latency — delivery never reorders a live stream
// (DST-NET-FIFO), even though the second segment's delivery instant differs.
func TestDSTNetLatencyFIFO(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const L = 50 * time.Millisecond
	var got []string
	var firstAt, secondAt time.Duration
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: L}}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		var base time.Time
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, _ := ln.Accept()
				for i := 0; i < 2; i++ {
					buf := make([]byte, 16)
					n, err := c.Read(buf)
					if err != nil {
						break
					}
					got = append(got, string(buf[:n]))
					if i == 0 {
						firstAt = time.Since(base)
					} else {
						secondAt = time.Since(base)
					}
				}
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			base = time.Now()
			c.Write([]byte("first"))
			time.Sleep(10 * time.Millisecond)
			c.Write([]byte("second"))
			<-done
			c.Close()
		})
	})
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("delivery order = %v, want [first second]", got)
	}
	if firstAt != L {
		t.Errorf("first segment delivered at %v, want %v", firstAt, L)
	}
	if secondAt != 10*time.Millisecond+L {
		t.Errorf("second segment delivered at %v, want %v", secondAt, 10*time.Millisecond+L)
	}
}

// TestDSTNetLatencyDeterminism: the same seed replays the same delivery
// virtual-times (DST-NET-LATENCY-DET) — checked across a small seed sweep, two
// runs per seed.
func TestDSTNetLatencyDeterminism(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 37 * time.Millisecond}}
	for seed := uint64(0); seed < 16; seed++ {
		ow1, rtt1 := dstPingPong(t, seed, opts)
		ow2, rtt2 := dstPingPong(t, seed, opts)
		if ow1 != ow2 || rtt1 != rtt2 {
			t.Fatalf("seed %d: delivery timing not reproducible: one-way %v vs %v, rtt %v vs %v", seed, ow1, ow2, rtt1, rtt2)
		}
		if ow1 != 37*time.Millisecond {
			t.Fatalf("seed %d: one-way delay = %v, want 37ms", seed, ow1)
		}
	}
}

// TestDSTNetLatencySkewInvariant: the wire delay is universe BASE time, not
// host-skewed wall time — so a cross-host link between two hosts whose clocks
// disagree still delivers exactly CrossHostLatency of base time later (the
// property an HLC is tested against). Measured by converting each host's skewed
// wall reading back to base (subtracting its configured offset); the one-way base
// delay must be exactly L regardless of the skews, and the client's own-clock
// round trip is exactly 2L (its offset cancels between send and receive).
func TestDSTNetLatencySkewInvariant(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const (
		L    = 50 * time.Millisecond
		offA = 30 * time.Millisecond  // server host clock skew
		offB = -20 * time.Millisecond // client host clock skew
	)
	var writeWall, serverReadWall, clientRespWall time.Time
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: L}}, func() {
		port := make(chan string, 1)
		simulation.Host("A", simulation.HostConfig{Clock: simulation.Skew(offA)}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 16)
				c.Read(buf)
				serverReadWall = time.Now() // host A's skewed wall clock
				c.Write([]byte("pong"))
				c.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{Clock: simulation.Skew(offB)}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			writeWall = time.Now() // host B's skewed wall clock
			c.Write([]byte("ping"))
			buf := make([]byte, 16)
			c.Read(buf)
			clientRespWall = time.Now()
			c.Close()
		})
	})
	// Convert each host-skewed wall reading back to universe base time.
	oneWayBase := serverReadWall.Add(-offA).Sub(writeWall.Add(-offB))
	if oneWayBase != L {
		t.Errorf("base-time one-way delay under skew = %v, want %v (latency must be skew-invariant)", oneWayBase, L)
	}
	if rtt := clientRespWall.Sub(writeWall); rtt != 2*L {
		t.Errorf("client own-clock RTT under skew = %v, want %v (offset must cancel over a round trip)", rtt, 2*L)
	}
}

// TestDSTNetClockStepInvariantDelivery is the cross-axis (clock x net) check: the wire
// delivers in universe BASE time (dstBaseNanos = wall minus the host's clock offset)
// even when a host on the link has been stepped, so a clock step on a receiver does
// not change the wire delay. The receiver is stepped a large amount (>> L) BEFORE the
// send is gated, so the server evaluates its delivery gate with its clock already
// stepped — the discriminating case: a wall-based wire (one gating on time.Now without
// subtracting the offset) would see the receiver's clock already past the delivery
// instant and deliver ~immediately, whereas the base-time wire converts the stepped
// wall back to base and still waits exactly L. (A step injected AFTER the segment is
// already gated cannot discriminate this: the wire arms a fake-clock relative timer
// once and that timer is step-immune regardless, so the step must precede the gate.)
// Delivery latency is measured on the ROOT (host 0, never stepped, so its clock is
// base); the receiver's own wall reading confirms the step took effect (else a no-op
// StepClock would pass) — proving both that the step fired and that delivery stayed
// base-correct.
func TestDSTNetClockStepInvariantDelivery(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const (
		L       = 100 * time.Millisecond
		bigStep = 10 * time.Second // >> L; a wall-based wire would deliver ~instantly
	)
	var baseLatency time.Duration
	var got string
	var serverReadWall, startBase time.Time
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: L}}, func() {
		port := make(chan string, 1)
		accepted := make(chan struct{})
		doWrite := make(chan struct{})
		wrote := make(chan struct{})
		readDone := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() { // server (receiver)
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				close(accepted)
				buf := make([]byte, 16)
				n, _ := c.Read(buf) // gate evaluated post-step: must still wait L of base time
				serverReadWall = time.Now()
				got = string(buf[:n])
				c.Close()
				close(readDone)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() { // client (sender)
			go func() { // in a goroutine so Host("B") returns and the root can drive
				p := <-port
				c, err := Dial("tcp", simulation.HostIP("A")+":"+p)
				if err != nil {
					panic(err)
				}
				<-doWrite
				c.Write([]byte("ping")) // deliverAt = base now + L (B is unstepped)
				close(wrote)
				<-readDone // hold the conn open until the server has read
				c.Close()
			}()
		})
		<-accepted                         // the server is blocked in Read with no data yet
		simulation.StepClock("A", bigStep) // step the RECEIVER before the send is gated
		startBase = time.Now()             // root (host 0) = base, after the step
		close(doWrite)                     // let the client write; the server's gate eval is now post-step
		<-wrote
		<-readDone // the server unblocks after L of base time, not immediately
		baseLatency = time.Since(startBase)
	})
	if got != "ping" {
		t.Errorf("data delivered to a stepped receiver = %q, want \"ping\" (a step must not corrupt or drop bytes)", got)
	}
	if baseLatency != L {
		t.Errorf("base-time delivery latency with a +%v step on the receiver = %v, want %v (the wire is base-time; a clock step must not change the delay)", bigStep, baseLatency, L)
	}
	// The receiver's own (stepped) wall reading at delivery is base+L shifted by the
	// step: this confirms the step actually took effect on host A while the wire still
	// delivered in base time — a no-op step would land at base+L (no step term).
	if got := serverReadWall.Sub(startBase); got != L+bigStep {
		t.Errorf("receiver wall at delivery − base start = %v, want %v (L + step): the step must take effect on the receiver yet leave delivery base-correct", got, L+bigStep)
	}
}

// TestDSTNetLatencyDeadline: a read deadline shorter than the link latency times
// out before delivery (the wire blocks on the fake clock and honors deadlines);
// a deadline past the latency delivers. This is the soundness check that latency
// is a real fake-timer wait, not an unobservable instant transfer.
func TestDSTNetLatencyDeadline(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const L = 50 * time.Millisecond
	var shortErr error
	var longOK bool
	simulation.RunWith(1, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: L}}, func() {
		port := make(chan string, 1)
		srvDone := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				defer close(srvDone)
				c, err := ln.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 16)
				c.Read(buf)
				c.Write([]byte("pong")) // delivered to the client L later
				c.Read(buf)             // block until the client closes (EOF), then tear down
				c.Close()
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			c.Write([]byte("ping"))
			// A 20ms deadline expires before the 50ms one-way response arrives.
			c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			buf := make([]byte, 16)
			if _, err := c.Read(buf); err != nil {
				shortErr = err
			}
			// A generous deadline lets the (already in-flight) response arrive.
			c.SetReadDeadline(time.Now().Add(time.Second))
			if n, err := c.Read(buf); err == nil && string(buf[:n]) == "pong" {
				longOK = true
			}
			c.Close()
			<-srvDone // let the server goroutine finish (no dangling bubble goroutine)
		})
	})
	var ne Error
	if !errors.As(shortErr, &ne) || !ne.Timeout() || !errors.Is(shortErr, os.ErrDeadlineExceeded) {
		t.Errorf("short read deadline = %v, want timeout wrapping os.ErrDeadlineExceeded", shortErr)
	}
	if !longOK {
		t.Errorf("response not delivered after the latency elapsed with a generous deadline")
	}
}

// TestDSTNetJitterBounded: with CrossHostJitter set, a cross-host segment's
// one-way delay is the base latency plus a value in [0, jitter) — so it stays
// within [base, base+jitter) and varies across seeds (the jitter is actually
// drawn, not a constant).
func TestDSTNetJitterBounded(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const base = 10 * time.Millisecond
	const jit = 40 * time.Millisecond
	opts := simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: base, CrossHostJitter: jit}}
	seen := map[time.Duration]bool{}
	for seed := uint64(0); seed < 32; seed++ {
		oneWay, _ := dstPingPong(t, seed, opts)
		if oneWay < base || oneWay >= base+jit {
			t.Errorf("seed %d: one-way delay %v outside [%v, %v)", seed, oneWay, base, base+jit)
		}
		seen[oneWay] = true
	}
	if len(seen) < 2 {
		t.Errorf("jitter produced a single delay across 32 seeds; it must vary with the seed")
	}
}

// TestDSTNetJitterDeterminism: jittered delivery replays exactly for a given seed
// (DST-FAULT-REPLAY) and the seed steers it (the RTT is not constant across seeds).
func TestDSTNetJitterDeterminism(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	opts := simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 10 * time.Millisecond, CrossHostJitter: 40 * time.Millisecond}}
	distinct := map[time.Duration]bool{}
	for seed := uint64(0); seed < 24; seed++ {
		ow1, rtt1 := dstPingPong(t, seed, opts)
		ow2, rtt2 := dstPingPong(t, seed, opts)
		if ow1 != ow2 || rtt1 != rtt2 {
			t.Fatalf("seed %d: jittered delivery not reproducible: one-way %v vs %v, rtt %v vs %v", seed, ow1, ow2, rtt1, rtt2)
		}
		distinct[rtt1] = true
	}
	if len(distinct) < 2 {
		t.Errorf("jittered RTT identical across seeds; jitter must steer with the seed")
	}
}

// TestDSTNetJitterFIFO: a burst of back-to-back messages, each given an
// independent jitter draw, still arrives in order — head-of-line keeps delivery
// in write order even when a later segment draws a smaller delay (DST-NET-FIFO
// under jitter).
func TestDSTNetJitterFIFO(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const N = 12
	var got []string
	simulation.RunWith(3, simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 5 * time.Millisecond, CrossHostJitter: 50 * time.Millisecond}}, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, err := ln.Accept()
				if err != nil {
					close(done)
					return
				}
				r := bufio.NewReader(c)
				for i := 0; i < N; i++ {
					line, err := r.ReadString('\n')
					if err != nil {
						break
					}
					got = append(got, line[:len(line)-1])
				}
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			for i := 0; i < N; i++ {
				fmt.Fprintf(c, "msg%02d\n", i) // each Write is a separate segment with its own jitter draw
			}
			<-done
			c.Close()
		})
	})
	if len(got) != N {
		t.Fatalf("received %d/%d messages: %v", len(got), N, got)
	}
	for i := 0; i < N; i++ {
		if want := fmt.Sprintf("msg%02d", i); got[i] != want {
			t.Errorf("message %d = %q, want %q (jitter reordered the stream)", i, got[i], want)
		}
	}
}

// dstThrottledTransfer sends `total` bytes in `chunk`-sized writes from host B to
// host A over a connection configured by opts, and returns the base-time span
// from the receiver's first read to its last (when it has all `total` bytes) plus
// the byte count received. With a bandwidth limit the link transmits chunks
// serially, so this span reflects the enforced rate.
func dstThrottledTransfer(t *testing.T, seed uint64, opts simulation.Options, total, chunk int) (span time.Duration, got int) {
	t.Helper()
	simulation.RunWith(seed, opts, func() {
		port := make(chan string, 1)
		done := make(chan struct{})
		simulation.Host("A", simulation.HostConfig{}, func() {
			ln, _ := Listen("tcp", ":0")
			_, p, _ := SplitHostPort(ln.Addr().String())
			port <- p
			go func() {
				c, err := ln.Accept()
				if err != nil {
					close(done)
					return
				}
				buf := make([]byte, chunk)
				var firstAt time.Time
				for got < total {
					n, err := c.Read(buf)
					if n > 0 && firstAt.IsZero() {
						firstAt = time.Now()
					}
					got += n
					if err != nil {
						break
					}
				}
				span = time.Since(firstAt)
				c.Close()
				close(done)
			}()
		})
		simulation.Host("B", simulation.HostConfig{}, func() {
			p := <-port
			c, _ := Dial("tcp", simulation.HostIP("A")+":"+p)
			payload := make([]byte, chunk)
			for sent := 0; sent < total; sent += chunk {
				c.Write(payload)
			}
			<-done
			c.Close()
		})
	})
	return span, got
}

// TestDSTNetThrottleRate: a bandwidth-limited cross-host link delivers bytes no
// faster than the configured rate — the receiver's first-to-last read span is at
// least (total-chunk)/bandwidth of base time (the propagation latency cancels
// between the first and last read). DST-NET-THROTTLE.
func TestDSTNetThrottleRate(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const total = 1 << 20  // 1 MiB
	const chunk = 32 << 10 // 32 KiB
	const bw = 10 << 20    // 10 MiB/s
	const lat = 5 * time.Millisecond
	opts := simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: lat, CrossHostBandwidth: bw}}
	span, got := dstThrottledTransfer(t, 1, opts, total, chunk)
	if got != total {
		t.Fatalf("received %d/%d bytes", got, total)
	}
	// Lower bound is the invariant (DST-NET-THROTTLE: no faster than B). The upper
	// bound is a sanity that throttle adds no gross spurious delay, with a slack of
	// one chunk's transmit time (the natural granularity) so it does not couple to
	// the exact store-and-forward arithmetic.
	minSpan := time.Duration((int64(total) - int64(chunk)) * int64(time.Second) / int64(bw))
	chunkXmit := time.Duration((int64(chunk)*int64(time.Second) + int64(bw) - 1) / int64(bw))
	if span < minSpan {
		t.Errorf("%d bytes at %d B/s: first-to-last read span %v, want >= %v (rate not limited)", total, bw, span, minSpan)
	}
	if span > minSpan+chunkXmit+time.Millisecond {
		t.Errorf("transfer span %v exceeds %v (excess delay beyond the transmission time)", span, minSpan+chunkXmit+time.Millisecond)
	}
}

// TestDSTNetThrottleDeterminism: throttle is deterministic (no random draw) — the
// same seed replays the same paced transfer (DST-FAULT-REPLAY). Throttle is
// intentionally seed-INDEPENDENT, so the analytic span assertion (not just
// run-to-run equality) guards against the transfer being a no-op or unpaced.
func TestDSTNetThrottleDeterminism(t *testing.T) {
	if !dstNetEnabled {
		t.Skip("requires -tags dst")
	}
	const total, chunk, bw = 256 << 10, 16 << 10, 4 << 20
	opts := simulation.Options{Network: simulation.NetworkConfig{CrossHostLatency: 3 * time.Millisecond, CrossHostBandwidth: bw}}
	wantSpan := time.Duration((int64(total) - int64(chunk)) * int64(time.Second) / int64(bw)) // (total-chunk)/B, exact here
	for seed := uint64(0); seed < 8; seed++ {
		s1, g1 := dstThrottledTransfer(t, seed, opts, total, chunk)
		s2, g2 := dstThrottledTransfer(t, seed, opts, total, chunk)
		if s1 != s2 || g1 != g2 {
			t.Fatalf("seed %d: throttled transfer not reproducible: span %v vs %v, got %d vs %d", seed, s1, s2, g1, g2)
		}
		if g1 != total || s1 != wantSpan {
			t.Fatalf("seed %d: got %d bytes in span %v, want %d in %v", seed, g1, s1, total, wantSpan)
		}
	}
}
