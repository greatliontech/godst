// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	bigint "math/big"
	"testing"
)

// TestDSTWirePopRemainInFlightTail is the white-box M8-residual pin: pop must report
// remain=true when the segment left behind after filling b is IN FLIGHT (deliverAt >
// maxArrival), not only when arrived bytes remain — the read loop re-wakes on remain,
// and a second reader parked on the empty queue (no timer of its own) is stranded on
// that in-flight segment otherwise. Manipulates the stream directly (no bubble/clock)
// so the contract is pinned free of scheduler timing.
func TestDSTWirePopRemainInFlightTail(t *testing.T) {
	s := newDstStream(0)
	s.segs = []dstSeg{
		{data: []byte("0000"), deliverAt: 5},  // arrived at horizon 5
		{data: []byte("1111"), deliverAt: 10}, // in flight at horizon 5
	}
	buf := make([]byte, 4)
	n, remain, eof, wait := s.pop(buf, 5) // horizon 5: only seg0 arrived
	if n != 4 || string(buf) != "0000" {
		t.Fatalf("pop = %d %q, want 4 \"0000\"", n, buf[:n])
	}
	if eof || wait != 0 {
		t.Fatalf("pop eof=%v wait=%v, want false/0", eof, wait)
	}
	if !remain {
		t.Fatalf("pop remain=false with an in-flight tail — the re-wake would not fire and a second reader strands")
	}
}

// TestDSTWirePopRemainNothingLeft: pop reports remain=false when the queue is fully
// drained — the re-wake must NOT fire spuriously forever.
func TestDSTWirePopRemainNothingLeft(t *testing.T) {
	s := newDstStream(0)
	s.segs = []dstSeg{{data: []byte("00"), deliverAt: 5}}
	buf := make([]byte, 8)
	n, remain, _, _ := s.pop(buf, 5)
	if n != 2 {
		t.Fatalf("pop n=%d, want 2", n)
	}
	if remain {
		t.Fatalf("pop remain=true with an empty queue — spurious re-wake")
	}
}

// TestDSTWireFreezeAtHorizonFINArrival pins freezeAtHorizon's CLOSE_WAIT
// discriminant, both directions and each conjunct: the FIN counts as ARRIVED
// (finArrived=true — tcp_reset's CLOSE_WAIT arm, EPIPE identity) only when
// the stream is closed, closeAt is within the horizon, AND no segment was
// dropped (in order, a FIN cannot overtake data — a jitter draw can invert
// closeAt below an undelivered segment's deliverAt, and that FIN is still
// behind the data on the wire). Manipulates the stream directly so each
// conjunct is pinned free of scheduler timing.
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
		for _, seg := range tc.segs {
			s.buffered += int64(len(seg.data))
		}
		if got := s.freezeAtHorizon(tc.horizon); got != tc.want {
			t.Errorf("%s: freezeAtHorizon(%d) finArrived = %v, want %v", tc.name, tc.horizon, got, tc.want)
		}
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
