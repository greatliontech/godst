// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"io"
	"os"
	"sync"
	"time"
	_ "unsafe" // for go:linkname
)

// A dstWire is the delay-bearing transport under a simulated cross-host
// connection. It backs EVERY cross-host connection (same-host stays on the
// synchronous, zero-delay net.Pipe): a segment is transmitted (bandwidth, store-
// and-forward), then propagates (latency + jitter), becoming readable in order on
// the deterministic fake clock. It is the seam every network-delivery fault hooks —
// jitter draws the per-segment delay, throttle paces it by bytes, partition
// blackholes its reads while writes keep buffering. With no fault configured the
// delay is zero, so it is a buffered-but-instant link (the faithful TCP send-buffer
// shape, and what partition's buffer-and-recover needs).
//
// Two properties make it sound and replay-exact:
//
//   - In-order (DST-NET-FIFO): the reader consumes the head (oldest) segment
//     first, so delivery is in append (write) order whatever each segment's
//     delivery time — bytes are never reordered on a live stream, the contract a
//     reliable in-order transport (TCP) already enforces. No deliverAt clamp is
//     needed: a later segment with a smaller delay is bunched behind the head
//     (head-of-line), never overtaking it.
//   - Base-time gated (DST-NET-LATENCY-DET): delivery is measured in universe
//     base time (time.Now minus the reader/writer's host clock offset) and waited
//     out with a relative fake-clock timer, so a configured delay is the same wire
//     delay regardless of per-host clock skew, and the delivery instants are a
//     deterministic function of the seed — the jitter draw comes from the
//     dedicated, stream-isolated fault RNG, so it replays too (DST-FAULT-REPLAY).
//
// Unlike net.Pipe, a write is buffered (it returns immediately, modeling a TCP
// send buffer the propagation delay drains) rather than rendezvousing with the
// reader; that is exactly the decoupling latency and buffer-and-recover require.
// Same-host connections keep net.Pipe's synchronous behavior (a dstWire is built
// only for cross-host conns), so the N=1 collapse — which has no cross-host conns —
// is byte-identical.

//go:linkname dstClockOffsetNow runtime.dstClockOffsetNow
func dstClockOffsetNow() int64

//go:linkname dstNetCrossHostLatencyNs runtime.dstNetCrossHostLatencyNs
func dstNetCrossHostLatencyNs() int64

//go:linkname dstNetCrossHostJitterNs runtime.dstNetCrossHostJitterNs
func dstNetCrossHostJitterNs() int64

//go:linkname dstNetCrossHostBandwidthBps runtime.dstNetCrossHostBandwidthBps
func dstNetCrossHostBandwidthBps() int64

// dstFaultRandN draws a deterministic value in [0,n) from the dedicated, seeded,
// stream-isolated fault RNG (n<=0 draws nothing) — the source for the per-segment
// jitter, so injected jitter replays exactly and never perturbs the schedule.
//
//go:linkname dstFaultRandN runtime.dstFaultRandN
func dstFaultRandN(n int64) int64

// dstBaseNanos is the current universe BASE virtual time in nanoseconds: the
// calling goroutine's host wall clock (time.Now) minus its host clock offset.
// Delivery is gated in base time so a configured latency is the same wire delay
// on every host even when hosts disagree on wall time (a skewed clock shifts
// time.Now but never a duration — the per-host clock contract).
func dstBaseNanos() int64 {
	return time.Now().UnixNano() - dstClockOffsetNow()
}

// dstSeg is a contiguous run of bytes queued on one direction of a wire, readable
// once the base-time clock reaches deliverAt.
type dstSeg struct {
	data      []byte
	deliverAt int64 // universe base-time nanoseconds
}

// dstStream is one direction of a dstWire: a FIFO byte queue whose segments
// become readable at their base-time deliverAt. The writer appends without
// blocking (a send buffer); the reader always consumes the head (oldest) segment
// first, so delivery is strictly in append (write) order whatever the per-segment
// deliverAt is — a jitter draw varies only WHEN a segment is released (and, via
// head-of-line, bunches the ones behind it), never the ORDER. So a live stream is
// never reordered (DST-NET-FIFO) without any need to clamp deliverAt.
type dstStream struct {
	mu         sync.Mutex
	segs       []dstSeg
	linkFreeAt int64         // base-time when the bandwidth-limited link finishes transmitting all queued bytes
	closed     bool          // writer end closed: the reader drains, then sees EOF
	ready      chan struct{} // buffered(1) wakeup, pinged on append/close
}

func newDstStream() *dstStream {
	return &dstStream{ready: make(chan struct{}, 1)}
}

