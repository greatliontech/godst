// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import "testing"

// TestDSTWirePopRemainInFlightTail is the white-box M8-residual pin: pop must report
// remain=true when the segment left behind after filling b is IN FLIGHT (deliverAt >
// maxArrival), not only when arrived bytes remain — the read loop re-wakes on remain,
// and a second reader parked on the empty queue (no timer of its own) is stranded on
// that in-flight segment otherwise. Manipulates the stream directly (no bubble/clock)
// so the contract is pinned free of scheduler timing.
func TestDSTWirePopRemainInFlightTail(t *testing.T) {
	s := newDstStream()
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
	s := newDstStream()
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
