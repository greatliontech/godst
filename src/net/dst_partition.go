// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package net

import (
	"sync"
	_ "unsafe" // for go:linkname
)

// Network partition targeting. The imperative API lives in testing/simulation
// (Partition/Heal/Isolate/HealHost — it owns host-name interning); it drives this
// table through runtime's always-linked passthrough (runtime.dstNetPartitionOp ->
// the hook registered below), so simulation needs no fragile linkname into net
// (net is absent from a simulation binary unless the SUT uses it). The table — a
// set of cut host-pairs plus a set of isolated hosts — is consulted at Dial (a
// blackhole connect blocks until heal/ctx) and at the wire's read (a blackhole
// established conn blocks reads while cut; writes keep buffering on the wire and
// flush in order on heal — the sound buffer-and-recover model). It is keyed by the
// connection's host attribution (dstConn.localHost/remoteHost), so a partition
// touches exactly the targeted pair's cross-host conns, none else (DST-FAULT-VICTIM).

//go:linkname dstSetNetPartitionHook runtime.dstSetNetPartitionHook
func dstSetNetPartitionHook(fn func(op, a, b uint32))

// Net-fault op codes — net's contract with testing/simulation's targeting API,
// which passes the same codes through runtime.dstNetPartitionOp. b is ignored for
// the host/process-level ops.
const (
	dstPartOpPartition           uint32 = iota + 1 // cut host-pair (a,b), symmetric, blackhole connect
	dstPartOpHeal                                  // restore host-pair (a,b), both directions
	dstPartOpIsolate                               // cut host a from all others
	dstPartOpHealHost                              // restore host a
	dstFaultOpResetPair                            // reset all conns between hosts a and b
	dstFaultOpResetProc                            // reset all conns owned by process a
	dstPartOpPartitionOneWay                       // cut ONLY direction a→b (asymmetric), blackhole connect
	dstPartOpPartitionRefuse                       // cut host-pair (a,b), symmetric, refuse (ECONNREFUSED) connect
	dstFaultOpCloseProcListeners                   // close all listeners owned by process a (crash teardown)
)

func init() { dstSetNetPartitionHook(dstApplyNetFaultOp) }

// dstApplyNetFaultOp dispatches a net-fault targeting op (from testing/simulation
// via runtime's passthrough) to the partition table or the connection registry.
func dstApplyNetFaultOp(op, a, b uint32) {
	switch op {
	case dstFaultOpResetPair:
		dstResetPair(a, b)
	case dstFaultOpResetProc:
		dstResetProc(a)
	case dstFaultOpCloseProcListeners:
		dstCloseProcListeners(a)
	default:
		dstApplyPartitionOp(op, a, b)
	}
}

// dstPart is the per-run partition table. Keyed off the run epoch (dstNetEpoch) so
// it resets each run with no explicit teardown. The wake channel is closed and
// remade on every change, to wake reads/dials blocked on a partition; it is made
// lazily under mu from in-bubble callers, so it is a bubble channel for the run.
//
// dirs/isolated map a cut source to the universe BASE time it became cut (its
// cut-start), not merely a present flag: an established conn's read must know
// which bytes ARRIVED before the cut (deliverAt <= cut-start — readable, they sit
// in the receiver's buffer) versus which are in flight or written after it (held
// until heal). Absent = not cut by that source. dirs is keyed by ORDERED direction
// (from→to): a symmetric Partition cuts both a→b and b→a; a one-directional
// PartitionOneWay(from,to) cuts only from→to (from's writes never reach to, while
// to→from still flows). isolated is symmetric (a host cut from all others, both ways).
var dstPart struct {
	mu       sync.Mutex
	epoch    uint64
	dirs     map[uint64]dstCut // ordered from→to → cut record
	isolated map[uint32]int64  // host → cut-start (isolated from all others, both directions)
	wake     chan struct{}
}

// dstCut is one directional cut: the base time it began and the connect mode a Dial
// across it observes — blackhole (the SYN is dropped, the dial blocks) or refuse
// (the peer answers RST, the dial fails ECONNREFUSED). The mode governs only connect;
// an established conn's read/write hold is the same for both (the link is cut).
type dstCut struct {
	start  int64
	refuse bool
}

// dstDirKey is the key for an ORDERED direction from→to (no canonicalization — a
// one-directional cut must be distinguishable from its reverse).
func dstDirKey(from, to uint32) uint64 {
	return uint64(from)<<32 | uint64(to)
}

// dstPartKey is the canonical (unordered) key for a host pair — used to match a
// conn to a host pair regardless of which end is local (the Reset fault, dst_reset.go).
func dstPartKey(a, b uint32) uint64 {
	if a > b {
		a, b = b, a
	}
	return uint64(a)<<32 | uint64(b)
}

// dstPartRoll resets the table when the run epoch advances. Caller holds mu.
func dstPartRoll() {
	if e := dstNetEpoch(); e != dstPart.epoch || dstPart.wake == nil {
		dstPart.epoch = e
		dstPart.dirs = make(map[uint64]dstCut)
		dstPart.isolated = make(map[uint32]int64)
		dstPart.wake = make(chan struct{})
	}
}

