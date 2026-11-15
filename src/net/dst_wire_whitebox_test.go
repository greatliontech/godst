// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	bigint "math/big"
	"sync"
	"testing"
)

// TestDSTWirePopInFlightTailAndBroadcastWake pins popBytes's in-flight wait
// calculation and broadcast generation wakeups: draining an arrived prefix
// leaves an in-flight tail reporting its positive wait duration alongside a
// non-nil readyCh, and a subsequent stream wake closes that generation channel
// for every parked reader at once.
func TestDSTWirePopInFlightTailAndBroadcastWake(t *testing.T) {
	s := newDstStream(4)
	s.segs = []dstSeg{
		{data: []byte("0000"), deliverAt: 5},  // arrived at horizon 5
		{data: []byte("1111"), deliverAt: 10}, // in flight at horizon 5
	}
	s.buffered = 8
	spaceCh1 := s.spaceChLocked()
	spaceCh2 := s.spaceChLocked()
	if spaceCh1 != spaceCh2 {
		t.Fatalf("concurrent parked writers got distinct generation channels")
	}
	buf := make([]byte, 4)
	n, eof, wait := s.pop(buf, 5) // horizon 5: only seg0 arrived
	if n != 4 || string(buf) != "0000" {
		t.Fatalf("pop = %d %q, want 4 \"0000\"", n, buf[:n])
	}
	if eof || wait != 0 {
		t.Fatalf("pop eof=%v wait=%v, want false/0", eof, wait)
	}
	if !isClosedChan(spaceCh1) || !isClosedChan(spaceCh2) {
		t.Fatalf("pop did not broadcast space wake to all parked writers")
	}
	kbuf, readyCh, eof, wait := s.popBytes(4, 5)
	if len(kbuf) != 0 || eof || wait != 5 || readyCh == nil {
		t.Fatalf("popBytes on in-flight tail = (len=%d, readyCh=%v, eof=%v, wait=%v), want (0, non-nil, false, 5ns)", len(kbuf), readyCh != nil, eof, wait)
	}
	s.wake()
	if !isClosedChan(readyCh) {
		t.Fatalf("wake did not close parked readyCh generation")
	}
}

// TestDSTWirePopDrainsCompletely verifies pop drains a single arrived segment
// and reports wait < 0 (park until woken) once empty.
func TestDSTWirePopDrainsCompletely(t *testing.T) {
	s := newDstStream(0)
	s.segs = []dstSeg{{data: []byte("00"), deliverAt: 5}}
	s.buffered = 2
	buf := make([]byte, 8)
	n, eof, wait := s.pop(buf, 5)
	if n != 2 || eof || wait != 0 {
		t.Fatalf("pop = (%d, eof=%v, wait=%v), want (2, false, 0)", n, eof, wait)
	}
	n, eof, wait = s.pop(buf, 5)
	if n != 0 || eof || wait >= 0 {
		t.Fatalf("pop on empty unclosed stream = (%d, eof=%v, wait=%v), want (0, false, <0)", n, eof, wait)
	}
}

// TestDSTWireFreezeAtHorizonFINArrival pins freezeAtHorizon's CLOSE_WAIT
// discriminant, both directions and each conjunct: the FIN counts as ARRIVED
// (finArrived=true — tcp_reset's CLOSE_WAIT arm, EPIPE identity) only when
// the stream is closed, closeAt is within the horizon, AND no segment was
// dropped (in order, a FIN cannot overtake data — a jitter draw can invert
// closeAt below an undelivered segment's deliverAt, and that FIN is still
// behind the data on the wire). Manipulates the stream directly so each
// conjunct is pinned free of scheduler timing; the end-to-end CLOSE_WAIT paths
// are pinned by TestDSTNetResetInCloseWaitIsEPIPE (injected RST) and
// TestDSTNetHalfCloseCloseWaitHorizonDeath (retransmit-horizon death after a
// peer CloseWrite FIN arrives).
func TestDSTWireFreezeAtHorizonFINArrival(t *testing.T) {
	cases := []struct {
		name    string
		closed  bool
		closeAt int64
		segs    []dstSeg
		horizon int64
		want    bool
	}{
		{"fin-arrived-empty", true, 5, nil, 10, true},
		{"fin-arrived-behind-delivered-data", true, 6, []dstSeg{{data: []byte("aa"), deliverAt: 5}}, 10, true},
		{"fin-at-horizon-boundary", true, 10, nil, 10, true},                                           // closeAt == horizon is delivered (pop's inclusive rule)
		{"data-at-horizon-boundary", true, 10, []dstSeg{{data: []byte("a"), deliverAt: 10}}, 10, true}, // deliverAt == horizon is delivered too (the live-scan's inclusive rule)
		{"no-close", false, 0, nil, 10, false},
		{"fin-in-flight", true, 11, nil, 10, false},                                                        // the RST beat the FIN: ESTABLISHED arm
		{"fin-behind-undelivered-data", true, 5, []dstSeg{{data: []byte("aa"), deliverAt: 15}}, 10, false}, // jitter-inverted closeAt: the FIN is still behind the data
	}
	for _, tc := range cases {
		s := newDstStream(0)
		s.closed = tc.closed
		s.closeAt = tc.closeAt
		s.segs = append([]dstSeg(nil), tc.segs...)
		dropped := 0
		for _, seg := range tc.segs {
			s.buffered += int64(len(seg.data))
			if seg.deliverAt > tc.horizon {
				dropped++
			}
		}
		if got := s.freezeAtHorizon(tc.horizon); got != tc.want {
			t.Errorf("%s: freezeAtHorizon(%d) finArrived = %v, want %v", tc.name, tc.horizon, got, tc.want)
		}
		// A FIN that had NOT arrived at the horizon dies with the in-flight
		// bytes: the dead socket never receives it, so it must not stay
		// recorded (its closeAt passing later would resurrect a clean EOF).
		if s.closed != (tc.closed && tc.want) {
			t.Errorf("%s: closed after freeze = %v, want %v (an unarrived FIN dies with the socket)", tc.name, s.closed, tc.closed && tc.want)
		}
		// Anything the freeze destroyed — a segment or the FIN — is
		// permanently unacknowledged for the sender: deadDropped.
		wantDead := dropped > 0 || (tc.closed && !tc.want)
		if s.deadDropped != wantDead {
			t.Errorf("%s: deadDropped after freeze = %v, want %v", tc.name, s.deadDropped, wantDead)
		}
	}
}