// wake signals a (possibly) blocked reader without blocking the signaler.
func (s *dstStream) wake() {
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// push appends a copy of b for delivery from now: a bandwidth-limited link
// transmits the segment (it occupies the link len(b)/bandwidth before propagating,
// serialized after earlier queued bytes via linkFreeAt), then the base latency and
// a jitter draw in [0,jitterNs) apply. Non-blocking (a send buffer). FIFO order
// needs no clamp: the reader consumes the head first, so a later segment (even one
// with a smaller jitter draw) is never delivered before an earlier one —
// head-of-line bunches it instead. bandwidthBps<=0 is unlimited; jitterNs<=0 draws
// nothing (so an inactive jitter fault leaves the fault stream untouched).
func (s *dstStream) push(b []byte, latencyNs, jitterNs, bandwidthBps int64) {
	data := append([]byte(nil), b...)
	s.mu.Lock()
	transmitEnd := dstBaseNanos()
	if bandwidthBps > 0 {
		// Serialize transmission at bandwidthBps bytes/sec: this segment starts when
		// the link is free (or now, whichever is later) and occupies it for
		// len/bandwidth, store-and-forward (delivered whole at the transmit end). The
		// transmit time is rounded UP, so even a sub-nanosecond segment costs ≥1ns —
		// delivery is never faster than B (the sound direction; the round-up adds at
		// most 1ns per segment).
		if s.linkFreeAt > transmitEnd {
			transmitEnd = s.linkFreeAt
		}
		transmitEnd += (int64(len(b))*1_000_000_000 + bandwidthBps - 1) / bandwidthBps
		s.linkFreeAt = transmitEnd
	}
	at := transmitEnd + latencyNs + dstFaultRandN(jitterNs)
	s.segs = append(s.segs, dstSeg{data: data, deliverAt: at})
	s.mu.Unlock()
	s.wake()
}

// closeWrite marks the writer end gracefully closed: the reader drains queued
// segments at their delivery times, then sees EOF.
func (s *dstStream) closeWrite() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.wake()
}

// pop copies deliverable bytes into b. It returns n>0 when bytes are ready;
// eof=true when the writer closed and the queue is drained; otherwise wait is the
// duration until the head segment becomes deliverable, or wait<0 when the queue
// is empty and open (block until woken).
func (s *dstStream) pop(b []byte) (n int, eof bool, wait time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.segs) > 0 {
		head := &s.segs[0]
		now := dstBaseNanos()
		if head.deliverAt > now {
			return 0, false, time.Duration(head.deliverAt - now)
		}
		n = copy(b, head.data)
		if n < len(head.data) {
			head.data = head.data[n:]
			return n, false, 0
		}
		s.segs[0] = dstSeg{} // release the delivered buffer (don't retain it in the backing array)
		s.segs = s.segs[1:]
		if len(s.segs) == 0 {
			s.segs = nil
		}
		return n, false, 0
	}
	if s.closed {
		return 0, true, 0
	}
	return 0, false, -1
}

// dstWireEnd is one endpoint of a dstWire; it implements net.Conn with the same
// raw error vocabulary as net.Pipe (io.EOF on a drained peer close,
// io.ErrClosedPipe on a local close or a peer gone, os.ErrDeadlineExceeded on a
// deadline), so the dstConn wrapper maps it to production error identity
// unchanged.
type dstWireEnd struct {
	out, in             *dstStream // out: this end writes (peer reads); in: this end reads (peer writes)
	latencyNs           int64
	jitterNs            int64  // per-segment delivery jitter is drawn from [0,jitterNs)
	bandwidthBps        int64  // link transmit rate in bytes/sec; 0 = unlimited
	localHost, peerHost uint32 // this end's host and the peer's, for partition targeting

	once       sync.Once
	localDone  chan struct{}
	remoteDone chan struct{} // the peer's localDone

	rdDead pipeDeadline
	wrDead pipeDeadline
}

// dstWirePair builds the two ends of a cross-host connection between dialerHost
// (the a/dialer end) and listenHost (the b/server end).
func dstWirePair(latencyNs, jitterNs, bandwidthBps int64, dialerHost, listenHost uint32) (Conn, Conn) {
	ab, ba := newDstStream(), newDstStream()
	doneA, doneB := make(chan struct{}), make(chan struct{})
	a := &dstWireEnd{
		out: ab, in: ba, latencyNs: latencyNs, jitterNs: jitterNs, bandwidthBps: bandwidthBps,
		localHost: dialerHost, peerHost: listenHost,
		localDone: doneA, remoteDone: doneB,
		rdDead: makePipeDeadline(), wrDead: makePipeDeadline(),
	}
	b := &dstWireEnd{
		out: ba, in: ab, latencyNs: latencyNs, jitterNs: jitterNs, bandwidthBps: bandwidthBps,
		localHost: listenHost, peerHost: dialerHost,
		localDone: doneB, remoteDone: doneA,
		rdDead: makePipeDeadline(), wrDead: makePipeDeadline(),
	}
	return a, b
}

