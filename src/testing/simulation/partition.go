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
// dependency on net). Partitioning is symmetric and at flow granularity: a cut
// link refuses new dials (they block until the link heals or the dial's
// context/deadline expires) and blackholes established connections (reads block,
// writes keep buffering); a heal resumes in-order delivery with no byte loss
// (DST-FAULT-SOUND). Calls outside a run, or in a run whose binary does not link
// net, are no-ops (there is no network to cut). Call them from within a Run.

//go:linkname dstNetPartitionOp runtime.dstNetPartitionOp
func dstNetPartitionOp(op, a, b uint32)

// Partition op codes — must match net's dst_partition.go.
const (
	partOpPartition uint32 = iota + 1
	partOpHeal
	partOpIsolate
	partOpHealHost
)

// Partition cuts the network link between hosts a and b (symmetric). Connections
// between them — in either direction — are blackholed and new dials across the cut
// block, until Heal(a, b) (or HealHost on either) restores the link.
func Partition(a, b string) {
	dstNetPartitionOp(partOpPartition, internHost(a), internHost(b))
}

// Heal restores the link between hosts a and b cut by Partition.
func Heal(a, b string) {
	dstNetPartitionOp(partOpHeal, internHost(a), internHost(b))
}

// Isolate cuts host from every other host at once (a full network partition of one
// node), until HealHost(host) restores it.
func Isolate(host string) {
	dstNetPartitionOp(partOpIsolate, internHost(host), 0)
}

// HealHost restores a host isolated by Isolate.
func HealHost(host string) {
	dstNetPartitionOp(partOpHealHost, internHost(host), 0)
}
