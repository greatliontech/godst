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
var dstPart struct {
	mu       sync.Mutex
	epoch    uint64
	pairs    map[uint64]bool
	isolated map[uint32]bool
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
		dstPart.pairs = make(map[uint64]bool)
		dstPart.isolated = make(map[uint32]bool)
		dstPart.wake = make(chan struct{})
	}
}

// dstApplyPartitionOp is the hook runtime invokes for simulation.Partition etc. It
// mutates the table and wakes everything blocked on a partition by closing and
// remaking the wake channel.
func dstApplyPartitionOp(op, a, b uint32) {
	dstPart.mu.Lock()
	dstPartRoll()
	switch op {
	case dstPartOpPartition:
		dstPart.pairs[dstPartKey(a, b)] = true
	case dstPartOpHeal:
		delete(dstPart.pairs, dstPartKey(a, b))
	case dstPartOpIsolate:
		dstPart.isolated[a] = true
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
	if a == b {
		return false
	}
	dstPart.mu.Lock()
	defer dstPart.mu.Unlock()
	dstPartRoll()
	return dstPart.isolated[a] || dstPart.isolated[b] || dstPart.pairs[dstPartKey(a, b)]
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
