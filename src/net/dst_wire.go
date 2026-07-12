// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"io"
	"math/bits"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	_ "unsafe" // for go:linkname
)

//go:linkname dstNetSendBufferBytes runtime.dstNetSendBufferBytes
func dstNetSendBufferBytes() int64

//go:linkname dstNetRetransmitTimeoutNs runtime.dstNetRetransmitTimeoutNs
func dstNetRetransmitTimeoutNs() int64

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
	ready      chan struct{} // buffered(1) wakeup, pinged on append/close (wakes the READER)
	space      chan struct{} // buffered(1) wakeup, pinged on pop (wakes a blocked WRITER)
	buffered   int64         // bytes written but not yet consumed by the reader — the send-buffer occupancy
	capacity   int64         // send-buffer capacity in bytes; 0 = unbounded (a write never blocks)
}

func newDstStream(capacity int64) *dstStream {
	return &dstStream{
		ready:    make(chan struct{}, 1),
		space:    make(chan struct{}, 1),
		capacity: capacity,
	}
}

// wake signals a (possibly) blocked reader without blocking the signaler.
func (s *dstStream) wake() {
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// wakeWriter signals a (possibly) blocked writer that send-buffer space freed.
func (s *dstStream) wakeWriter() {
	select {
	case s.space <- struct{}{}:
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
	s.mu.Lock()
	s.pushLocked(b, latencyNs, jitterNs, bandwidthBps)
	s.mu.Unlock()
	s.wake()
}

// pushLocked is push's body with the caller holding s.mu (and not signaling the reader —
// the caller does). The bounded-buffer write path uses it to check capacity and append
// atomically under one lock hold.
func (s *dstStream) pushLocked(b []byte, latencyNs, jitterNs, bandwidthBps int64) {
	data := append([]byte(nil), b...)
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
		transmitEnd += dstTransmitNanos(int64(len(b)), bandwidthBps)
		s.linkFreeAt = transmitEnd
	}
	at := transmitEnd + latencyNs + dstFaultRandN(jitterNs)
	s.segs = append(s.segs, dstSeg{data: data, deliverAt: at})
	s.buffered += int64(len(data))
}

// dstTransmitNanos returns ceil(nbytes * 1e9 / bps) — the base-time a bandwidth-limited
// link occupies transmitting nbytes at bps bytes/sec — overflow-safe for any in-spec
// nbytes and bps (a wrapped negative transmit time would corrupt linkFreeAt and break
// the rate bound). Two int64 overflows are avoided: nbytes*1e9 wraps for nbytes ≳ 9.2 GB
// (reachable with an unbounded send buffer and a giant Write), so the whole seconds q
// are split off and only the remainder r<bps carries the ×1e9; and r*1e9 itself wraps
// for bps ≳ 9.2e9 B/s (~73 Gbit/s, reached by a high-bandwidth link when r is large), so
// r*1e9 is formed in 128 bits and ceil-divided by bps (the fractional quotient is <1e9,
// so it fits, and the 128-bit high word is ≪ bps, so bits.Div64 never overflows).
func dstTransmitNanos(nbytes, bps int64) int64 {
	q := nbytes / bps
	r := nbytes % bps
	hi, lo := bits.Mul64(uint64(r), 1_000_000_000)
	lo, carry := bits.Add64(lo, uint64(bps)-1, 0) // + (bps-1) before the divide = ceil
	hi += carry
	frac, _ := bits.Div64(hi, lo, uint64(bps))
	return q*1_000_000_000 + int64(frac)
}

// closeWrite marks the writer end gracefully closed at the current base time (the
// FIN's arrival): the reader drains queued segments at their delivery times, then
// sees EOF — but a partition holds the FIN too, so EOF is withheld until heal unless
// the close arrived before the cut (closeAt <= cut-start), exactly like a data byte.
func (s *dstStream) closeWrite(latencyNs, jitterNs int64) {
	s.mu.Lock()
	s.closed = true
	s.closeAt = dstBaseNanos() + latencyNs + dstFaultRandN(jitterNs)
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
		s.buffered -= int64(c) // consumed by the reader: frees the peer's send-buffer space (unread for capacity==0/unbounded, where the writer never consults it)
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
		terminal := s.closed && s.closeAt <= maxArrival && len(s.segs) == 0
		return n, len(s.segs) > 0 || terminal, false, 0
	}
	if s.closed && s.closeAt <= maxArrival && len(s.segs) == 0 {
		return 0, false, true, 0
	}
	if s.closed && len(s.segs) == 0 && s.closeAt > maxArrival {
		return 0, false, false, time.Duration(s.closeAt - maxArrival)
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

	retransNs int64       // send-into-a-dead-peer retransmit horizon (0 = none)
	timedOut  atomic.Bool // the retransmit horizon fired: this end is dead (ETIMEDOUT)

	// The retransmit-exhaustion watchdog for this end's OUTGOING direction: armed
	// whenever undeliverable bytes are observed under a cut (any write into the
	// cut, or a blocked read holding dying outbound bytes), so a small write into
	// a permanent partition kills the conn at the horizon even though the write
	// itself returned instantly (buffered, as TCP's async send does) — the error
	// then surfaces on the blocked or any subsequent operation, never
	// succeeds-and-forgets. Disarmed by a heal that delivers the bytes; a
	// heal-then-recut restarts the window (the heal-resets precedent, erring
	// toward fewer/later ETIMEDOUTs). horizonKill closes when the watchdog kills
	// the end, waking blocked reads/writes to observe timedOut.
	horizonMu     sync.Mutex
	horizonArmed  bool
	horizonAnchor int64 // base-time the current undeliverable episode was first observed
	horizonOnce   sync.Once
	horizonKill   chan struct{}

	once       sync.Once
	localDone  chan struct{}
	remoteDone chan struct{} // the peer's localDone

	rdDead pipeDeadline
	wrDead pipeDeadline
}

// dstWirePair builds the two ends of a connection between dialerHost
// (the a/dialer end) and listenHost (the b/server end). Each direction gets a send
// buffer of capacity bytes (0 = unbounded) and the retransmit horizon retransNs.
func dstWirePair(latencyNs, jitterNs, bandwidthBps, capacity, retransNs int64, dialerHost, listenHost uint32) (Conn, Conn) {
	ab, ba := newDstStream(capacity), newDstStream(capacity)
	doneA, doneB := make(chan struct{}), make(chan struct{})
	a := &dstWireEnd{
		out: ab, in: ba, latencyNs: latencyNs, jitterNs: jitterNs, bandwidthBps: bandwidthBps, retransNs: retransNs,
		localHost: dialerHost, peerHost: listenHost,
		localDone: doneA, remoteDone: doneB, horizonKill: make(chan struct{}),
		rdDead: makePipeDeadline(), wrDead: makePipeDeadline(),
	}
	b := &dstWireEnd{
		out: ba, in: ab, latencyNs: latencyNs, jitterNs: jitterNs, bandwidthBps: bandwidthBps, retransNs: retransNs,
		localHost: listenHost, peerHost: dialerHost,
		localDone: doneB, remoteDone: doneA, horizonKill: make(chan struct{}),
		rdDead: makePipeDeadline(), wrDead: makePipeDeadline(),
	}
	return a, b
}

func (*dstWireEnd) LocalAddr() Addr  { return pipeAddr{} }
func (*dstWireEnd) RemoteAddr() Addr { return pipeAddr{} }

// heldBeyond reports whether the stream still queues anything a cut beginning at
// cutStart holds: a segment (or the FIN) whose delivery lies at or after the cut
// (the same arrived-strictly-before-the-cut boundary pop uses). These are the
// bytes a real sender would be retransmitting into the void.
func (s *dstStream) heldBeyond(cutStart int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.segs {
		if s.segs[i].deliverAt > cutStart-1 {
			return true
		}
	}
	return s.closed && s.closeAt > cutStart-1
}

// armHorizon starts the outgoing retransmit-exhaustion watchdog if it is not
// already running. Callers observe undeliverable outbound bytes under a cut; the
// anchor is always an OBSERVATION time — set here at arming, and re-set by
// horizonCheck when a heal-then-recut starts a new episode — never earlier than
// the real first retransmission, so the sim errs toward later timeouts (the
// sound direction).
func (e *dstWireEnd) armHorizon() {
	if e.retransNs <= 0 || e.timedOut.Load() {
		return
	}
	e.horizonMu.Lock()
	if e.horizonArmed {
		e.horizonMu.Unlock()
		return
	}
	e.horizonArmed = true
	e.horizonAnchor = dstBaseNanos()
	e.horizonMu.Unlock()
	time.AfterFunc(time.Duration(e.retransNs), e.horizonCheck)
}

// horizonCheck runs at the watchdog's deadline: disarm if the episode ended (heal
// delivered the bytes, or the end closed), extend if a heal-then-recut restarted
// the window, otherwise kill this end — timedOut, wake every blocked operation.
func (e *dstWireEnd) horizonCheck() {
	if e.timedOut.Load() || isClosedChan(e.localDone) {
		e.horizonMu.Lock()
		e.horizonArmed = false
		e.horizonMu.Unlock()
		return
	}
	cutStart, cut, _ := dstPartCutStartDir(e.localHost, e.peerHost)
	if !cut || !e.out.heldBeyond(cutStart) {
		e.horizonMu.Lock()
		e.horizonArmed = false
		e.horizonMu.Unlock()
		return
	}
	e.horizonMu.Lock()
	anchor := e.horizonAnchor
	if cutStart > anchor {
		// Healed and re-cut since arming: this is a NEW undeliverable episode,
		// and this check is its first observation — re-anchor at NOW, never at
		// the cut's start. The cut may predate the episode's bytes by
		// arbitrarily long (written at cut+Δ while the stale watchdog was
		// still pending), and a cut-start anchor would kill them before their
		// own horizon elapsed — a premature, sim-only ETIMEDOUT. Now is always
		// ≥ any held byte's write time, so the window errs later (the
		// heal-resets precedent's sound direction).
		anchor = dstBaseNanos()
		e.horizonAnchor = anchor
	}
	e.horizonMu.Unlock()
	if remaining := e.retransNs - (dstBaseNanos() - anchor); remaining > 0 {
		time.AfterFunc(time.Duration(remaining), e.horizonCheck)
		return
	}
	e.horizonMu.Lock()
	e.horizonArmed = false // no AfterFunc pending past the kill
	e.horizonMu.Unlock()
	e.timedOut.Store(true)
	e.horizonOnce.Do(func() { close(e.horizonKill) })
	e.out.wake()
	e.out.wakeWriter()
	e.in.wake()
}

// unreadInbound reports whether this end's receive direction still holds bytes
// the end never consumed — the close(2)-sends-RST predicate: a socket closed
// with a non-empty receive queue answers the peer with RST, not FIN (delivered
// and still-in-flight bytes alike; data arriving after the close elicits the
// same RST).
func (e *dstWireEnd) unreadInbound() bool {
	e.in.mu.Lock()
	defer e.in.mu.Unlock()
	return e.in.buffered > 0
}

func (e *dstWireEnd) Read(b []byte) (int, error) {
	n, err := e.read(b)
	if err != nil && err != io.EOF && err != io.ErrClosedPipe && err != syscall.ETIMEDOUT {
		err = &OpError{Op: "read", Net: "pipe", Err: err}
	}
	// io.EOF / io.ErrClosedPipe / syscall.ETIMEDOUT pass raw for the dstConn wrapper to
	// map to production identity (a pipe-OpError here would double-wrap and leak "pipe").
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
		// Reads receive data flowing peer→local, so the INCOMING direction governs the
		// hold: a one-directional cut of peer→local blocks these reads while local→peer
		// (this end's writes) still flows.
		cutStart, cut, _ := dstPartCutStartDir(e.peerHost, e.localHost)
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
			e.in.wakeWriter() // freed send-buffer space: wake the peer's blocked writer
			if remain {
				e.in.wake() // a segment remains; re-signal so a second blocked reader wakes
			}
			return n, nil
		}
		if eof {
			e.in.wake() // EOF is persistent: hand the condition to another blocked reader.
			return 0, io.EOF
		}
		// A retransmit-horizon death surfaces only after deliverable data and a
		// delivered FIN (tcp_recvmsg reports pending data, then the socket
		// error) — so a killed end still drains what the network carried.
		if e.timedOut.Load() {
			return 0, syscall.ETIMEDOUT
		}
		// A blocked read observing dying OUTBOUND bytes arms the watchdog — the
		// spec's "blocked operation" surfacing: write-then-read into a permanent
		// cut must fail at the horizon, not hang forever. Checked before EITHER
		// block below: under a ONE-WAY outbound cut the inbound direction is
		// live (cut=false), yet this end's held bytes still die — a real
		// sender's ACKs never return through the cut. The any-cut gate keeps
		// the common no-partition block lock-free.
		if dstPartAnyCut() {
			if outCutStart, outCut, _ := dstPartCutStartDir(e.localHost, e.peerHost); outCut && e.out.heldBeyond(outCutStart) {
				e.armHorizon()
			}
		}
		if cut {
			// Arrived-before-cut bytes exhausted; anything else (in flight, written
			// after the cut, or a not-yet-arrived FIN) is held. Block until heal, a
			// deadline, a local close, or the outbound retransmit horizon killing
			// this end.
			select {
			case <-wake:
			case <-e.horizonKill:
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
		case <-wake:
			// A partition change while parked on a live link: re-evaluate, so a
			// cut landing AFTER this read blocked still arms the outbound
			// watchdog (the hoisted check above) instead of stranding the
			// reader past the horizon. The wake channel is remade per change —
			// one wakeup per fault op, no spin.
		case <-e.horizonKill:
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
	if err != nil && err != io.ErrClosedPipe && err != syscall.ETIMEDOUT {
		err = &OpError{Op: "write", Net: "pipe", Err: err}
	}
	// io.ErrClosedPipe / syscall.ETIMEDOUT pass raw for the dstConn wrapper to map (a
	// pipe-OpError here would double-wrap and leak "pipe" into the error identity).
	return n, err
}

func (e *dstWireEnd) write(b []byte) (int, error) {
	switch {
	case e.timedOut.Load():
		return 0, syscall.ETIMEDOUT
	case isClosedChan(e.localDone):
		return 0, io.ErrClosedPipe
	case isClosedChan(e.remoteDone):
		return 0, io.ErrClosedPipe
	case isClosedChan(e.wrDead.wait()):
		return 0, os.ErrDeadlineExceeded
	}
	// Bounded send buffer with backpressure: the bytes queue for delivery
	// (transmission + latency + jitter) into a send buffer of capacity e.out.capacity
	// bytes. A write that would overrun the buffer BLOCKS until the reader drains
	// enough (the peer consuming bytes frees space, wakeWriter), so a program cannot
	// outrun a slow peer with unbounded buffering. capacity 0 = unbounded
	// (SendBuffer<0, user-chosen): the fast path, a write never blocks.
	//
	// The retransmit horizon fires ONLY while the link is PARTITIONED — bytes held at
	// a cut are genuinely undeliverable, so a permanent cut kills the conn ETIMEDOUT
	// (kernel retransmit exhaustion, the sound direction: a real deadline-less write
	// into a permanent partition also fails in bounded time). A full buffer behind a
	// LIVE peer that is merely slow or has stopped reading is TCP zero-window persist,
	// NOT retransmit exhaustion: the write blocks with no horizon and resumes when the
	// peer drains — firing ETIMEDOUT there would be a sim-only failure a live peer
	// cannot produce (the false-positive class Soundness forbids).
	total := 0
	cutStart := int64(-1) // base-time the current partition-block began; -1 = not counting
	for len(b) > 0 {
		e.out.mu.Lock()
		for e.out.capacity > 0 && e.out.buffered >= e.out.capacity {
			e.out.mu.Unlock()
			// Fetch the partition wake channel before reading the cut state, so a cut
			// that begins (or heals) while we block still re-evaluates the horizon.
			wake := dstPartWakeCh()
			var horizonC <-chan time.Time
			var horizonT *time.Timer
			if e.retransNs > 0 && dstPartitionedDir(e.localHost, e.peerHost) { // outgoing local→peer is where a write's bytes are held
				if cutStart < 0 {
					cutStart = dstBaseNanos() // the cut-block began; a heal resets it, restarting the timer on ACK progress
				}
				// The window is a base-time delta (skew-invariant); the timer fires on
				// the writer's host clock, so under a DriftClock rate change "retransNs
				// of base time" shifts slightly — deterministic, and faithful to a real
				// retransmit timer running on the sender's own clock.
				remaining := e.retransNs - (dstBaseNanos() - cutStart)
				if remaining <= 0 {
					e.timedOut.Store(true)
					return total, syscall.ETIMEDOUT
				}
				horizonT = time.NewTimer(time.Duration(remaining))
				horizonC = horizonT.C
			} else {
				cutStart = -1 // live peer (or no horizon): persist, reset the cut window
			}
			select {
			case <-e.out.space:
			case <-wake: // partition began or healed: re-evaluate the horizon
			case <-e.horizonKill:
				return total, syscall.ETIMEDOUT
			case <-horizonC:
				e.timedOut.Store(true)
				return total, syscall.ETIMEDOUT
			case <-e.wrDead.wait():
				if horizonT != nil {
					horizonT.Stop()
				}
				return total, os.ErrDeadlineExceeded
			case <-e.localDone:
				if horizonT != nil {
					horizonT.Stop()
				}
				return total, io.ErrClosedPipe
			case <-e.remoteDone:
				if horizonT != nil {
					horizonT.Stop()
				}
				return total, io.ErrClosedPipe
			}
			if horizonT != nil {
				horizonT.Stop()
			}
			e.out.mu.Lock()
		}
		room := int64(len(b))
		if e.out.capacity > 0 {
			if avail := e.out.capacity - e.out.buffered; avail < room {
				room = avail
			}
		}
		e.out.pushLocked(b[:room], e.latencyNs, e.jitterNs, e.bandwidthBps)
		roomRemains := e.out.capacity > 0 && e.out.buffered < e.out.capacity
		e.out.mu.Unlock()
		e.out.wake()
		if roomRemains {
			// Chain-wake another writer blocked on this direction: one drain frees one
			// cap-1 space token, so a woken writer that leaves room must pass the baton —
			// mirroring the reader's `remain` re-signal (pop → wake). Without it,
			// concurrent writers strand with buffer space free (a lost wakeup / hang).
			e.out.wakeWriter()
		}
		total += int(room)
		b = b[room:]
		cutStart = -1 // progress: reset the cut window
		if dstPartAnyCut() && dstPartitionedDir(e.localHost, e.peerHost) {
			// The bytes just buffered are undeliverable: a real sender's
			// retransmissions into the void exhaust at the horizon even though
			// this write returned (TCP's async send). Arm the watchdog so the
			// death surfaces on the blocked or a subsequent operation — a small
			// write into a permanent cut never succeeds-and-forgets. The cheap
			// any-cut gate keeps the un-partitioned fast path map-lookup-free.
			e.armHorizon()
		}
	}
	return total, nil
}

func (e *dstWireEnd) Close() error {
	e.once.Do(func() {
		close(e.localDone)
		// Stop accepting reads/writes locally (localDone) and let the peer drain
		// what we already sent, then see EOF (closeWrite on our out = peer's in).
		e.out.closeWrite(e.latencyNs, e.jitterNs)
	})
	return nil
}

func (e *dstWireEnd) SetDeadline(t time.Time) error {
	if isClosedChan(e.localDone) {
		return io.ErrClosedPipe
	}
	e.rdDead.set(t)
	e.wrDead.set(t)
	return nil
}

func (e *dstWireEnd) SetReadDeadline(t time.Time) error {
	if isClosedChan(e.localDone) {
		return io.ErrClosedPipe
	}
	e.rdDead.set(t)
	return nil
}

func (e *dstWireEnd) SetWriteDeadline(t time.Time) error {
	if isClosedChan(e.localDone) {
		return io.ErrClosedPipe
	}
	e.wrDead.set(t)
	return nil
}