func (*dstWireEnd) LocalAddr() Addr  { return pipeAddr{} }
func (*dstWireEnd) RemoteAddr() Addr { return pipeAddr{} }

func (e *dstWireEnd) Read(b []byte) (int, error) {
	n, err := e.read(b)
	if err != nil && err != io.EOF && err != io.ErrClosedPipe {
		err = &OpError{Op: "read", Net: "pipe", Err: err}
	}
	return n, err
}

func (e *dstWireEnd) read(b []byte) (int, error) {
	for {
		switch {
		case isClosedChan(e.localDone):
			return 0, io.ErrClosedPipe
		case isClosedChan(e.rdDead.wait()):
			return 0, os.ErrDeadlineExceeded
		}
		// Partition blackhole: while the link is cut, deliver nothing — block until
		// it heals, the read deadline fires, or this end closes. The peer's writes
		// keep buffering on the wire (push is unaffected) and flush in order once
		// the reader resumes here, so a healed stream loses no bytes and never
		// reorders (the sound buffer-and-recover model). Fetch the wake channel
		// before the check so a heal racing the check still wakes us.
		wake := dstPartWakeCh()
		if dstPartitioned(e.localHost, e.peerHost) {
			select {
			case <-wake:
			case <-e.localDone:
			case <-e.rdDead.wait():
			}
			continue
		}
		n, eof, wait := e.in.pop(b)
		if n > 0 {
			return n, nil
		}
		if eof {
			return 0, io.EOF
		}
		// Block until data arrives, the head segment becomes deliverable, a
		// deadline fires, or either end closes; then re-evaluate.
		var timerC <-chan time.Time
		var timer *time.Timer
		if wait > 0 {
			timer = time.NewTimer(wait)
			timerC = timer.C
		}
		// Note: the peer closing is NOT a wake case here. A graceful peer close
		// leaves already-written bytes that must still drain at their delivery
		// times; closeWrite pings in.ready and sets in.closed, so the reader wakes,
		// delivers the remainder on the clock, then sees EOF via pop. Waking on the
		// peer's done channel instead would spin (it stays ready) before the buffered
		// segments' delivery timers fire.
		select {
		case <-e.in.ready:
		case <-timerC:
		case <-e.localDone:
		case <-e.rdDead.wait():
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (e *dstWireEnd) Write(b []byte) (int, error) {
	n, err := e.write(b)
	if err != nil && err != io.ErrClosedPipe {
		err = &OpError{Op: "write", Net: "pipe", Err: err}
	}
	return n, err
}

func (e *dstWireEnd) write(b []byte) (int, error) {
	switch {
	case isClosedChan(e.localDone):
		return 0, io.ErrClosedPipe
	case isClosedChan(e.remoteDone):
		return 0, io.ErrClosedPipe
	case isClosedChan(e.wrDead.wait()):
		return 0, os.ErrDeadlineExceeded
	}
	// Buffered: the bytes are queued for delivery (transmission + latency + jitter)
	// and the call returns immediately (a TCP send buffer the propagation delay
	// drains). The buffer is unbounded so a write never blocks — even under
	// throttle, which paces delivery, not the sender; a write deadline therefore
	// only gates entry. Sender backpressure (a bounded send buffer whose fill blocks
	// Write) is a deferred refinement.
	e.out.push(b, e.latencyNs, e.jitterNs, e.bandwidthBps)
	return len(b), nil
}

func (e *dstWireEnd) Close() error {
	e.once.Do(func() {
		close(e.localDone)
		// Stop accepting reads/writes locally (localDone) and let the peer drain
		// what we already sent, then see EOF (closeWrite on our out = peer's in).
		e.out.closeWrite()
	})
	return nil
}

func (e *dstWireEnd) SetDeadline(t time.Time) error {
	if isClosedChan(e.localDone) || isClosedChan(e.remoteDone) {
		return io.ErrClosedPipe
	}
	e.rdDead.set(t)
	e.wrDead.set(t)
	return nil
}

func (e *dstWireEnd) SetReadDeadline(t time.Time) error {
	if isClosedChan(e.localDone) || isClosedChan(e.remoteDone) {
		return io.ErrClosedPipe
	}
	e.rdDead.set(t)
	return nil
}

func (e *dstWireEnd) SetWriteDeadline(t time.Time) error {
	if isClosedChan(e.localDone) || isClosedChan(e.remoteDone) {
		return io.ErrClosedPipe
	}
	e.wrDead.set(t)
	return nil
}