// dstApplyPartitionOp is the hook runtime invokes for simulation.Partition etc. It
// mutates the table and wakes everything blocked on a partition by closing and
// remaking the wake channel. A cut records the current base time as its cut-start
// (only when it was not already cut by that source, so a redundant re-cut does not
// move the boundary); a heal removes the source.
func dstApplyPartitionOp(op, a, b uint32) {
	now := dstBaseNanos()
	dstPart.mu.Lock()
	dstPartRoll()
	switch op {
	case dstPartOpPartition:
		dstCutDir(a, b, now, false) // symmetric blackhole: both directions
		dstCutDir(b, a, now, false)
	case dstPartOpPartitionRefuse:
		dstCutDir(a, b, now, true) // symmetric refuse: both directions
		dstCutDir(b, a, now, true)
	case dstPartOpPartitionOneWay:
		dstCutDir(a, b, now, false) // asymmetric blackhole: only a→b
	case dstPartOpHeal:
		delete(dstPart.dirs, dstDirKey(a, b))
		delete(dstPart.dirs, dstDirKey(b, a))
	case dstPartOpIsolate:
		if _, cut := dstPart.isolated[a]; !cut {
			dstPart.isolated[a] = now
		}
	case dstPartOpHealHost:
		delete(dstPart.isolated, a)
	}
	close(dstPart.wake)
	dstPart.wake = make(chan struct{})
	dstPart.mu.Unlock()
}

// dstCutDir records a cut on direction from→to. First-cut-wins: a redundant re-cut
// is a no-op (it must not move the cut-start boundary, nor flip the mode of an
// existing cut). Caller holds mu.
func dstCutDir(from, to uint32, now int64, refuse bool) {
	if _, cut := dstPart.dirs[dstDirKey(from, to)]; !cut {
		dstPart.dirs[dstDirKey(from, to)] = dstCut{start: now, refuse: refuse}
	}
}

// dstPartCutStartDir reports whether the DIRECTED link from→to is currently cut and,
// if so, the base-time the cut began — the EARLIEST start among the active sources
// (the from→to directional cut, or EITHER endpoint isolated — isolation cuts every
// direction) — and whether ANY active source DROPS packets (blackhole) as opposed to
// a pure refuse-mode cut (which emits an RST). Earliest is the conservative choice: a
// segment counts as arrived-before-the-cut only if it was delivered before every
// active source's start, so no byte the real link would have held is delivered.
// blackhole is OR-ed across sources: an isolated endpoint or a blackhole-mode cut
// drops, so if either is active on this direction no RST can escape it (isolation is
// always blackhole). The same host is never cut.
func dstPartCutStartDir(from, to uint32) (start int64, cut, blackhole bool) {
	if from == to {
		return 0, false, false
	}
	dstPart.mu.Lock()
	defer dstPart.mu.Unlock()
	dstPartRoll()
	consider := func(t int64, ok, drops bool) {
		if !ok {
			return
		}
		if !cut || t < start {
			start = t
		}
		cut = true
		if drops {
			blackhole = true
		}
	}
	ta, oka := dstPart.isolated[from]
	consider(ta, oka, true) // an isolated endpoint drops packets — no RST can escape
	tb, okb := dstPart.isolated[to]
	consider(tb, okb, true)
	if c, ok := dstPart.dirs[dstDirKey(from, to)]; ok {
		consider(c.start, true, !c.refuse) // blackhole-mode cut drops; refuse-mode cut emits RST
	}
	return
}

// dstPartitionedDir reports whether the DIRECTED link from→to is currently cut.
func dstPartitionedDir(from, to uint32) bool {
	_, cut, _ := dstPartCutStartDir(from, to)
	return cut
}

// dstDialCut reports whether a dial from dialer to target is cut — its SYN
// (dialer→target) OR the returning SYN-ACK (target→dialer) is dropped, either of
// which prevents the handshake — and, if so, whether the mode is refuse (the dial
// fails ECONNREFUSED) rather than blackhole (blocks until heal/deadline/horizon). The
// dial refuses ONLY when the cut is purely refuse-mode: any blackhole/drop source (an
// isolated endpoint, or a blackhole-mode cut) on EITHER handshake direction swallows
// the peer's RST, so the connect blackholes (times out) instead — a real dropped-
// packet path never delivers an ECONNREFUSED, and reporting one would be a sim-only
// false failure (the false-positive class Soundness forbids).
func dstDialCut(dialer, target uint32) (cut, refuse bool) {
	_, c1, bh1 := dstPartCutStartDir(dialer, target)
	_, c2, bh2 := dstPartCutStartDir(target, dialer)
	cut = c1 || c2
	refuse = cut && !bh1 && !bh2
	return
}

// dstPartWakeCh returns the channel closed on the next partition change, so a
// blocked Dial/Read can wait for a heal. Fetch it BEFORE re-checking
// dstPartitioned: a change after the fetch closes the fetched channel, so there is
// no missed wakeup.
func dstPartWakeCh() <-chan struct{} {
	dstPart.mu.Lock()
	defer dstPart.mu.Unlock()
	dstPartRoll()
	return dstPart.wake
}
