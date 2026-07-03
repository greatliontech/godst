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
	dstPartOpPartition  uint32 = iota + 1 // cut host-pair (a,b), symmetric
	dstPartOpHeal                         // restore host-pair (a,b)
	dstPartOpIsolate                      // cut host a from all others
	dstPartOpHealHost                     // restore host a
	dstFaultOpResetPair                   // reset all conns between hosts a and b
	dstFaultOpResetProc                   // reset all conns owned by process a
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
	default:
		dstApplyPartitionOp(op, a, b)
	}
}

// dstPart is the per-run partition table. Keyed off the run epoch (dstNetEpoch) so
// it resets each run with no explicit teardown. The wake channel is closed and
// remade on every change, to wake reads/dials blocked on a partition; it is made
// lazily under mu from in-bubble callers, so it is a bubble channel for the run.
//
// pairs/isolated map a cut source to the universe BASE time it became cut (its
// cut-start), not merely a present flag: an established conn's read must know
// which bytes ARRIVED before the cut (deliverAt <= cut-start — readable, they sit
// in the receiver's buffer) versus which are in flight or written after it (held
// until heal). Absent = not cut by that source.
var dstPart struct {
	mu       sync.Mutex
	epoch    uint64
	pairs    map[uint64]int64
	isolated map[uint32]int64
	wake     chan struct{}
}

// dstPartKey is the canonical (unordered) key for a host pair.
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
		dstPart.pairs = make(map[uint64]int64)
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
		if _, cut := dstPart.pairs[dstPartKey(a, b)]; !cut {
			dstPart.pairs[dstPartKey(a, b)] = now
		}
	case dstPartOpHeal:
		delete(dstPart.pairs, dstPartKey(a, b))
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

// dstPartitioned reports whether the link between hosts a and b is currently cut
// (either host isolated, or the pair partitioned). The same host is never cut.
func dstPartitioned(a, b uint32) bool {
	_, cut := dstPartCutStart(a, b)
	return cut
}

// dstPartCutStart reports whether the link between hosts a and b is currently cut
// and, if so, the base-time the cut began — the EARLIEST start among the active cut
// sources (pair cut, either host isolated). Earliest is the conservative choice: a
// segment counts as arrived-before-the-cut only if it was delivered before every
// active source's start, so no byte the real link would have held is delivered. The
// same host is never cut.
func dstPartCutStart(a, b uint32) (int64, bool) {
	if a == b {
		return 0, false
	}
	dstPart.mu.Lock()
	defer dstPart.mu.Unlock()
	dstPartRoll()
	start := int64(0)
	cut := false
	consider := func(t int64, ok bool) {
		if ok && (!cut || t < start) {
			start, cut = t, true
		}
	}
	ta, oka := dstPart.isolated[a]
	consider(ta, oka)
	tb, okb := dstPart.isolated[b]
	consider(tb, okb)
	tp, okp := dstPart.pairs[dstPartKey(a, b)]
	consider(tp, okp)
	return start, cut
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
