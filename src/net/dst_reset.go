// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"sort"
	"sync"
)

// Connection-reset targeting. The imperative API in testing/simulation
// (Reset / ResetProcess) drives this through runtime's passthrough into
// dstApplyNetFaultOp, exactly like partition. A reset enumerates the active
// connections matching the victim — a host-pair (all conns between hosts a and b)
// or a process (all conns that process owns either end of) — and tears each down
// via the connection's existing resetConn(). A reset hits BOTH ends, closing each
// transport, so a subsequent read returns the close (ECONNRESET) before draining
// any buffered bytes — in-flight data is dropped, as a real RST discards it.
// Matching is by the connection's host/process attribution (dstConn.localHost/
// remoteHost/localProc/remoteProc), so a reset touches exactly the victim's conns
// (DST-FAULT-VICTIM).

// dstConns is the per-run set of active simulated connections, registered at Dial
// and deregistered on Close/reset. Keyed off the run epoch (dstNetEpoch) so it
// resets each run. Both ends of a connection are registered, each as its own
// dstConn. resetConn tears down the END it is called on — that end's transport
// closes; the shared reset flag only maps the peer's eventual EOF to ECONNRESET
// identity, it does NOT close the peer's transport, so a peer whose own dstConn
// is not reset still DRAINS queued bytes before seeing the error. The no-drain
// RST teardown therefore requires resetting each end's own dstConn — every
// reset matcher reaches both ends of a live conn (the host matcher spares a
// survivor's end whose victim end already closed — see dstResetHost; the
// process-EXIT path deliberately enumerates one end, a per-end graceful
// close). resetConn is idempotent when both ends match: an atomic flag store,
// a sync.Once close, a map delete.
var dstConns struct {
	mu    sync.Mutex
	epoch uint64
	set   map[*dstConn]struct{}
	seq   uint64 // per-run monotonic registration counter (see dstConn.regSeq)
}

func dstConnsRoll() { // caller holds mu
	if e := dstNetEpoch(); e != dstConns.epoch || dstConns.set == nil {
		dstConns.epoch = e
		dstConns.set = make(map[*dstConn]struct{})
		dstConns.seq = 0
	}
}

func dstConnRegister(c *dstConn) {
	dstConns.mu.Lock()
	dstConnsRoll()
	// Stamp a per-run registration sequence so a multi-victim reset can order its
	// victims deterministically. Both ends of one conn register, so each gets its
	// own seq; the reset iterates the whole registry, and ordering by seq is a pure
	// function of Dial order (schedule-determined), never of pointer/heap address.
	dstConns.seq++
	c.regSeq = dstConns.seq
	dstConns.set[c] = struct{}{}
	dstConns.mu.Unlock()
}

func dstConnDeregister(c *dstConn) {
	dstConns.mu.Lock()
	dstConnsRoll()
	delete(dstConns.set, c)
	dstConns.mu.Unlock()
}

// dstResetMatching resets every registered conn for which match reports true. The
// victims are collected under the lock and reset outside it, so resetConn's own
// deregister does not re-enter the lock. They are reset in registration-SEQUENCE
// order — never the registry map's iteration order, which hashes run-varying pointer
// addresses (the fixed -tags dst hash key does not make addresses reproducible), so
// with several victims the wake order of their blocked readers, and thus the whole
// downstream schedule, would diverge across same-seed runs (DST-FAULT-REPLAY).
func dstResetMatching(match func(*dstConn) bool) {
	for _, c := range dstMatchedVictims(match) {
		c.resetConn()
	}
}

// dstMatchedVictims collects the registered conns satisfying match and returns them
// in registration-SEQUENCE order (never the registry map's pointer-address iteration
// order — see dstResetMatching's note). Factored out so the ordering is directly
// testable without resetConn's side effects. Collect-before-teardown is
// load-bearing beyond ordering: a matcher's predicate may read state a sibling
// victim's resetConn mutates (dstResetHost's app-closed skip reads the peer
// end's done channel), so every match must run against the pre-fault snapshot,
// before the first teardown.
func dstMatchedVictims(match func(*dstConn) bool) []*dstConn {
	dstConns.mu.Lock()
	var victims []*dstConn
	dstConnsRoll()
	for c := range dstConns.set {
		if match(c) {
			victims = append(victims, c)
		}
	}
	dstConns.mu.Unlock()
	sort.Slice(victims, func(i, j int) bool { return victims[i].regSeq < victims[j].regSeq })
	return victims
}

// dstLocalBindInUse reports whether a live conn on host already occupies the local
// binding ip:port — a 2-tuple, which is what the real path checks: Go binds an
// explicit LocalAddr without SO_REUSEADDR, so bind(2) fails EADDRINUSE on a local
// addr:port collision even when the destinations differ, and an ephemeral allocator
// must skip a still-live port. Scans the per-run conn registry by the dialer end's
// attribution (localHost + local address). Deterministic (a set-membership test, no
// iteration order observed).
func dstLocalBindInUse(host uint32, ip IP, port int) bool {
	return dstConnBindInUse(host, ip, port, "", false)
}

