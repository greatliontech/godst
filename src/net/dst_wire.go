// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"io"
	"math"
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

//go:linkname dstNewBaseTimer time.newDSTBaseTimer
func dstNewBaseTimer(d time.Duration) *time.Timer

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
	frozen     bool          // the receiving socket died (injected RST or retransmit-horizon death): later pushes are discarded (freezeAtHorizon)
	lastArrive int64         // latest deliverAt ever queued — the last instant this direction carried a segment (keepalive's activity signal)

	// deadDropped records that bytes (or a FIN) committed to this stream were
	// destroyed by the receiver's death — dropped by freezeAtHorizon or
	// discarded by a post-freeze push. The real sender's counterpart segments
	// are permanently unacknowledged (a dead socket ACKs nothing), so its
	// retransmissions into a cut exhaust: heldBeyond keeps reporting them held
	// even though the segments themselves are gone from the queue.
	deadDropped bool
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

// pushLocked appends a copy of b for delivery from now, with the caller
// holding s.mu (and signaling the reader itself): a bandwidth-limited link
// transmits the segment (it occupies the link len(b)/bandwidth before
// propagating, serialized after earlier queued bytes via linkFreeAt), then
// the base latency and a jitter draw in [0,jitterNs) apply. Non-blocking (a
// send buffer). FIFO order needs no clamp: the reader consumes the head
// first, so a later segment (even one with a smaller jitter draw) is never
// delivered before an earlier one — head-of-line bunches it instead. The
// write path holds s.mu across its capacity check and this append, one
// atomic unit. bandwidthBps<=0 is unlimited; jitterNs<=0 draws nothing (so
// an inactive jitter fault leaves the fault stream untouched). Returns
// dead=true when the segment was discarded because the receiving socket is
// gone (frozen) — the caller surfaces the sender-side consequence.
func (s *dstStream) pushLocked(b []byte, latencyNs, jitterNs, bandwidthBps int64) (dead bool) {
	if s.frozen {
		// The receiving socket died (an injected RST or a retransmit-horizon
		// death — a CLOSED socket): a late segment is answered with an RST,
		// never queued. The local send still succeeds first, as a real send()
		// into a doomed conn does; the sender-side failure is the caller's
		// (write's dead-push handling, or the fault loop's own injection).
		s.deadDropped = true
		return true
	}
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
		transmitEnd = dstDelayAdd(transmitEnd, dstTransmitNanos(int64(len(b)), bandwidthBps))
		s.linkFreeAt = transmitEnd
	}
	at := dstDelayAdd(dstDelayAdd(transmitEnd, latencyNs), dstFaultRandN(jitterNs))
	s.segs = append(s.segs, dstSeg{data: data, deliverAt: at})
	s.buffered += int64(len(data))
	if at > s.lastArrive {
		s.lastArrive = at
	}
	return false
}

// arrivedThrough reports the latest segment-arrival instant no later than
// horizon (the caller's cut-capped arrival horizon), and whether any queued
// segment (or destroyed bytes, deadDropped) is still undelivered at that
// horizon — the keepalive prober's two inputs: activity and
// data-outstanding. lastArrive is a running max, so under a cut the capped
// value can sit at the horizon rather than the true last pre-cut arrival —
// at or after it, never before — which only defers a probe (the errs-later,
// sound direction).
func (s *dstStream) arrivedThrough(horizon int64) (lastArrive int64, outstanding bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.lastArrive
	if last > horizon {
		last = horizon
	}
	if s.deadDropped {
		return last, true
	}
	for i := range s.segs {
		if s.segs[i].deliverAt > horizon {
			return last, true
		}
	}
	return last, false
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
	if q > math.MaxInt64/1_000_000_000 {
		return math.MaxInt64
	}
	return dstDelayAdd(q*1_000_000_000, int64(frac))
}

// dstDelayAdd saturates nonnegative duration and absolute-time composition at
// the latest representable timer deadline. Network delay inputs are
// nonnegative; treating a negative value as zero keeps this internal boundary
// monotone if a caller violates that precondition.
func dstDelayAdd(base, delay int64) int64 {
	if base < 0 {
		base = 0
	}
	if delay <= 0 {
		return base
	}
	if base >= math.MaxInt64-delay {
		return math.MaxInt64
	}
	return base + delay
}

func dstLinkDelay(latencyNs, jitterNs int64) int64 {
	return dstDelayAdd(latencyNs, dstFaultRandN(jitterNs))
}