// TestDSTWireFrozenStreamIsTerminal pins the dead-socket push contract at the
// stream level: a push after the freeze is discarded (dead=true) and marks
// deadDropped — the destroyed bytes count as held for the sender's own
// retransmit exhaustion (heldBeyond) even though no segment is queued — and a
// post-freeze closeWrite records no FIN (a CLOSED socket queues neither data
// nor a FIN, so neither can resurrect delivery or a clean EOF later).
func TestDSTWireFrozenStreamIsTerminal(t *testing.T) {
	s := newDstStream(0)
	s.segs = []dstSeg{{data: []byte("pre"), deliverAt: 5}}
	s.buffered = 3
	s.freezeAtHorizon(5)
	if s.deadDropped {
		t.Fatalf("deadDropped after a freeze that dropped nothing = true, want false")
	}
	if dead := s.pushLocked([]byte("post"), 0, 0, 0); !dead {
		t.Errorf("pushLocked on a frozen stream = dead=false, want true (the segment met a CLOSED socket)")
	}
	if len(s.segs) != 1 || string(s.segs[0].data) != "pre" || s.buffered != 3 {
		t.Errorf("frozen stream after a post-freeze push: segs=%d buffered=%d, want the pre-death prefix only (1 seg, 3 bytes)", len(s.segs), s.buffered)
	}
	if !s.deadDropped {
		t.Errorf("deadDropped after a discarded push = false, want true")
	}
	if !s.heldBeyond(100) {
		t.Errorf("heldBeyond after a discarded push = false, want true (the real counterpart segments are permanently unacknowledged)")
	}
	s.closeWrite(0, 0)
	if s.closed {
		t.Errorf("closeWrite on a frozen stream recorded the FIN — a CLOSED socket never queues it, and its closeAt passing would resurrect a clean EOF")
	}
	// The kept pre-death prefix stays drainable (tcp_recvmsg reports pending
	// data before the socket error).
	buf := make([]byte, 8)
	if n, eof, _ := s.pop(buf, 10); n != 3 || string(buf[:3]) != "pre" || eof {
		t.Errorf("pop on the frozen stream = (%d, %q, eof=%v), want (3, %q, false)", n, buf[:n], eof, "pre")
	}
}

