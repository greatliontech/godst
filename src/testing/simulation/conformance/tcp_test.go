// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package conformance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
	"unsafe"
)

// The TCP grammar is confined to the modeled surface: the net.Conn /
// net.Listener interfaces over "tcp" loopback dials by IP (the spec
// records that *net.TCPConn assertions fail and DNS is a follow-on, so
// neither is probed). Addresses and ports are leg-local and never enter
// outcomes; dials reference listener slots.

// tcpOutstandingMax bounds unread bytes per direction so plain writes
// never block on either leg (host kernel socket buffers are tunable but
// far above this; the sim send buffer is 1 MiB).
const tcpOutstandingMax = 60000

// ---------------------------------------------------------------------------
// TCP ops.

func tcpListen(track bool) op {
	return op{
		name: fmt.Sprintf("listen(127.0.0.1:0) track=%v", track),
		run: func(w *world) outcome {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if track {
				w.lns = append(w.lns, ln)
			} else if ln != nil {
				ln.Close()
			}
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

func slotLn(w *world, slot int) net.Listener {
	if slot < 0 || slot >= len(w.lns) {
		return nil
	}
	return w.lns[slot]
}

func slotConn(w *world, slot int) net.Conn {
	if slot < 0 || slot >= len(w.conns) {
		return nil
	}
	return w.conns[slot]
}

// tcpDial dials the listener slot's address (recorded per leg at run
// time — never compared). track appends the conn slot.
func tcpDial(lnSlot int, track bool) op {
	return op{
		name: fmt.Sprintf("dial(ln %d) track=%v", lnSlot, track),
		run: func(w *world) outcome {
			ln := slotLn(w, lnSlot)
			if ln == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			c, err := net.Dial("tcp", ln.Addr().String())
			if track {
				w.conns = append(w.conns, c)
			} else if c != nil {
				c.Close()
			}
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

// tcpDialClosed dials an address recorded from a listener that has been
// closed: a live kernel answers RST (ECONNREFUSED) on both legs.
func tcpDialClosed(lnSlot int) op {
	return op{
		name: fmt.Sprintf("dial-closed(ln %d)", lnSlot),
		run: func(w *world) outcome {
			ln := slotLn(w, lnSlot)
			if ln == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			c, err := net.Dial("tcp", ln.Addr().String())
			if c != nil {
				c.Close()
			}
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

// tcpDialCanceled dials with an already-canceled context.
func tcpDialCanceled(lnSlot int) op {
	return op{
		name: fmt.Sprintf("dial-canceled-ctx(ln %d)", lnSlot),
		run: func(w *world) outcome {
			ln := slotLn(w, lnSlot)
			if ln == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var d net.Dialer
			c, err := d.DialContext(ctx, "tcp", ln.Addr().String())
			if c != nil {
				c.Close()
			}
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

func tcpAccept(lnSlot int, track bool) op {
	return op{
		name: fmt.Sprintf("accept(ln %d) track=%v", lnSlot, track),
		run: func(w *world) outcome {
			ln := slotLn(w, lnSlot)
			if ln == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			type res struct {
				c   net.Conn
				err error
			}
			// Bound a would-hang accept (nothing pending) so a harness
			// bug cannot wedge the host leg: close the listener from a
			// timer if accept has not returned. The generator only
			// emits accept with a pending dial, so the bound is safety
			// netting, not semantics.
			done := make(chan res, 1)
			go func() {
				c, err := ln.Accept()
				done <- res{c, err}
			}()
			var r res
			select {
			case r = <-done:
			case <-time.After(guardReady):
				ln.Close()
				r = <-done
				r.err = fmt.Errorf("conformance: accept guard expired: %w", r.err)
			}
			if track {
				w.conns = append(w.conns, r.c)
			} else if r.c != nil {
				r.c.Close()
			}
			return outcome{Err: errClass(r.err), N: -1}
		},
	}
}

func tcpAcceptClosed(lnSlot int) op {
	return op{
		name: fmt.Sprintf("accept-closed(ln %d)", lnSlot),
		run: func(w *world) outcome {
			ln := slotLn(w, lnSlot)
			if ln == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			c, err := ln.Accept()
			if c != nil {
				c.Close()
			}
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

func tcpCloseLn(slot int) op {
	return op{
		name: fmt.Sprintf("close-ln(%d)", slot),
		run: func(w *world) outcome {
			ln := slotLn(w, slot)
			if ln == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(ln.Close()), N: -1}
		},
	}
}

// tcpWrite writes a payload expected to be accepted without blocking
// (the generator caps outstanding unread bytes). label tags the
// recorded-divergence arms (write-after-peer-close, post-reset).
func tcpWrite(label string, slot int, payload []byte) op {
	return op{
		name:      fmt.Sprintf("%s(conn %d, %d bytes)", label, slot, len(payload)),
		writeSize: len(payload),
		run: func(w *world) outcome {
			c := slotConn(w, slot)
			if c == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			c.SetWriteDeadline(time.Now().Add(guardReady))
			n, err := c.Write(payload)
			c.SetWriteDeadline(time.Time{})
			return outcome{Err: errClass(err), N: n}
		},
	}
}

// tcpReadN reads exactly n bytes (delivery-tolerant: TCP promises no
// read-boundary alignment on either leg, so exact-count reads go
// through io.ReadFull under a generous guard).
func tcpReadN(label string, slot, n int) op {
	return op{
		name: fmt.Sprintf("%s(conn %d, %d bytes)", label, slot, n),
		run: func(w *world) outcome {
			c := slotConn(w, slot)
			if c == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			c.SetReadDeadline(time.Now().Add(guardReady))
			buf := make([]byte, n)
			rn, err := io.ReadFull(c, buf)
			c.SetReadDeadline(time.Time{})
			return outcome{Err: errClass(err), N: rn, State: contentHash(buf[:max(rn, 0)])}
		},
	}
}

// tcpRead is a single bare Read (EOF/reset shapes; the guard bounds a
// would-block read).
func tcpRead(label string, slot, n int, guard time.Duration) op {
	return op{
		name: fmt.Sprintf("%s(conn %d, %d bytes, guard %v)", label, slot, n, guard),
		run: func(w *world) outcome {
			c := slotConn(w, slot)
			if c == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			if guard > 0 {
				c.SetReadDeadline(time.Now().Add(guard))
			}
			buf := make([]byte, n)
			rn, err := c.Read(buf)
			if guard > 0 {
				c.SetReadDeadline(time.Time{})
			}
			return outcome{Err: errClass(err), N: rn, State: contentHash(buf[:max(rn, 0)])}
		},
	}
}

func tcpCloseConn(slot int) op {
	return op{
		name: fmt.Sprintf("close-conn(%d)", slot),
		run: func(w *world) outcome {
			c := slotConn(w, slot)
			if c == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(c.Close()), N: -1}
		},
	}
}

func tcpSetReadDeadline(slot int, kind string) op {
	return op{
		name: fmt.Sprintf("conn-set-read-deadline(%d, %s)", slot, kind),
		run: func(w *world) outcome {
			c := slotConn(w, slot)
			if c == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(c.SetReadDeadline(deadlineTime(kind))), N: -1}
		},
	}
}

// Settle conditions: what a settle waits for on the polled connection.
const (
	settleData = "data-or-fin" // delivered bytes or a FIN: the fd polls readable
	settleErr  = "rst-error"   // an RST's socket error: POLLERR (or POLLHUP once reported)
)

// Linux poll(2) event bits (the syscall package exports no poll constants).
const (
	pollIN  = 0x0001
	pollERR = 0x0008
	pollHUP = 0x0010
)

// tcpSettle waits until slot's connection observably carries cond, so
// in-flight control traffic (FIN/RST) or data has landed before the next
// op keys on it. On the host the real fd is polled non-destructively
// (poll(2) reports POLLERR without consuming sk_err, unlike SO_ERROR)
// every 10ms up to a generous 2s cap — host load can only slow the wait,
// never flip an outcome. A simulated conn exposes no fd (the type
// assertion fails), so the sim leg keeps a fixed virtual sleep:
// deterministic, free, and delivery rides the schedule. slot -1 is a
// plain both-legs sleep for shapes with no conn to poll.
func tcpSettle(slot int, cond string) op {
	return op{
		name: fmt.Sprintf("settle(conn %d, %s)", slot, cond),
		run: func(w *world) outcome {
			c := slotConn(w, slot)
			if c == nil {
				time.Sleep(20 * time.Millisecond)
				return outcome{N: -1}
			}
			sc, ok := c.(syscall.Conn)
			if !ok {
				// The simulated conn: virtual, deterministic.
				time.Sleep(20 * time.Millisecond)
				return outcome{N: -1}
			}
			deadline := time.Now().Add(2 * time.Second)
			for !hostConnCond(sc, cond) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			return outcome{N: -1}
		},
	}
}

// hostConnCond reports whether the host connection's fd currently shows
// cond. Errors reaching the fd (a closed conn) count as settled: there
// is nothing left to wait for.
func hostConnCond(sc syscall.Conn, cond string) bool {
	rc, err := sc.SyscallConn()
	if err != nil {
		return true
	}
	matched := false
	cerr := rc.Control(func(fd uintptr) {
		pfd := struct {
			fd      int32
			events  int16
			revents int16
		}{fd: int32(fd), events: pollIN}
		ts := syscall.Timespec{} // zero timeout: non-blocking poll
		n, _, _ := syscall.Syscall6(syscall.SYS_PPOLL,
			uintptr(unsafe.Pointer(&pfd)), 1, uintptr(unsafe.Pointer(&ts)), 0, 0, 0)
		if n != 1 {
			return
		}
		switch cond {
		case settleErr:
			matched = pfd.revents&(pollERR|pollHUP) != 0
		default:
			matched = pfd.revents&(pollIN|pollERR|pollHUP) != 0
		}
	})
	return cerr != nil || matched
}

// ---------------------------------------------------------------------------
// The TCP allowlist.

// hostCloseInFlightCanFIN probes whether THIS host can close a conn
// BEFORE loopback delivery of bytes already written toward it — the FIN
// ordering of the close-vs-arrival race. The simulation always takes the
// RST ordering (bytes still in flight toward the closer count as queued,
// the recorded collapse), so the divergent first-write shape below is
// reachable only where the host's FIN ordering occurs (loopback delivery
// usually wins the race); the entry is host-condition-gated on observing
// it, like fs-chtimes-omit-missing.
var hostCloseInFlightCanFIN = sync.OnceValue(func() bool {
	for range 256 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return false
		}
		d, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			ln.Close()
			return false
		}
		a, err := ln.Accept()
		if err != nil {
			d.Close()
			ln.Close()
			return false
		}
		d.Write([]byte{1})
		a.Close() // races the byte's loopback delivery: empty queue FINs, queued byte RSTs
		d.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, rerr := d.Read(make([]byte, 1))
		fin := errors.Is(rerr, io.EOF)
		d.Close()
		ln.Close()
		if fin {
			return true
		}
	}
	return false
})

func tcpAllowlist() []allowEntry {
	return []allowEntry{
		{
			key:        "net-close-in-flight-fin-ordering",
			cite:       `design.md §In-memory deterministic network: "a Close() of an end whose receive queue holds UNREAD data answers with RST instead of FIN (the kernel's close(2) conditional; bytes still in flight toward the closer count as queued for that decision — the recorded collapse: the sim RSTs immediately, one of the two orderings the real close-vs-arrival race produces)" — when the host's ordering runs the other way (close beats delivery), the host end FINs: its peer's writes are accepted until one elicits the RST and its reads see io.EOF, where the sim's conn is already reset (one-shot sk_err: ECONNRESET on the first failing op, then EOF/EPIPE, agreeing with the host again).`,
			applicable: func() bool { return hostCloseInFlightCanFIN() },
			match: func(o op, host, sim outcome) bool {
				// The host-FIN-ordering window on a conn the generator model
				// reset via close-with-in-flight. Write leg: the host accepts
				// the payload (the RST-eliciting write) where the sim's reset
				// conn refuses — ECONNRESET if this op consumes its one-shot
				// sk_err, EPIPE if an earlier op already did. Read leg: the
				// host reads the FIN's io.EOF where the sim's first failing
				// read consumes ECONNRESET (later sim reads are io.EOF and
				// agree exactly).
				if strings.HasPrefix(o.name, "post-reset-write(") {
					return host.Err == "" && host.N == o.writeSize &&
						(sim.Err == "OpError(write)/errno:ECONNRESET" || sim.Err == "OpError(write)/errno:EPIPE") &&
						sim.N == 0
				}
				if strings.HasPrefix(o.name, "post-reset-read(") {
					return host.Err == "EOF" &&
						sim.Err == "OpError(read)/errno:ECONNRESET" &&
						host.N == sim.N
				}
				return false
			},
		},
		{
			key:  "net-first-write-after-fin",
			cite: `design.md §In-memory deterministic network: "Today the first write after a peer close fails instantly with ECONNRESET (the wire rejects a write whose peer end is gone); the succeed-then-RST round trip is the follow-on's work."`,
			match: func(o op, host, sim outcome) bool {
				// Exactly the FIRST write after the peer's clean close: the
				// host accepts it (the RST round trip is still in flight),
				// the sim fails it instantly. From the second write on, both
				// legs carry the kernel's one-shot post-reset identities
				// (write EPIPE, read EOF) and must agree exactly — no
				// allowlist entry exists for them.
				return strings.HasPrefix(o.name, "write-after-peer-close#1(") &&
					host.Err == "" &&
					sim.Err == "OpError(write)/errno:ECONNRESET"
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Fixed coverage ladder.

func tcpCoverageOps() []op {
	var ops []op
	add := func(o op) { ops = append(ops, o) }

	// Listener 0; conns: 0=dialer A, 1=accepted A.
	add(tcpListen(true))
	add(tcpDial(0, true))
	add(tcpAccept(0, true))

	// Byte-stream transfer and exact-read normalization.
	add(tcpWrite("write", 0, pat(1000, 50)))
	add(tcpReadN("readn", 1, 1000))
	add(tcpWrite("write", 1, pat(8192, 51)))
	add(tcpWrite("write", 1, pat(100, 52))) // reads coalesce across writes
	add(tcpReadN("readn", 0, 8292))

	// Zero-length ops.
	add(tcpRead("read-zero", 0, 0, 0))
	add(tcpWrite("write-zero", 0, nil))

	// Deadline arms: expired deadline with delivered data unread.
	add(tcpWrite("write", 1, pat(64, 53)))
	add(tcpSettle(0, settleData)) // data delivered before the deadline probe
	add(tcpSetReadDeadline(0, dlPast))
	add(tcpRead("read-expired-deadline", 0, 64, 0)) // deadline beats delivered data
	add(tcpSetReadDeadline(0, dlClear))
	add(tcpReadN("readn", 0, 64))                // data intact after clear
	add(tcpRead("read-block", 0, 8, guardBlock)) // empty: ErrDeadlineExceeded

	// FIN ladder: graceful close drains then EOF (persistent).
	add(tcpWrite("write", 1, pat(300, 54)))
	add(tcpCloseConn(1))
	add(tcpSettle(0, settleData))
	add(tcpReadN("readn-before-eof", 0, 300)) // drains buffered data
	add(tcpRead("read-eof", 0, 16, guardReady))
	add(tcpRead("read-eof-again", 0, 16, 0)) // EOF is persistent state

	// Post-FIN write ladder. Write #1 is the recorded follow-on gap (host
	// accepts, sim fails instantly); from #2 on both legs carry the kernel's
	// one-shot post-reset identities (write EPIPE, read EOF) exactly.
	add(tcpWrite("write-after-peer-close#1", 0, pat(32, 55)))
	add(tcpSettle(0, settleErr)) // the RST round trip
	add(tcpWrite("write-after-peer-close#2", 0, pat(32, 56)))
	add(tcpSettle(0, settleErr))
	add(tcpWrite("post-reset-write", 0, pat(32, 57)))
	add(tcpRead("post-reset-read", 0, 16, 0))
	// Ops on the locally closed end.
	add(tcpWrite("write-on-closed", 1, pat(4, 58)))
	add(tcpRead("read-on-closed", 1, 4, 0))
	add(tcpCloseConn(1)) // double close
	add(tcpSetReadDeadline(1, dlPast))
	add(tcpCloseConn(0))

	// RST ladder: close with unread receive data resets the peer. The
	// one-shot sk_err ladder is pinned end-to-end on both legs: first read
	// ECONNRESET, later writes EPIPE, later reads persistently EOF.
	add(tcpDial(0, true))                   // conn 2
	add(tcpAccept(0, true))                 // conn 3
	add(tcpWrite("write", 2, pat(500, 59))) // 3 never reads it
	add(tcpSettle(3, settleData))           // the 500 bytes must be QUEUED at 3 for its close to RST
	add(tcpCloseConn(3))                    // unread data: RST, not FIN
	add(tcpSettle(2, settleErr))
	add(tcpRead("read-after-rst", 2, 16, guardReady)) // ECONNRESET, consuming sk_err (2's receive queue is empty: nothing to drain before the error)
	add(tcpWrite("post-reset-write", 2, pat(8, 60)))  // EPIPE (sk_err consumed)
	add(tcpRead("post-reset-read", 2, 8, 0))          // EOF (CLOSED-socket read)
	add(tcpRead("post-reset-read", 2, 8, 0))          // EOF persists
	add(tcpCloseConn(2))

	// Accept FIFO order, pinned via a tag byte per dialer.
	add(tcpDial(0, true)) // conn 4
	add(tcpDial(0, true)) // conn 5
	add(tcpWrite("write", 4, []byte{0xA1}))
	add(tcpWrite("write", 5, []byte{0xB2}))
	add(tcpAccept(0, true)) // conn 6: first dial's peer
	add(tcpAccept(0, true)) // conn 7: second dial's peer
	add(tcpReadN("readn-accept-order-first", 6, 1))
	add(tcpReadN("readn-accept-order-second", 7, 1))
	for _, c := range []int{4, 5, 6, 7} {
		add(tcpCloseConn(c))
	}

	// Canceled-context dial, listener close ladder, refused dial.
	add(tcpDialCanceled(0))
	add(tcpCloseLn(0))
	add(tcpAcceptClosed(0))        // ErrClosed
	add(tcpCloseLn(0))             // double close: ErrClosed
	add(tcpSettle(-1, settleData)) // no conn to poll: plain sleep (close(2) of a listener is synchronous)
	add(tcpDialClosed(0))          // ECONNREFUSED

	return ops
}

// tcpCoverageState: listeners and conns the ladder leaves in the slot
// tables (for the random generator's numbering).
const (
	tcpCoverageLns   = 1
	tcpCoverageConns = 8
)

// ---------------------------------------------------------------------------
// Random grammar.

type tcpConnState struct {
	peer       int // peer conn slot; -1 until paired
	closed     bool
	peerClosed bool
	reset      bool
	unread     int // bytes written by peer, not yet read here
	postClose  int // writes since the peer's close
}

type tcpGen struct {
	rng     *rand.Rand
	ops     []op
	ln      int // the one live listener slot
	pending int // dials not yet accepted
	conns   []tcpConnState
	nConns  int
}

func (g *tcpGen) add(o op) { g.ops = append(g.ops, o) }

// pair dials and accepts, minting a linked conn pair.
func (g *tcpGen) pair() (int, int) {
	g.add(tcpDial(g.ln, true))
	d := g.nConns
	g.nConns++
	g.add(tcpAccept(g.ln, true))
	a := g.nConns
	g.nConns++
	g.conns = append(g.conns, tcpConnState{peer: a}, tcpConnState{peer: d})
	return d, a
}

func (g *tcpGen) step() {
	live := 0
	for _, c := range g.conns {
		if !c.closed {
			live++
		}
	}
	if live == 0 && len(g.conns) >= 16 {
		return // sequence exhausted its conn budget
	}
	if live == 0 {
		g.pair()
		return
	}
	idx := g.rng.IntN(len(g.conns))
	c := &g.conns[idx]
	r := g.rng.IntN(100)
	switch {
	case r < 8:
		if len(g.conns) < 16 {
			g.pair()
		}
	case r < 40: // write
		if c.closed {
			g.add(tcpWrite("write-on-closed", idx, pat(4, byte(g.rng.IntN(256)))))
			return
		}
		size := 1 + g.rng.IntN(8192)
		peer := &g.conns[c.peer]
		switch {
		case c.reset || c.peerClosed && c.postClose >= 2:
			g.add(tcpWrite("post-reset-write", idx, pat(size, byte(g.rng.IntN(256)))))
			c.reset = true
		case c.peerClosed:
			c.postClose++
			g.add(tcpWrite(fmt.Sprintf("write-after-peer-close#%d", c.postClose), idx, pat(size, byte(g.rng.IntN(256)))))
			g.add(tcpSettle(idx, settleErr)) // the RST the closed peer answers with
			if c.postClose >= 2 {
				c.reset = true
			}
		case peer.unread+size <= tcpOutstandingMax:
			g.add(tcpWrite("write", idx, pat(size, byte(g.rng.IntN(256)))))
			peer.unread += size
		default:
			// Peer saturated by the harness cap: drain it instead.
			g.add(tcpReadN("readn", c.peer, peer.unread))
			peer.unread = 0
		}
	case r < 70: // read
		if c.closed {
			g.add(tcpRead("read-on-closed", idx, 8, 0))
			return
		}
		switch {
		case c.reset:
			g.add(tcpRead("post-reset-read", idx, 8, 0))
		case c.unread > 0:
			n := 1 + g.rng.IntN(c.unread)
			g.add(tcpReadN("readn", idx, n))
			c.unread -= n
		case c.peerClosed:
			g.add(tcpRead("read-eof", idx, 8, guardReady))
		default:
			g.add(tcpRead("read-block", idx, 8, guardBlock))
		}
	case r < 82: // close (double closes included)
		hadUnread := c.unread > 0
		g.add(tcpCloseConn(idx))
		// Settle the PEER: the close's control segment (RST when this
		// end had unread inbound bytes, FIN otherwise) must land before
		// later ops key on it.
		cond := settleData
		if hadUnread {
			cond = settleErr
		}
		g.add(tcpSettle(c.peer, cond))
		if !c.closed {
			c.closed = true
			peer := &g.conns[c.peer]
			if !peer.closed {
				if hadUnread {
					peer.reset = true // close with unread data: RST
				}
				peer.peerClosed = true
			}
		}
	default: // deadline arms
		if c.closed {
			g.add(tcpSetReadDeadline(idx, dlPast))
			return
		}
		if g.rng.IntN(2) == 0 && !c.reset && !c.peerClosed {
			g.add(tcpSetReadDeadline(idx, dlPast))
			g.add(tcpRead("read-expired-deadline", idx, 8, 0))
			g.add(tcpSetReadDeadline(idx, dlClear))
		} else {
			g.add(tcpSetReadDeadline(idx, dlClear))
		}
	}
}

func genTCPOps(seed uint64, n int) []op {
	g := &tcpGen{rng: rand.New(rand.NewPCG(seed, 0x7C9)), ln: 0}
	g.ops = tcpCoverageOps()
	g.nConns = tcpCoverageConns
	for range tcpCoverageConns {
		g.conns = append(g.conns, tcpConnState{peer: -1, closed: true})
	}
	// The coverage ladder closed its listener; mint the grammar's own.
	g.add(tcpListen(true))
	g.ln = tcpCoverageLns
	for range n {
		g.step()
	}
	return g.ops
}

// ---------------------------------------------------------------------------
// Domain tests.

func TestDSTConformanceTCP(t *testing.T) {
	allow := tcpAllowlist()
	fired := make(map[string]int)
	for _, seed := range sweepSeeds(t) {
		ops := genTCPOps(seed, 120)
		host := runOpsHost(t, ops)
		sim := runOpsSim(t, seed, ops)
		if d := diffOutcomes(ops, host, sim, allow, fired); d != nil {
			reportDivergence(t, "tcp", seed, ops, d)
			return
		}
	}
	checkAllowlistCoverage(t, allow, fired)
}

// Targeted two-goroutine cases (outcome-set comparison; see doc.go).
func tcpBlockedScenario(kind string) blockedResult {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return blockedResult{-1, "listen:" + errClass(err)}
	}
	defer ln.Close()
	d, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return blockedResult{-1, "dial:" + errClass(err)}
	}
	defer d.Close()
	a, err := ln.Accept()
	if err != nil {
		return blockedResult{-1, "accept:" + errClass(err)}
	}
	defer a.Close()
	switch kind {
	case "peer-close":
		go func() { time.Sleep(50 * time.Millisecond); a.Close() }()
	case "local-close":
		go func() { time.Sleep(50 * time.Millisecond); d.Close() }()
	}
	d.SetReadDeadline(time.Now().Add(guardReady))
	n, rerr := d.Read(make([]byte, 8))
	return blockedResult{n, errClass(rerr)}
}

func TestDSTConformanceTCPCloseDuringRead(t *testing.T) {
	cases := []struct {
		kind  string
		legal func(r blockedResult) bool
		want  string
	}{
		{"peer-close", func(r blockedResult) bool {
			return r.n == 0 && r.err == "EOF"
		}, "n=0, EOF (graceful FIN with nothing unread)"},
		{"local-close", func(r blockedResult) bool {
			return r.n == 0 && r.err == "OpError(read)/ErrClosed:net"
		}, "n=0, net.ErrClosed"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			hostRes := tcpBlockedScenario(tc.kind)
			if !tc.legal(hostRes) {
				t.Errorf("host outcome %+v outside the host-legal set (%s)", hostRes, tc.want)
			}
			var simRes blockedResult
			simulation.Run(1, func() {
				simRes = tcpBlockedScenario(tc.kind)
			})
			if !tc.legal(simRes) {
				t.Errorf("sim outcome %+v outside the host-legal set (%s); host observed %+v", simRes, tc.want, hostRes)
			}
		})
	}
}
