// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import _ "unsafe" // for go:linkname

// Network partition targeting. These imperative calls cut and restore links in the
// simulated network during a run, so a SUT can test how it tolerates partitions.
// They name hosts (the same names passed to Host); the call interns the name to the
// host id the network keys connections by, and drives the partition through
// runtime's always-linked passthrough into net (so simulation needs no direct
// dependency on net). Partitioning is at flow granularity. The default Partition is
// symmetric and blackholes: a cut link drops new dials' SYNs (they block until the
// link heals or the dial's context/deadline/retransmit-horizon expires) and holds
// established connections (reads block, writes buffer then block); a heal resumes
// in-order delivery with no byte loss (DST-FAULT-SOUND). Two mode variants: PartitionRefuse
// makes dials fail ECONNREFUSED fast (peer-down) instead of blackholing, and
// PartitionOneWay cuts a single direction (from→to) while the reverse still flows.
// Calls outside a run, or in a run whose binary does not link net, are no-ops (there
// is no network to cut). Call them from within a Run: during an active run a call
// from a goroutine outside the run's bubble panics (the caller-position rule,
// docs/dst/faults.md "Fault callers fail loud too").

//go:linkname dstNetPartitionOp runtime.dstNetPartitionOp
func dstNetPartitionOp(op, a, b uint32)

//go:linkname dstNetHostDead runtime.dstNetHostDead
func dstNetHostDead(host uint32) bool

//go:linkname dstInSimBubble runtime.dstInSimBubble
func dstInSimBubble() bool

// Net-fault op codes — must match net's dst_partition.go.
const (
	partOpPartition uint32 = iota + 1
	partOpHeal
	partOpIsolate
	partOpHealHost
	partOpResetPair
	partOpResetProc
	partOpPartitionOneWay
	partOpPartitionRefuse
	partOpCloseProcListeners
	partOpCloseProcConns
	partOpResetHost
	partOpCloseHostListeners
	partOpHostDown
	partOpHostUp
)

// Partition cuts the network link between hosts a and b (symmetric). Connections
// between them — in either direction — are blackholed and new dials across the cut
// block, until Heal(a, b) (or HealHost on either) restores the link. Like every
// victim-naming fault API, it panics during a run on an undeclared host or process
// name and is a no-op outside a run.
func Partition(a, b string) {
	requireBubbleFaultCaller("Partition")
	dstNetPartitionOp(partOpPartition, lookupHost(a), lookupHost(b))
}

// Heal restores the link between hosts a and b cut by Partition.
func Heal(a, b string) {
	requireBubbleFaultCaller("Heal")
	dstNetPartitionOp(partOpHeal, lookupHost(a), lookupHost(b))
}

// Isolate cuts host from every other host at once (a full network partition of one
// node), until HealHost(host) restores it.
func Isolate(host string) {
	requireBubbleFaultCaller("Isolate")
	dstNetPartitionOp(partOpIsolate, lookupHost(host), 0)
}

// HealHost restores a host isolated by Isolate.
func HealHost(host string) {
	requireBubbleFaultCaller("HealHost")
	dstNetPartitionOp(partOpHealHost, lookupHost(host), 0)
}

// PartitionOneWay cuts ONLY the direction from→to (asymmetric): from's writes never
// reach to (and to's dials of from time out), while to→from still delivers. This is a
// real failure mode — asymmetric routing, a firewall dropping one direction — and a
// classic distributed-systems adversary. Heal(from, to) restores it (Heal clears both
// directions). Same victim-naming rules as Partition.
func PartitionOneWay(from, to string) {
	requireBubbleFaultCaller("PartitionOneWay")
	dstNetPartitionOp(partOpPartitionOneWay, lookupHost(from), lookupHost(to))
}

// PartitionRefuse cuts the link between hosts a and b (symmetric) in REFUSE mode: a
// Dial across the cut fails fast with ECONNREFUSED (the peer answers RST — "peer down"
// semantics), where Partition instead blackholes the dial (the SYN is dropped, the
// dial blocks). Both are real TCP outcomes a SUT tests against; the choice is the
// SUT's. Established connections behave as under Partition (reads block, writes buffer
// then block); the mode governs only new connects. Heal(a, b) restores it. The refuse
// returns immediately, not after a half-RTT SYN traversal — the same recorded timing
// simplification a direct ECONNREFUSED to a declared-but-unlistened port carries. If
// the pair is ALSO isolated or blackhole-cut, the drop wins and the dial blackholes
// (a dropped SYN elicits no RST), never a false ECONNREFUSED.
func PartitionRefuse(a, b string) {
	requireBubbleFaultCaller("PartitionRefuse")
	dstNetPartitionOp(partOpPartitionRefuse, lookupHost(a), lookupHost(b))
}

// Reset injects ECONNRESET on every active connection between hosts a and b (in
// either direction), modeling a transient that tears those flows down — a real RST
// from a peer crash or a middlebox. Both ends of each connection observe
// ECONNRESET on their next operation; any in-flight buffered bytes are dropped.
func Reset(a, b string) {
	requireBubbleFaultCaller("Reset")
	dstNetPartitionOp(partOpResetPair, lookupHost(a), lookupHost(b))
}

// ResetProcess injects ECONNRESET on every active connection process p owns an end
// of (as dialer or as the listening process) — modeling that process's sockets
// being torn down (e.g. as a lighter-weight precursor to the process crash fault).
func ResetProcess(p string) {
	requireBubbleFaultCaller("ResetProcess")
	dstNetPartitionOp(partOpResetProc, lookupProc(p), 0)
}