// dstConnBindInUse is the general conn-side bind probe: does a live conn end on
// host occupy local port? ip nil means any IP at the port (the wildcard-listen
// probe); family "" means either family, otherwise "tcp4"/"tcp6" narrows it.
// dialerEndsOnly counts only dialer-side ends: an ACCEPTED server end inherits
// the listener's SO_REUSEADDR, so it never blocks a new listener — a server
// restarted while its old connections drain must be able to re-bind its port —
// while a dialer's socket carries no SO_REUSEADDR and blocks everything.
// Deterministic (a set-membership test, no iteration order observed).
func dstConnBindInUse(host uint32, ip IP, port int, family string, dialerEndsOnly bool) bool {
	dstConns.mu.Lock()
	defer dstConns.mu.Unlock()
	dstConnsRoll()
	for c := range dstConns.set {
		if c.localHost != host {
			continue
		}
		if dialerEndsOnly && c.acceptState != nil {
			continue
		}
		la, ok := c.local.(*TCPAddr)
		if !ok || la.Port != port {
			continue
		}
		if ip != nil && !la.IP.Equal(ip) {
			continue
		}
		if family != "" && dstAddrFamily("", la.IP) != family {
			continue
		}
		return true
	}
	return false
}

// dstResetHost resets every conn an end of which lives on host h — the machine
// lost power, so each of its sockets emits an RST. Matching EITHER end (as the
// pair and process matchers do) resets both registered dstConns of an affected
// connection, so the surviving peer's own transport end closes too and its
// next read fails ECONNRESET WITHOUT draining — queued and in-flight bytes are
// dropped, as a real RST destroys the receive queue (DST-FAULT-SOUND).
// Matching only the victim's local end would tear down just that end, which
// presents at the peer as a graceful write-close: the peer would drain every
// queued segment — bytes the powered-off machine's teardown destroyed — before
// seeing the reset. A conn between two other hosts has neither end on h and is
// untouched (DST-FAULT-VICTIM).
func dstResetHost(h uint32) {
	dstResetMatching(func(c *dstConn) bool {
		if c.localHost == h {
			return true
		}
		if c.remoteHost != h {
			return false
		}
		// The surviving peer's end of a victim conn. Skip it when the victim's
		// own end was already closed before the power loss (its Close
		// deregistered it): the data and FIN are on the wire, and a powered-off
		// machine emits no packet that could destroy bytes its peer already
		// holds — the peer drains and reads EOF (or the ECONNRESET an earlier
		// exit-reset recorded), exactly as the pre-crash teardown left it. No
		// real RST exists for an app-closed end at power loss
		// (DST-FAULT-SOUND). Every simulated connection is wire-backed (the
		// transport contract); a future non-wire Conn wrapper falls to the
		// reset arm — the uniform crash collapse — never to a silent skip.
		// The predicate is evaluated against the PRE-CRASH snapshot:
		// dstMatchedVictims runs every match before any resetConn, so a
		// victim-side reset in the same crash (which closes exactly the
		// channel tested here) cannot flip a survivor entry to a spurious
		// skip. An interleaved match-and-reset loop would reintroduce the
		// drain whenever the victim end carries the lower regSeq (victim
		// dialed) — see TestDSTCrashHostDropsInFlightBytesVictimDialer.
		if e, ok := c.Conn.(*dstWireEnd); ok && isClosedChan(e.remoteDone) {
			return false
		}
		return true
	})
}

// dstCloseHostListeners closes every listener on host h — the whole port space
// dies with the machine, whichever of its processes bound it.
func dstCloseHostListeners(h uint32) {
	dstCloseListeners(func(l *dstListener) bool { return l.host == h })
}

// dstResetPair resets every conn between hosts a and b (either direction).
func dstResetPair(a, b uint32) {
	key := dstPartKey(a, b)
	dstResetMatching(func(c *dstConn) bool {
		return dstPartKey(c.localHost, c.remoteHost) == key
	})
}

// dstResetProc resets every conn that process p owns an end of (as dialer or as
// the listening/accepting process).
func dstResetProc(p uint32) {
	dstResetMatching(func(c *dstConn) bool {
		return c.localProc == p || c.remoteProc == p
	})
}

// dstCloseProcConns closes the connection ENDS process p owns — the exit-time
// close (the kernel closes a dying process's sockets on normal exit). The
// kernel's own conditional applies per end: a socket whose receive queue holds
// unread data answers the peer with RST (ECONNRESET), otherwise the close FINs
// and the peer drains buffered bytes then reads io.EOF. (Crash resets
// unconditionally — a different fault, see faults.md.) Each end of a
// connection registers its own dstConn whose localProc is that end's owner, so
// matching localProc alone covers every end p owns; the victims close in
// registration-sequence order (DST-FAULT-REPLAY, as dstResetMatching).
func dstCloseProcConns(p uint32) {
	for _, c := range dstMatchedVictims(func(c *dstConn) bool { return c.localProc == p }) {
		// Every simulated connection is wire-backed (the transport contract),
		// so the assertion always holds today; a future non-wire Conn wrapper
		// must extend the unread-inbound probe or it silently degrades the RST
		// arm to a graceful close.
		if e, ok := c.Conn.(*dstWireEnd); ok && e.unreadInbound() {
			c.resetConn()
			continue
		}
		c.Close()
	}
}

func dstCloseProcListeners(p uint32) {
	dstCloseListeners(func(l *dstListener) bool { return l.proc == p })
}

func dstCloseListeners(match func(*dstListener) bool) {
	type victim struct {
		key string
		l   *dstListener
	}
	dstNet.mu.Lock()
	dstNetRoll()
	var victims []victim
	for key, l := range dstNet.listeners {
		if match(l) {
			victims = append(victims, victim{key: key, l: l})
		}
	}
	dstNet.mu.Unlock()
	sort.Slice(victims, func(i, j int) bool { return victims[i].key < victims[j].key })
	seen := make(map[*dstListener]bool)
	for _, v := range victims {
		if seen[v.l] {
			continue
		}
		seen[v.l] = true
		v.l.Close()
	}
}
