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

// A dstWire is the delay-bearing transport under a simulated connection. It backs
// EVERY connection — cross-host with the configured latency/jitter/bandwidth,
// same-host and loopback with a zero-latency wire (never partitioned): a segment is
// transmitted (bandwidth, store-and-forward), then propagates (latency + jitter),
// becoming readable in order on the deterministic fake clock. It is the seam every network-delivery fault hooks —
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
// reader; that is exactly the decoupling latency and buffer-and-recover require,
// and it is why same-host conns are wire-backed too: a rendezvous pipe deadlocks
// two co-located peers that each write before reading, an execution real TCP cannot
// produce. Reads are a byte STREAM: one Read returns whatever arrived bytes fit,
// coalescing across write boundaries (TCP gives no framing), never message-framed.

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
	closeAt    int64         // base-time the writer closed (the FIN's arrival); valid iff closed
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

// closeWrite marks the writer end gracefully closed at the current base time (the
// FIN's arrival): the reader drains queued segments at their delivery times, then
// sees EOF — but a partition holds the FIN too, so EOF is withheld until heal unless
// the close arrived before the cut (closeAt <= cut-start), exactly like a data byte.
func (s *dstStream) closeWrite() {
	s.mu.Lock()
	s.closed = true
	s.closeAt = dstBaseNanos()
	s.mu.Unlock()
	s.wake()
}

// pop copies bytes that have ARRIVED (deliverAt <= maxArrival) into b, coalescing
// across segment boundaries exactly as a TCP receive buffer does — one Read returns
// whatever contiguous arrived bytes fit, spanning as many writes as fit in b (write
// boundaries are not preserved; TCP does not preserve them). maxArrival is the
// caller's arrival horizon: base-time now normally, or the partition cut-start while
// cut (so only bytes that reached the receive buffer before the cut are readable).
//
// Returns: n>0 with remain=true when ANY segment is still queued after filling b —
// arrived (b filled first) OR in-flight. The caller re-wakes on remain so a second
// blocked reader is signaled; the cap-1 ready channel cannot hold a second ping, and
// a reader parked on an empty queue has no timer of its own, so re-waking on an
// in-flight remainder too (not just arrived bytes) is what lets it arm a timer for
// that segment instead of stranding. eof=true when the writer's close has arrived
// (closeAt <= maxArrival) and no segments remain; otherwise wait is the base-time
// duration until the head segment arrives (>0), or wait<0 when nothing is pending
// within the horizon (block until woken).
func (s *dstStream) pop(b []byte, maxArrival int64) (n int, remain, eof bool, wait time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.segs) > 0 && n < len(b) {
		head := &s.segs[0]
		if head.deliverAt > maxArrival {
			// Head not yet arrived within the horizon. If we already copied some,
			// return it (a segment remains → remain=true); else report the wait until
			// this segment arrives (only meaningful when maxArrival is base-time now —
			// a partition caps the horizon at cut-start, and the caller blocks on the
			// heal instead).
			if n > 0 {
				return n, true, false, 0
			}
			return 0, false, false, time.Duration(head.deliverAt - maxArrival)
		}
		c := copy(b[n:], head.data)
		n += c
		if c < len(head.data) {
			head.data = head.data[c:]
			return n, true, false, 0 // b full mid-segment, bytes remain: signal another reader
		}
		s.segs[0] = dstSeg{} // release the delivered buffer (don't retain it in the backing array)
		s.segs = s.segs[1:]
		if len(s.segs) == 0 {
			s.segs = nil
		}
	}
	if n > 0 {
		// Re-signal iff any segment remains — arrived (b filled at a boundary) or
		// in-flight. A woken second reader either grabs arrived bytes (re-waking
		// again) or arms a timer for the in-flight head; either terminates. A reader
		// that pops nothing does not re-wake, so there is no spin.
		return n, len(s.segs) > 0, false, 0
	}
	if s.closed && s.closeAt <= maxArrival && len(s.segs) == 0 {
		return 0, false, true, 0
	}
	return 0, false, false, -1
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
	if len(b) == 0 {
		return 0, nil
	}
	for {
		switch {
		case isClosedChan(e.localDone):
			return 0, io.ErrClosedPipe
		case isClosedChan(e.rdDead.wait()):
			return 0, os.ErrDeadlineExceeded
		}
		// Fetch the partition wake channel BEFORE reading the cut state so a heal
		// racing the check still wakes us. Partition holds only bytes NOT yet in the
		// receiver's buffer at the cut: the arrival horizon is base-time now normally,
		// capped at the cut-start while cut, so bytes that arrived before the cut stay
		// readable (they sit in the kernel receive buffer — blackholing them would be
		// a sim-only failure), while in-flight and after-cut bytes are held until heal.
		// The peer's writes keep buffering on the wire (push is unaffected) and flush
		// in order once the link heals — the sound buffer-and-recover model.
		wake := dstPartWakeCh()
		maxArrival := dstBaseNanos()
		cutStart, cut := dstPartCutStart(e.localHost, e.peerHost)
		if cut {
			// A byte counts as arrived-before-the-cut only if it was delivered
			// STRICTLY before the cut began: cutStart-1 is the inclusive horizon.
			// The boundary is load-bearing — a write issued right after Partition()
			// with no virtual time in between has deliverAt == cutStart, and it must
			// be HELD (it was sent after the cut), not delivered. Held bytes flush on
			// heal, when cut is false and the horizon returns to now.
			if cutStart-1 < maxArrival {
				maxArrival = cutStart - 1
			}
		}
		n, remain, eof, wait := e.in.pop(b, maxArrival)
		if n > 0 {
			if remain {
				e.in.wake() // a segment remains; re-signal so a second blocked reader wakes
			}
			return n, nil
		}
		if eof {
			return 0, io.EOF
		}
		if cut {
			// Arrived-before-cut bytes exhausted; anything else (in flight, written
			// after the cut, or a not-yet-arrived FIN) is held. Block until heal, a
			// deadline, or a local close.
			select {
			case <-wake:
			case <-e.localDone:
			case <-e.rdDead.wait():
			}
			continue
		}
		// Block until data arrives, the head segment becomes deliverable, a
		// deadline fires, or this end closes; then re-evaluate.
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