// TestDSTTransmitNanosOverflowSafe pins the throttle transmit-time arithmetic against a
// math/big oracle, including the two int64-overflow regimes the guard exists for: a
// giant push (nbytes*1e9 wraps, split off via q) and a very high bandwidth (r*1e9 wraps,
// formed in 128 bits). A wrapped-negative transmit time would corrupt linkFreeAt and let
// delivery outrun B — the rate-bound (DST-NET-THROTTLE) failing silently.
func TestDSTTransmitNanosOverflowSafe(t *testing.T) {
	const maxI64 = int64(1<<63 - 1)
	cases := []struct{ nbytes, bps int64 }{
		{0, 100},                           // no bytes → 0
		{1, 1},                             // 1 s
		{4, 800},                           // 5 ms (the throttle tests' shape)
		{1, 1_000_000_000},                 // sub-ns → rounds up to 1 ns
		{10_000_000_000, 12_500_000_000},   // 100 Gbit/s, r large: r*1e9 overflows int64
		{9_999_999_999, 10_000_000_000},    // r just below bps, high bandwidth
		{20_000_000_000, 1_000_000_000},    // 20 GB push: nbytes*1e9 overflows, q carries it
		{maxI64 - 1, maxI64},               // r ≈ maxI64 (stresses the 128-bit high word < bps invariant)
		{1_000_000_000_000, 9_200_000_000}, // ~73 Gbit/s boundary, multi-second q
		{maxI64, 1},                        // mathematical duration exceeds int64 and must saturate
	}
	for _, c := range cases {
		got := dstTransmitNanos(c.nbytes, c.bps)
		// Oracle: ceil(nbytes*1e9/bps) in arbitrary precision.
		n := new(bigint.Int).Mul(bigint.NewInt(c.nbytes), bigint.NewInt(1_000_000_000))
		q, r := new(bigint.Int).QuoRem(n, bigint.NewInt(c.bps), new(bigint.Int))
		want := q
		if r.Sign() != 0 {
			want = new(bigint.Int).Add(q, bigint.NewInt(1))
		}
		wantI64 := maxI64
		if want.IsInt64() {
			wantI64 = want.Int64()
		}
		if got != wantI64 {
			t.Errorf("dstTransmitNanos(%d, %d) = %d, want %d (ceil, overflow-safe)", c.nbytes, c.bps, got, wantI64)
		}
		if c.nbytes > 0 && got < 1 {
			t.Errorf("dstTransmitNanos(%d, %d) = %d; a nonzero payload must cost ≥1 ns (never faster than B)", c.nbytes, c.bps, got)
		}
	}
}

func TestDSTDelayArithmeticSaturates(t *testing.T) {
	const maxI64 = int64(1<<63 - 1)
	for _, tc := range []struct {
		base, delay, want int64
	}{
		{0, 0, 0},
		{-1, 5, 5},
		{10, 20, 30},
		{maxI64 - 1, 1, maxI64},
		{maxI64 - 1, 2, maxI64},
		{maxI64, 1, maxI64},
	} {
		if got := dstDelayAdd(tc.base, tc.delay); got != tc.want {
			t.Errorf("dstDelayAdd(%d, %d) = %d, want %d", tc.base, tc.delay, got, tc.want)
		}

	}
	if got := dstDelayAdd(dstDelayAdd(maxI64-2, 1), 2); got != maxI64 {
		t.Errorf("three-term near-limit delay sum = %d, want %d", got, maxI64)
	}

	s := &dstStream{linkFreeAt: maxI64 - 1}
	s.pushLocked([]byte("x"), maxI64, 0, 1)
	if s.linkFreeAt != maxI64 || s.segs[0].deliverAt != maxI64 {
		t.Fatalf("saturated push: linkFreeAt=%d deliverAt=%d, want both %d", s.linkFreeAt, s.segs[0].deliverAt, maxI64)
	}
	s.closeWrite(maxI64, 0)
	if s.closeAt != maxI64 {
		t.Fatalf("saturated FIN deadline = %d, want %d", s.closeAt, maxI64)
	}
}

func TestDSTWireWriteConcurrentInjectRSTLeafLock(t *testing.T) {
	// 1. Direct leaf-lock check: holding e.out.mu and checking writeAbortedOutLocked
	// after injectRST or horizonDie must never re-enter e.out.mu or nest e.in.mu.
	for _, useHorizon := range []bool{false, true} {
		aConn, bConn := dstWirePair(0, 0, 1, 2, nil, nil)
		a := aConn.(*dstWireEnd)
		if useHorizon {
			a.horizonDie()
		} else {
			a.injectRST()
		}
		wrDeadCh := a.wrDead.wait()
		a.out.mu.Lock()
		aborted := a.writeAbortedOutLocked(wrDeadCh)
		a.out.mu.Unlock()
		if !aborted {
			t.Fatalf("useHorizon=%v: writeAbortedOutLocked = false, want true", useHorizon)
		}
		_ = aConn.Close()
		_ = bConn.Close()
	}

	// 2. Concurrent Write of a 2 MiB slice racing injectRST / horizonDie across
	// the outside-lock copy window must never self-deadlock on e.out.mu.
	bigBuf := make([]byte, 2<<20)
	for iter := range 64 {
		aConn, bConn := dstWirePair(0, 0, 1, 2, nil, nil)
		a := aConn.(*dstWireEnd)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = a.Write(bigBuf)
		}()
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				a.injectRST()
			} else {
				a.horizonDie()
			}
		}(iter)
		wg.Wait()
		_ = aConn.Close()
		_ = bConn.Close()
	}
}
