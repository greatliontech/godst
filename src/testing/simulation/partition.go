// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

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
	partOpCrashProcConns
)

// Partition cuts the network link between hosts a and b (symmetric). Connections
// between them — in either direction — are blackholed and new dials across the cut
// block, until HealLink(a, b) (or UnisolateHost on either) restores the link. Like every
// victim-naming fault API, it panics during a run on an undeclared host or process
// name and is a no-op outside a run.
func Partition(a, b string) {
	withBubbleFaultCaller("Partition", func() { dstNetPartitionOp(partOpPartition, lookupHost(a), lookupHost(b)) })
}

// HealLink restores the link between hosts a and b cut by Partition,
// PartitionOneWay, or PartitionRefuse.
func HealLink(a, b string) {
	withBubbleFaultCaller("HealLink", func() { dstNetPartitionOp(partOpHeal, lookupHost(a), lookupHost(b)) })
}

// IsolateHost cuts host from every other host at once (a full network partition of one
// node), until UnisolateHost(host) restores it.
func IsolateHost(host string) {
	withBubbleFaultCaller("IsolateHost", func() { dstNetPartitionOp(partOpIsolate, lookupHost(host), 0) })
}

// UnisolateHost restores a host isolated by IsolateHost.
func UnisolateHost(host string) {
	withBubbleFaultCaller("UnisolateHost", func() { dstNetPartitionOp(partOpHealHost, lookupHost(host), 0) })
}

// PartitionOneWay cuts ONLY the direction from→to (asymmetric): from's writes never
// reach to (and to's dials of from time out), while to→from still delivers. This is a
// real failure mode — asymmetric routing, a firewall dropping one direction — and a
// classic distributed-systems adversary. HealLink(from, to) restores it (HealLink clears both
// directions). Same victim-naming rules as Partition.
func PartitionOneWay(from, to string) {
	withBubbleFaultCaller("PartitionOneWay", func() { dstNetPartitionOp(partOpPartitionOneWay, lookupHost(from), lookupHost(to)) })
}

// PartitionRefuse cuts the link between hosts a and b (symmetric) in REFUSE mode: a
// Dial across the cut fails fast with ECONNREFUSED (the peer answers RST — "peer down"
// semantics), where Partition instead blackholes the dial (the SYN is dropped, the
// dial blocks). Both are real TCP outcomes a SUT tests against; the choice is the
// SUT's. Established connections behave as under Partition (reads block, writes buffer
// then block); the mode governs only new connects. HealLink(a, b) restores it. The refuse
// returns immediately, not after a half-RTT SYN traversal — the same recorded timing
// simplification a direct ECONNREFUSED to a declared-but-unlistened port carries. If
// the pair is ALSO isolated or blackhole-cut, the drop wins and the dial blackholes
// (a dropped SYN elicits no RST), never a false ECONNREFUSED.
func PartitionRefuse(a, b string) {
	withBubbleFaultCaller("PartitionRefuse", func() { dstNetPartitionOp(partOpPartitionRefuse, lookupHost(a), lookupHost(b)) })
}

// ResetLink injects ECONNRESET on every active connection between hosts a and b (in
// either direction), modeling a transient that tears those flows down — a real RST
// from a middlebox. Both ends survive and receive the RST as a real kernel would:
// each drains the bytes already delivered to its receive queue, then reads fail
// ECONNRESET and writes fail immediately; in-flight buffered bytes are dropped.
func ResetLink(a, b string) {
	withBubbleFaultCaller("ResetLink", func() { dstNetPartitionOp(partOpResetPair, lookupHost(a), lookupHost(b)) })
}

// ResetProcess injects ECONNRESET on every active connection owned by any live
// invocation of process proc on host (pass host == "" for a process declared on
// the unnamed root host) — modeling that process's sockets being torn down as a
// lighter-weight precursor to CrashProcess. The process itself stays alive, so
// every end — its own included — drains its delivered bytes before failing
// ECONNRESET; in-flight bytes are dropped.
func ResetProcess(host, proc string) {
	withBubbleFaultCaller("ResetProcess", func() {
		var hid uint32
		if host != "" {
			hid = lookupHost(host)
			if hid == 0 {
				return
			}
		}
		p := lookupProc(hid, proc)
		if p == 0 {
			return
		}
		for _, slot := range activeProcSlots(p) {
			dstNetPartitionOp(partOpResetProc, slot, 0)
		}
	})
}
