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

// A dstWire is the latency-bearing transport under a simulated cross-host
// connection. It replaces net.Pipe (the synchronous, zero-latency transport used
// for same-host connections) when a connection has a non-zero base latency: a
// byte written at base-time T becomes readable at T+latency, in order, on the
// deterministic fake clock. It is the seam every network-delivery fault will hook
// (jitter varies the per-segment delay, throttle paces it by bytes, partition
// blackholes it) — base latency is its first, constant-delay policy.
//
// Two properties make it sound and replay-exact:
//
//   - In-order (DST-NET-FIFO): a segment's delivery time is clamped to be no
//     earlier than the previous segment's, so bytes are never reordered on a live
//     stream — the contract a reliable in-order transport (TCP) already enforces.
//   - Base-time gated (DST-NET-LATENCY-DET): delivery is measured in universe
//     base time (time.Now minus the reader/writer's host clock offset) and waited
//     out with a relative fake-clock timer, so a configured latency is the same
//     wire delay regardless of per-host clock skew, and the delivery instants are
//     a deterministic function of the seed.
//
// Unlike net.Pipe, a write is buffered (it returns immediately, modeling a TCP
// send buffer the propagation delay drains) rather than rendezvousing with the
// reader; that is exactly the decoupling latency requires. Same-host connections
// keep net.Pipe's synchronous behavior (latency 0 never constructs a dstWire), so
// the N=1 collapse and every test that sets no latency are byte-identical.

//go:linkname dstClockOffsetNow runtime.dstClockOffsetNow
func dstClockOffsetNow() int64

//go:linkname dstNetCrossHostLatencyNs runtime.dstNetCrossHostLatencyNs
func dstNetCrossHostLatencyNs() int64

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
// blocking (a send buffer); the reader gates on the head segment's deliverAt.
// deliverAt is computed under mu, so it is monotone non-decreasing in append
// (write) order — delivery is strictly in order, never reordering a live stream
// even under concurrent writers (DST-NET-FIFO).
type dstStream struct {
	mu     sync.Mutex
	segs   []dstSeg
	closed bool          // writer end closed: the reader drains, then sees EOF
	ready  chan struct{} // buffered(1) wakeup, pinged on append/close
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

// push appends a copy of b for delivery latencyNs base-time nanoseconds from now.
// Non-blocking (a send buffer). deliverAt is read under mu together with the
// append, so concurrent writers cannot interleave a lower deliverAt behind a
// higher one: append order equals base-time order equals delivery order (FIFO).
func (s *dstStream) push(b []byte, latencyNs int64) {
	data := append([]byte(nil), b...)
	s.mu.Lock()
	at := dstBaseNanos() + latencyNs
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
	out, in   *dstStream // out: this end writes (peer reads); in: this end reads (peer writes)
	latencyNs int64

	once       sync.Once
	localDone  chan struct{}
	remoteDone chan struct{} // the peer's localDone

	rdDead pipeDeadline
	wrDead pipeDeadline
}

func dstWirePair(latencyNs int64) (Conn, Conn) {
	ab, ba := newDstStream(), newDstStream()
	doneA, doneB := make(chan struct{}), make(chan struct{})
	a := &dstWireEnd{
		out: ab, in: ba, latencyNs: latencyNs,
		localDone: doneA, remoteDone: doneB,
		rdDead: makePipeDeadline(), wrDead: makePipeDeadline(),
	}
	b := &dstWireEnd{
		out: ba, in: ab, latencyNs: latencyNs,
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
	// Buffered: the bytes are queued for delivery latencyNs later and the call
	// returns immediately (a TCP send buffer the propagation delay drains). An
	// unbounded buffer never blocks, so a write deadline only gates entry — until
	// throttle adds a bounded buffer.
	e.out.push(b, e.latencyNs)
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
