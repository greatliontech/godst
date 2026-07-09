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
// resets each run. Both ends of a connection are registered, so a reset finds the
// conn through whichever end is still live; resetConn shares one reset flag, so
// resetting either end tears down both (idempotent if both match).
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
// testable without resetConn's side effects.
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
	dstConns.mu.Lock()
	defer dstConns.mu.Unlock()
	dstConnsRoll()
	for c := range dstConns.set {
		if c.localHost != host {
			continue
		}
		la, ok := c.local.(*TCPAddr)
		if !ok || la.Port != port {
			continue
		}
		if la.IP.Equal(ip) {
			return true
		}
	}
	return false
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

func dstCloseProcListeners(p uint32) {
	type victim struct {
		key string
		l   *dstListener
	}
	dstNet.mu.Lock()
	dstNetRoll()
	var victims []victim
	for key, l := range dstNet.listeners {
		if l.proc == p {
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
