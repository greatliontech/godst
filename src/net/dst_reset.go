// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import "sync"

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
}

func dstConnsRoll() { // caller holds mu
	if e := dstNetEpoch(); e != dstConns.epoch || dstConns.set == nil {
		dstConns.epoch = e
		dstConns.set = make(map[*dstConn]struct{})
	}
}

func dstConnRegister(c *dstConn) {
	dstConns.mu.Lock()
	dstConnsRoll()
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
// deregister does not re-enter the lock.
func dstResetMatching(match func(*dstConn) bool) {
	dstConns.mu.Lock()
	dstConnsRoll()
	var victims []*dstConn
	for c := range dstConns.set {
		if match(c) {
			victims = append(victims, c)
		}
	}
	dstConns.mu.Unlock()
	for _, c := range victims {
		c.resetConn()
	}
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