// closeWrite marks the writer end gracefully closed at the current base time (the
// FIN's arrival): the reader drains queued segments at their delivery times, then
// sees EOF — but a partition holds the FIN too, so EOF is withheld until heal unless
// the close arrived before the cut (closeAt <= cut-start), exactly like a data byte.
// A FIN toward a frozen stream is discarded like any late segment (the receiving
// socket is CLOSED; it answers with RST, it never queues) — recording it would
// resurrect a clean EOF on a dead end.
func (s *dstStream) closeWrite(latencyNs, jitterNs int64) {
	s.mu.Lock()
	if s.frozen {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.closeAt = dstDelayAdd(dstDelayAdd(dstBaseNanos(), latencyNs), dstFaultRandN(jitterNs))
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

	retransNs int64       // send-into-a-dead-peer retransmit horizon (0 = none; TCP_USER_TIMEOUT overrides per socket — effRetransNs)
	timedOut  atomic.Bool // the retransmit horizon fired: this end is dead (ETIMEDOUT)

	// opts is this end's socket-option state (the dialing socket's for the
	// dialer end, the accept-inherited clone for the server end); bornAt the
	// establishment instant. The keepalive prober (kaCheck) reads opts live
	// at every decision, so a post-establishment option write takes effect on
	// the next probe, as the kernel's timer reads its socket fields.
	opts   *dstSockOpts
	bornAt int64

	// The keepalive prober's chain state — the armHorizon/horizonCheck
	// AfterFunc-chain pattern: kaArmed guards a single pending timer;
	// kaEpisode is the base-time anchor of the current unanswered-probe
	// episode (-1 = none: the socket is idle-waiting or its probes are being
	// answered); kaAckAt the last answered-probe instant (an activity
	// signal, as the probe's ACK resets the kernel's idle clock).
	kaMu        sync.Mutex
	kaArmed     bool
	kaEpisode   int64
	kaAckAt     int64
	kaProbesOut int64 // probes sent unanswered this episode — the kernel's icsk_probes_out

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

	// An RST arrived at this end (fault matcher, crash teardown, or the RST a
	// dead peer socket answers a live segment with — dstDeadPushRST): reads
	// drain the already-DELIVERED bytes first, then fail; writes
	// fail immediately. rstKill closes when the RST lands, waking blocked
	// operations to observe rstArrived (the horizonKill pattern). See
	// injectRST for the kernel contract; the receive queue itself is FROZEN
	// by the injection (dstStream.frozen), so a peer end whose own
	// injection the teardown loop has not reached yet cannot slip bytes
	// into this queue after the RST — a real CLOSED socket answers such a
	// late segment with an RST of its own, never queues it.
	rstArrived atomic.Bool
	rstOnce    sync.Once
	rstKill    chan struct{}

	// rstCloseWait records that the injected RST arrived with the peer's FIN
	// already delivered — production CLOSE_WAIT, where tcp_reset pends EPIPE
	// instead of ECONNRESET (host-probed: post-RST reads are plain EOF and
	// writes EPIPE, with no ECONNRESET arm). Written once inside rstOnce;
	// consumed by the dstConn wrapper's consumeSkErr.
	rstCloseWait atomic.Bool

	once       sync.Once
	localDone  chan struct{}
	remoteDone chan struct{} // the peer's localDone

	rdDead pipeDeadline
	wrDead pipeDeadline
}

// dstWirePair builds the two ends of a connection between dialerHost
// (the a/dialer end) and listenHost (the b/server end). Each direction gets a send
// buffer of capacity bytes (0 = unbounded) and the retransmit horizon retransNs.
// dialerOpts/serverOpts are the per-end socket-option states; registering each
// end's keepalive kick (and arming the prober when keepalive is already
// enabled) happens here, at establishment — the kernel's keepalive timer arms
// when the connection does.
func dstWirePair(latencyNs, jitterNs, bandwidthBps, capacity, retransNs int64, dialerHost, listenHost uint32, dialerOpts, serverOpts *dstSockOpts) (Conn, Conn) {
	ab, ba := newDstStream(capacity), newDstStream(capacity)
	doneA, doneB := make(chan struct{}), make(chan struct{})
	born := dstBaseNanos()
	a := &dstWireEnd{
		out: ab, in: ba, latencyNs: latencyNs, jitterNs: jitterNs, bandwidthBps: bandwidthBps, retransNs: retransNs,
		localHost: dialerHost, peerHost: listenHost,
		opts: dialerOpts, bornAt: born, kaEpisode: -1,
		localDone: doneA, remoteDone: doneB, horizonKill: make(chan struct{}), rstKill: make(chan struct{}),
		rdDead: makePipeDeadline(), wrDead: makePipeDeadline(),
	}
	b := &dstWireEnd{
		out: ba, in: ab, latencyNs: latencyNs, jitterNs: jitterNs, bandwidthBps: bandwidthBps, retransNs: retransNs,
		localHost: listenHost, peerHost: dialerHost,
		opts: serverOpts, bornAt: born, kaEpisode: -1,
		localDone: doneB, remoteDone: doneA, horizonKill: make(chan struct{}), rstKill: make(chan struct{}),
		rdDead: makePipeDeadline(), wrDead: makePipeDeadline(),
	}
	if dialerOpts != nil {
		dialerOpts.setKick(a.kaPoke)
	}
	if serverOpts != nil {
		serverOpts.setKick(b.kaPoke)
	}
	return a, b
}

// userTimeoutNs is this end's TCP_USER_TIMEOUT (0 = unset; nil opts — a
// bare test-constructed wire — has none).
func (e *dstWireEnd) userTimeoutNs() int64 {
	if e.opts == nil {
		return 0
	}
	return e.opts.userTimeoutNs()
}

// effRetransNs is this end's effective retransmission horizon: the socket's
// TCP_USER_TIMEOUT when set (tcp(7): the maximum time transmitted data may
// remain unacknowledged before the connection is forcibly closed — it
// applies even when the run's horizon is disabled), else the run's
// RetransmitTimeout.
func (e *dstWireEnd) effRetransNs() int64 {
	if e.opts != nil {
		if uto := e.opts.userTimeoutNs(); uto > 0 {
			return uto
		}
	}
	return e.retransNs
}

// The keepalive law — tcp_keepalive_timer's death, modeled as an
// OP-ARMED, EPISODE-BOUNDED watchdog (the armHorizon pattern), never a
// standing timer: a free-running per-conn probe chain would keep the
// bubble's virtual clock advancing forever and destroy quiescence-based
// deadlock detection (a sim-only liveness break). The watchdog arms when a
// BLOCKED operation observes the unanswerable-probe state — the link cut in
// either direction (the probe cannot reach, or its ACK cannot return), or
// the peer's host DEAD (a crashed machine answers nothing) — while no
// outbound data is outstanding (with data in flight the RETRANSMIT
// machinery governs, as production's keepalive timer is not armed while the
// retransmit timer is). The connection dies ETIMEDOUT — tcp_write_err's
// sk_err, the same one-shot identity ladder as retransmission exhaustion
// (horizonDie) — when the episode has lasted the full probe schedule:
// remaining idle time plus keepIntvl×keepCnt (TCP_USER_TIMEOUT, when set,
// overrides the probing budget, tcp(7)). A heal before the deadline
// disarms; the episode anchor is the blocked op's first observation — an
// observation time, never the cut's start — so the sim errs toward later
// deaths (the armHorizon precedent's sound direction).
//
// Recorded completeness limit (⊆-real, the safe direction): a connection NO
// operation observes during a cut-then-heal window misses the death a real
// kernel's free-running prober would have delivered — the same class as the
// recorded flow-level ACK-starvation limit. And a probe meeting a REBOOTED
// peer host (its sockets gone) is treated as answered rather than RST'd
// (production's ECONNRESET): the conn survives where production kills it —
// missed-fault direction again, never a false failure.
//
// Determinism: fake-clock AfterFuncs reading partition/host state at fire
// time; no randomness, no scheduling choices beyond the timers themselves.

// kaActivity is the last-activity instant keepalive idles from: the latest
// of establishment, the last segment arrival either direction carried
// (arrival ≈ its ACK), and the last answered probe — each arrival capped at
// its direction's cut-start (a capped value can sit at the cap rather than
// the true pre-cut arrival: at or after it, never before, which only defers
// the probe — errs later). Also reports whether outbound data is
// outstanding at the same horizon.
func (e *dstWireEnd) kaActivity(now int64) (act int64, outstanding bool) {
	inHorizon := now
	if cs, cut, _ := dstPartCutStartDir(e.peerHost, e.localHost); cut && cs-1 < inHorizon {
		inHorizon = cs - 1
	}
	outHorizon := now
	if cs, cut, _ := dstPartCutStartDir(e.localHost, e.peerHost); cut && cs-1 < outHorizon {
		outHorizon = cs - 1
	}
	inLast, _ := e.in.arrivedThrough(inHorizon)
	outLast, out := e.out.arrivedThrough(outHorizon)
	act = e.bornAt
	if inLast > act {
		act = inLast
	}
	if outLast > act {
		act = outLast
	}
	return act, out
}

// kaUnanswerable reports whether a keepalive probe sent now could not be
// answered: a cut in either direction, or the peer's host dead.
func (e *dstWireEnd) kaUnanswerable() bool {
	return dstPartitionedDir(e.localHost, e.peerHost) ||
		dstPartitionedDir(e.peerHost, e.localHost) ||
		dstHostDead(e.peerHost)
}

// armKeepalive starts the keepalive prober if a blocked operation should:
// keepalive enabled, probes unanswerable now, no outbound data outstanding.
// Idempotent while armed; cheap early-outs keep the block paths' loops
// unburdened. The first fire lands at the first probe instant — the idle
// time from the last activity, or the arming observation itself if idle had
// already expired (the errs-later anchor: an unobserved conn's probes
// before the arm are not presumed to have failed).
func (e *dstWireEnd) armKeepalive() {
	if e.opts == nil || e.timedOut.Load() || e.rstArrived.Load() {
		return
	}
	enabled, idleNs, _, _, _ := e.opts.kaParams()
	if !enabled || !e.kaUnanswerable() {
		return
	}
	now := dstBaseNanos()
	act, outstanding := e.kaActivity(now)
	if outstanding {
		return // the retransmit horizon owns the death
	}
	e.kaMu.Lock()
	if e.kaArmed {
		e.kaMu.Unlock()
		return
	}
	e.kaArmed = true
	if e.kaEpisode < 0 {
		e.kaEpisode = now
	}
	if e.kaAckAt > act {
		act = e.kaAckAt
	}
	due := dstDelayAdd(act, idleNs)
	if e.kaEpisode > due {
		due = e.kaEpisode
	}
	e.kaMu.Unlock()
	remaining := due - now
	if remaining < 1 {
		remaining = 1
	}
	time.AfterFunc(time.Duration(remaining), e.kaCheck)
}

// kaCheck is one keepalive-timer fire — tcp_keepalive_timer's own per-fire
// algorithm, run on the probe grid (the first fire at idle-from-activity,
// then every interval), so the death instant is grid-exact, never earlier
// than a real kernel's:
//
//   - The link answerable (no cut, host up): the pending probe is ACKed —
//     the failure state resets and the prober DISARMS (op-armed design: no
//     standing chain on a live link; a blocked op re-arms when it next
//     observes an unanswerable state). A probe meeting the peer's dead
//     SOCKET over the live link is answered with RST instead
//     (probeDeadPeer).
//   - Unanswerable: the kill check runs BEFORE this fire's probe would be
//     sent, exactly as the kernel's — with TCP_USER_TIMEOUT set, kill when
//     the time since last activity has reached it AND at least one probe is
//     already out (the user timeout replaces the count check, tcp(7));
//     without it, kill when the probes already out have reached the count.
//     Otherwise send the probe (count it) and re-fire in an interval.
//
// Each fire OBSERVES partition/host state, so a heal between fires is seen
// within one interval and resets the counter as the answered probe would —
// no cut-history inference is needed for episode continuity — and ACTIVITY
// between fires resets it too (the due-recompute branch: an arrival's ACK
// zeroes the kernel's probe counter). The chain is bounded relative to
// quiet: at most count+1 fires (or the user-timeout's grid equivalent) per
// QUIET episode, then death or disarm; inbound activity under a persistent
// one-way cut defers fires by trailing the arrivals, which keeps the bubble
// non-quiescent only while the peer itself is active — quiescence-based
// deadlock detection is preserved.
func (e *dstWireEnd) kaCheck() {
	disarm := func() {
		e.kaMu.Lock()
		e.kaArmed = false
		e.kaEpisode = -1
		e.kaProbesOut = 0
		e.kaMu.Unlock()
	}
	if e.opts == nil || e.timedOut.Load() || e.rstArrived.Load() || isClosedChan(e.localDone) {
		disarm()
		return
	}
	enabled, idleNs, intvlNs, cnt, utoNs := e.opts.kaParams()
	if !enabled {
		disarm()
		return
	}
	now := dstBaseNanos()
	if !e.kaUnanswerable() {
		if e.probeDeadPeer(false) {
			// The probe met the peer's CLOSED socket over a live link: RST,
			// the one-shot ECONNRESET (rstArrived set; blocked ops woken).
			disarm()
			return
		}
		// Answered: the ACK resets the idle clock and the failure state.
		e.kaMu.Lock()
		e.kaArmed = false
		e.kaEpisode = -1
		e.kaProbesOut = 0
		e.kaAckAt = now
		e.kaMu.Unlock()
		return
	}
	act, outstanding := e.kaActivity(now)
	if outstanding {
		disarm() // the retransmit horizon owns the death now
		return
	}
	e.kaMu.Lock()
	if e.kaEpisode < 0 {
		e.kaEpisode = now
	}
	if e.kaAckAt > act {
		act = e.kaAckAt
	}
	due := dstDelayAdd(act, idleNs)
	if e.kaEpisode > due {
		due = e.kaEpisode
	}
	if now < due {
		// Activity moved the first probe instant past this fire: an arrival
		// post-dated the last fire's scheduling (nothing else moves due
		// mid-episode), and that arrival's ACK zeroes the kernel's
		// icsk_probes_out (tcp_ack) — so probes sent before the activity
		// burst never count toward a later kill. Reset here, the ACK-reset
		// point, then re-fire at the due instant.
		e.kaProbesOut = 0
		e.kaMu.Unlock()
		time.AfterFunc(time.Duration(due-now), e.kaCheck)
		return
	}
	kill := false
	if utoNs > 0 {
		kill = now-act >= utoNs && e.kaProbesOut > 0
	} else {
		kill = e.kaProbesOut >= cnt
	}
	if kill {
		e.kaArmed = false
		e.kaEpisode = -1
		e.kaProbesOut = 0
		e.kaMu.Unlock()
		e.horizonDie()
		return
	}
	e.kaProbesOut++
	e.kaMu.Unlock()
	time.AfterFunc(time.Duration(intvlNs), e.kaCheck)
}

// kaPoke wakes this end's blocked reads so they re-evaluate arming — the
// option layer's kick after a keepalive-affecting write (enabling
// SO_KEEPALIVE through a stashed RawConn must reach an already-parked
// read). Deliberately NOT a writer wake: a fabricated send-buffer space
// token would read as window progress to a parked zero-window write and
// reset its user-timeout stall clock; a write parked when an option lands
// picks the change up at its next genuine wake (errs later).
func (e *dstWireEnd) kaPoke() {
	e.in.wake()
}

func (*dstWireEnd) LocalAddr() Addr  { return pipeAddr{} }
func (*dstWireEnd) RemoteAddr() Addr { return pipeAddr{} }

// heldBeyond reports whether the stream still queues anything a cut beginning at
// cutStart holds: a segment (or the FIN) whose delivery lies at or after the cut
// (the same arrived-strictly-before-the-cut boundary pop uses). These are the
// bytes a real sender would be retransmitting into the void. Bytes the
// receiver's death destroyed (deadDropped) count as held forever: the real
// counterpart segments are permanently unacknowledged — a dead socket ACKs
// nothing — so the sender's retransmissions into a cut still exhaust even
// though the simulated segments are gone from the queue.
func (s *dstStream) heldBeyond(cutStart int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadDropped {
		return true
	}
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
	horizon := e.effRetransNs()
	if horizon <= 0 || e.timedOut.Load() {
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
	time.AfterFunc(time.Duration(horizon), e.horizonCheck)
}

// horizonCheck runs at the watchdog's deadline: disarm if the episode ended (heal
// delivered the bytes, or the end closed), extend if a heal-then-recut restarted
// the window, otherwise kill this end — timedOut, wake every blocked operation.
func (e *dstWireEnd) horizonCheck() {
	// rstArrived disarms too: an injected RST destroyed the socket, and its
	// retransmit timer died with it — a later horizon expiry must not flip
	// the conn's ECONNRESET identity to ETIMEDOUT. (The converse order —
	// timeout first, RST later — keeps ETIMEDOUT: read and write consult
	// timedOut first, matching a kernel that stopped accepting segments
	// when retransmission exhausted.)
	if e.timedOut.Load() || e.rstArrived.Load() || isClosedChan(e.localDone) {
		e.horizonMu.Lock()
		e.horizonArmed = false
		e.horizonMu.Unlock()
		return
	}
	cutStart, cut, _ := dstPartCutStartDir(e.localHost, e.peerHost)
	hostDead := dstHostDead(e.peerHost)
	if cut {
		// The cut's own heldBeyond boundary governs, as before.
		if !e.out.heldBeyond(cutStart) {
			e.horizonMu.Lock()
			e.horizonArmed = false
			e.horizonMu.Unlock()
			return
		}
	} else if hostDead {
		// A dead peer host: only destroyed-unacknowledged bytes
		// (deadDropped) keep the watchdog armed — the delivered prefix
		// still queued in the frozen stream was ACKed before the machine
		// died and is not outstanding.
		if !e.out.deadDroppedNow() {
			e.horizonMu.Lock()
			e.horizonArmed = false
			e.horizonMu.Unlock()
			return
		}
	} else {
		// Neither cut nor dead: a heal delivered the bytes, or the machine
		// REBOOTED — a fresh kernel answers the retransmissions with RST
		// (probeDeadPeer at the parked ops' wake), never a timeout. Disarm
		// either way.
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
	if remaining := e.effRetransNs() - (dstBaseNanos() - anchor); remaining > 0 {
		time.AfterFunc(time.Duration(remaining), e.horizonCheck)
		return
	}
	e.horizonMu.Lock()
	e.horizonArmed = false // no AfterFunc pending past the kill
	e.horizonMu.Unlock()
	e.horizonDie()
}

// horizonDie kills this end at its retransmit horizon. Retransmission
// exhaustion runs tcp_write_err → tcp_done: sk_err = ETIMEDOUT pends (the
// one-shot identity — read/write consult timedOut) and the socket is CLOSED,
// its kernel state gone, so the death is TERMINAL for this connection's
// delivery in both directions. The receive direction freezes at the kill
// instant exactly as an injected RST does — bytes delivered before the death
// stay drainable (tcp_recvmsg reports pending data first), bytes in flight or
// held by a cut die, and a later segment or FIN meets a CLOSED socket and is
// never queued, so a heal after the death resurrects nothing. The send
// direction's still-undelivered bytes die too: nothing retransmits them after
// tcp_done, so a heal must never flush them to the peer (the peer keeps only
// what had already arrived, per its own arrival horizon). Each direction's
// death instant is capped at that direction's cut-start, the same
// arrived-strictly-before-the-cut boundary pop and injectRST use. Wakes every
// blocked operation on both ends so each re-evaluates.
func (e *dstWireEnd) horizonDie() {
	e.timedOut.Store(true)
	inHorizon := dstBaseNanos()
	if cutStart, cut, _ := dstPartCutStartDir(e.peerHost, e.localHost); cut && cutStart-1 < inHorizon {
		inHorizon = cutStart - 1
	}
	e.in.freezeAtHorizon(inHorizon)
	outHorizon := dstBaseNanos()
	if cutStart, cut, _ := dstPartCutStartDir(e.localHost, e.peerHost); cut && cutStart-1 < outHorizon {
		outHorizon = cutStart - 1
	}
	e.out.freezeAtHorizon(outHorizon)
	e.horizonOnce.Do(func() { close(e.horizonKill) })
	e.out.wake()       // the peer's blocked reader re-evaluates (its held bytes died; probeDeadPeer)
	e.out.wakeWriter() // this end's blocked writer observes timedOut
	e.in.wake()        // this end's blocked reader observes timedOut
	e.in.wakeWriter()  // the peer's blocked writer re-evaluates (the buffer will never drain; probeDeadPeer)
}

// injectRST delivers a fault-injected RST to this end — the kernel-faithful
// RECEIVER shape (host-probed): an incoming RST sets the socket error, but
// tcp_recvmsg reports pending data before the error, so bytes already
// DELIVERED to this end's receive queue stay readable and drain first;
// only then do reads fail (the dstConn wrapper maps the failure and the
// drained-EOF through the shared reset flag to the one-shot sk_err identity —
// first failing op ECONNRESET, later reads EOF, later writes EPIPE; an RST
// arriving after the peer's FIN takes the CLOSE_WAIT arm, rstCloseWait).
// Writes fail immediately (the socket error is already pending). Bytes
// still IN FLIGHT toward this end are destroyed: the RST reached the socket
// before they did — one of the orderings a real injection race produces —
// and a crashed sender's untransmitted send buffer never leaves its kernel.
// "Delivered" is the same arrival horizon Read uses: under a partition cut
// only bytes that arrived strictly before the cut are in the receive queue;
// everything the cut holds dies with the connection.
func (e *dstWireEnd) injectRST() {
	// Once-guarded: a second injection (two fault ops racing to the same
	// conn) must not re-freeze at a later horizon and resurrect bytes the
	// first RST already killed.
	e.rstOnce.Do(func() {
		horizon := dstBaseNanos()
		if cutStart, cut, _ := dstPartCutStartDir(e.peerHost, e.localHost); cut && cutStart-1 < horizon {
			horizon = cutStart - 1
		}
		if e.in.freezeAtHorizon(horizon) {
			// The peer's FIN (and everything ahead of it) had already arrived:
			// the RST met a CLOSE_WAIT socket (tcp_reset pends EPIPE there).
			e.rstCloseWait.Store(true)
		}
		e.rstArrived.Store(true)
		close(e.rstKill)
	})
	e.in.wake()        // a reader parked on the ready channel re-evaluates
	e.out.wakeWriter() // a writer parked on send-buffer space observes the RST
}

// probeDeadPeer is the probe seam for PARKED operations: a blocked read or
// write woken while the counterpart socket is dead (this end's out stream
// frozen — the receiver was destroyed by a retransmit-horizon death)
// re-evaluates here instead of re-parking forever. Production never stops
// probing a stalled connection: a blocked writer's zero-window probes, and a
// blocked reader's retransmissions of its destroyed, never-to-be-ACKed bytes
// (needDropped — a reader with nothing outstanding has no segment in flight
// to elicit an RST and hangs on a real kernel too, so probing there would be
// a sim-only false failure). When such a segment meets the CLOSED socket
// over a link live in BOTH directions, the answered RST surfaces the
// one-shot ECONNRESET exactly as a push into the frozen stream does
// (dstDeadPushRST; the caller then observes rstArrived) — and with the same
// zero-round-trip timing collapse: the RST lands at the wake, not after a
// zero-window-probe interval. Under a cut the probe stays silent and the cut
// arms govern instead: a forward (local→peer) cut is this end's own
// retransmit horizon (heldBeyond/deadDropped → armHorizon → ETIMEDOUT), and
// a return-only (peer→local) cut swallows the RST — the recorded flow-level
// ACK-starvation limit (faults.md, Partition), the safe ⊆-real direction. An
// end already dead (timedOut) or reset (rstArrived) keeps its earlier
// identity: sk_err is one field, whichever teardown pends first owns it.
func (e *dstWireEnd) probeDeadPeer(needDropped bool) bool {
	e.out.mu.Lock()
	frozen, dropped := e.out.frozen, e.out.deadDropped
	e.out.mu.Unlock()
	if !frozen || (needDropped && !dropped) {
		return false
	}
	if e.timedOut.Load() || e.rstArrived.Load() ||
		dstPartitionedDir(e.localHost, e.peerHost) || dstPartitionedDir(e.peerHost, e.localHost) ||
		dstHostDead(e.peerHost) {
		// A DEAD peer host answers nothing (power loss has no kernel to RST
		// with) — silence, until the machine reboots and its fresh kernel
		// meets the probe. The cut arms and the host-dead arm alike leave
		// the death to the retransmit/keepalive laws.
		return false
	}
	dstDeadPushRST(e)
	return true
}

// freezeAtHorizon drops every segment not yet delivered at horizon and
// FREEZES the stream — a socket death (an injected RST, or a retransmit-
// horizon kill: tcp_write_err → tcp_done, socket CLOSED) destroys in-flight
// bytes, never the receive queue, and the dead socket accepts nothing
// further: a push after the freeze is discarded under the same lock (program
// order, not timestamps — at zero latency a post-death push carries
// deliverAt equal to the horizon, so no timestamp comparison can separate it
// from a drainable same-instant pre-death byte). The receive queue kept is
// the longest FIFO PREFIX whose every segment has arrived (head-of-line: a
// jitter draw can give a later segment an earlier deliverAt, but it is
// readable only behind its predecessors — exactly pop's delivery rule), so
// the scan stops at the first undelivered segment and the whole remainder
// dies. The dropped suffix is cleared so the backing array does not retain
// the dead buffers. A FIN that had NOT arrived at the horizon dies with the
// in-flight bytes — the dead socket will never receive it, and keeping it
// recorded would resurrect a clean EOF once its closeAt passes. Any drop
// (segment or FIN) marks deadDropped: the sender's counterpart segments are
// permanently unacknowledged (heldBeyond). Reports whether the writer's FIN
// had ARRIVED at the horizon — closed with closeAt within the horizon and no
// segment dropped (in order, a FIN cannot overtake data): the
// RST-met-CLOSE_WAIT discriminant.
func (s *dstStream) freezeAtHorizon(horizon int64) (finArrived bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = true
	live := 0
	for live < len(s.segs) && s.segs[live].deliverAt <= horizon {
		live++
	}
	finArrived = s.closed && s.closeAt <= horizon && live == len(s.segs)
	if s.closed && !finArrived {
		s.closed = false
		s.deadDropped = true
	}
	if live < len(s.segs) {
		s.deadDropped = true
	}
	for i := live; i < len(s.segs); i++ {
		s.buffered -= int64(len(s.segs[i].data))
		s.segs[i] = dstSeg{}
	}
	s.segs = s.segs[:live]
	if len(s.segs) == 0 {
		s.segs = nil
	}
	return finArrived
}

// deadDroppedNow reports whether this stream carries destroyed-unacknowledged
// bytes (deadDropped) — the ONLY outstanding-data witness after a crash
// freeze: the delivered prefix still queued in a frozen stream was ACKed by
// the peer's kernel before it died, and production retransmits nothing for
// ACKed bytes (arming a horizon on them would be a sim-only ETIMEDOUT).
func (s *dstStream) deadDroppedNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deadDropped
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
		// An injected RST follows the same tcp_recvmsg rule: the delivered
		// bytes above drained first (injectRST truncated everything else), and
		// only then does the socket error surface — before any FIN wait, since
		// the RST supersedes a still-traveling graceful close. The dstConn
		// wrapper maps ErrClosedPipe through the one-shot sk_err (consumeSkErr;
		// the shared reset flag routes the drained-EOF path there too).
		if e.rstArrived.Load() {
			return 0, io.ErrClosedPipe
		}
		// A read parked against a DEAD counterpart socket re-probes at every
		// wake (probeDeadPeer): its destroyed, never-to-be-ACKed outbound
		// bytes are what production keeps retransmitting, and over a live
		// link the retransmission meets the CLOSED socket and the answered
		// RST surfaces the one-shot ECONNRESET — the next iteration drains
		// any delivered bytes, then observes rstArrived. This is what rescues
		// a heal landing between the counterpart's death and this end's own
		// horizonCheck (which then sees cut=false and disarms): without the
		// probe the read would re-park forever.
		if e.probeDeadPeer(true) {
			continue
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
		if dstHostDead(e.peerHost) && e.out.deadDroppedNow() {
			// A dead peer HOST destroyed this end's unacknowledged bytes
			// (the crash freeze marks them deadDropped; anything pushed
			// later joins them): the retransmissions into the silent
			// machine exhaust at the horizon. Bytes merely QUEUED in the
			// frozen stream are the delivered-and-ACKed prefix — production
			// retransmits nothing for them, so they never arm.
			e.armHorizon()
		}
		// A blocked read is the keepalive law's observer: an idle connection
		// (nothing outstanding — otherwise the horizon above governs) whose
		// probes are unanswerable dies at the probe schedule's exhaustion,
		// the death this parked read surfaces (horizonKill wake → timedOut).
		e.armKeepalive()
		if cut {
			// Arrived-before-cut bytes exhausted; anything else (in flight, written
			// after the cut, or a not-yet-arrived FIN) is held. Block until heal, a
			// deadline, a local close, or the outbound retransmit horizon killing
			// this end. The ready channel is a wake source here too: it carries
			// no deliverable bytes under a cut (pop's horizon holds them), but
			// the option layer's poke rides it (kaPoke) — a keepalive enabled
			// through a stashed RawConn must reach this parked read to arm the
			// watchdog, and a spurious wake just re-evaluates and re-parks.
			select {
			case <-e.in.ready:
			case <-wake:
			case <-e.horizonKill:
			case <-e.rstKill:
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
			timer = dstNewBaseTimer(wait)
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
		case <-e.rstKill:
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
	case e.rstArrived.Load():
		// An injected RST already landed: the socket error is pending and a
		// send fails with it immediately (no drain applies to writes). The
		// dstConn wrapper maps ErrClosedPipe through the one-shot sk_err.
		return 0, io.ErrClosedPipe
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
		// Re-check the injected-RST state on every chunk, not just at
		// entry: under access-granularity (Level 2) scheduling the fault op
		// can interleave between chunk pushes — a real kernel's destroyed
		// connection can never carry the remaining bytes. (A horizon death
		// needs no flag re-check here: horizonDie freezes this very stream, so
		// a racing chunk is discarded under the stream lock — the push seam
		// enforces it structurally.)
		if e.rstArrived.Load() {
			return total, io.ErrClosedPipe
		}
		e.out.mu.Lock()
		for e.out.capacity > 0 && e.out.buffered >= e.out.capacity {
			e.out.mu.Unlock()
			// Fetch the partition wake channel before reading the cut state, so a cut
			// that begins (or heals) while we block still re-evaluates the horizon.
			// The probe below reads the cut state too, so it must also follow the
			// fetch: a heal landing after a pre-fetch probe declined would close
			// only the superseded channel — a lost wakeup, and the parked writer
			// would strand exactly where the probe exists to fail it.
			wake := dstPartWakeCh()
			// A write parked on a full send buffer toward a DEAD counterpart
			// socket re-probes at every wake (probeDeadPeer): the frozen
			// buffer will never drain, and production's zero-window probes
			// against the CLOSED socket are answered with an RST over a live
			// link — the blocked send fails with the one-shot ECONNRESET
			// instead of re-parking forever. Under a cut the arms are
			// unchanged: forward-cut bytes die at this end's own horizon
			// below, and a return-only cut swallows the RST.
			if e.probeDeadPeer(false) {
				return total, io.ErrClosedPipe
			}
			var horizonC <-chan time.Time
			var horizonT *time.Timer
			if horizon := e.effRetransNs(); horizon > 0 && (dstPartitionedDir(e.localHost, e.peerHost) || dstHostDead(e.peerHost)) { // outgoing local→peer is where a write's bytes are held; a DEAD peer host is as undeliverable as a cut
				if cutStart < 0 {
					cutStart = dstBaseNanos() // the cut-block began; a heal resets it, restarting the timer on ACK progress
				}
				// The window is a base-time delta (skew-invariant); the timer fires on
				// the writer's host clock, so under a DriftClock rate change "retransNs
				// of base time" shifts slightly — deterministic, and faithful to a real
				// retransmit timer running on the sender's own clock.
				remaining := horizon - (dstBaseNanos() - cutStart)
				if remaining <= 0 {
					e.horizonDie()
					return total, syscall.ETIMEDOUT
				}
				horizonT = time.NewTimer(time.Duration(remaining))
				horizonC = horizonT.C
			} else if uto := e.userTimeoutNs(); uto > 0 {
				// TCP_USER_TIMEOUT bounds a ZERO-WINDOW stall too (tcp(7),
				// tcp_probe_timer checks icsk_user_timeout): a write parked on
				// a full send buffer with no window progress for the timeout
				// dies ETIMEDOUT even against a LIVE peer — the one exception
				// to the persist-forever model, opted into per socket. The
				// anchor is the block's start, reset on any freed space
				// (window progress), the errs-later direction.
				if cutStart < 0 {
					cutStart = dstBaseNanos()
				}
				remaining := uto - (dstBaseNanos() - cutStart)
				if remaining <= 0 {
					e.horizonDie()
					return total, syscall.ETIMEDOUT
				}
				horizonT = time.NewTimer(time.Duration(remaining))
				horizonC = horizonT.C
			} else {
				cutStart = -1 // live peer (or no horizon): persist, reset the cut window
			}
			select {
			case <-e.out.space:
				// Freed space is window progress (the peer's receiver
				// drained): re-anchor the stall window — production's
				// user-timeout clock resets on ACK progress. Errs later
				// under a cut (pre-cut drains defer the death, never
				// hasten it); the async watchdog (armHorizon) still bounds
				// held bytes independently.
				cutStart = -1
			case <-wake: // partition began or healed: re-evaluate the horizon
			case <-e.rstKill:
				if horizonT != nil {
					horizonT.Stop()
				}
				return total, io.ErrClosedPipe
			case <-e.horizonKill:
				if horizonT != nil {
					horizonT.Stop()
				}
				return total, syscall.ETIMEDOUT
			case <-horizonC:
				e.horizonDie()
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
			// Re-check the injected-RST state after EVERY wake, before the
			// buffer is touched again — including the freed-space wake that
			// exits this loop. A parked writer can have COMMITTED to the
			// freed-space arm before the RST landed (the peer drains, then
			// the fault op runs, with the writer runnable but not yet
			// resumed), so the select choice alone cannot carry the check:
			// without it the resumed writer pushes post-RST bytes the peer
			// could then read — a sim-only execution (a real kernel wakes a
			// blocked sender with the pending socket error, and the
			// destroyed connection never carries another byte). A horizon
			// death is covered structurally instead: horizonDie freezes this
			// stream, so a racing push is discarded at the push seam.
			if e.rstArrived.Load() {
				return total, io.ErrClosedPipe
			}
			e.out.mu.Lock()
		}
		room := int64(len(b))
		if e.out.capacity > 0 {
			if avail := e.out.capacity - e.out.buffered; avail < room {
				room = avail
			}
		}
		dead := e.out.pushLocked(b[:room], e.latencyNs, e.jitterNs, e.bandwidthBps)
		roomRemains := e.out.capacity > 0 && e.out.buffered < e.out.capacity
		e.out.mu.Unlock()
		e.out.wake()
		if dead && !e.timedOut.Load() && !e.rstArrived.Load() &&
			!dstPartitionedDir(e.localHost, e.peerHost) && !dstPartitionedDir(e.peerHost, e.localHost) &&
			!dstHostDead(e.peerHost) {
			// The segment met a dead (CLOSED) peer socket over a live link: the
			// dead kernel answers it with an RST that reaches this end. The
			// local send's success stands — a real send() into a doomed conn
			// succeeds first — and subsequent operations carry the one-shot
			// reset identity (ECONNRESET), exactly production's shape for a
			// peer that timed out and died. Under a cut no RST can traverse:
			// the segment is silently dropped and this end's own
			// retransmissions exhaust at ITS horizon instead (deadDropped keeps
			// the destroyed bytes counting as held — armHorizon below). A cut
			// of only the RETURN direction swallows the RST too; that end
			// neither resets nor times out here — the recorded flow-level
			// ACK-starvation limit (faults.md, Partition), the safe ⊆-real
			// direction.
			dstDeadPushRST(e)
		}
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
		if (dstPartAnyCut() && dstPartitionedDir(e.localHost, e.peerHost)) || dstHostDead(e.peerHost) {
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
