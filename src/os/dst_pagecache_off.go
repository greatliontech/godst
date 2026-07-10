// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && !linux

// A page cache is a memfd, which only Linux has; these platforms have no
// simulated mmap either (dst_mmap_off.go), so a node's bytes stay a plain Go
// slice and dstNodeSetSizeLocked is its slice-arithmetic twin. One
// representation per binary either way.

package os

type dstNodeCache struct{}

func dstNodeBackLocked(node *dstFSNode) {}

// dstNodeSetSizeLocked sets a node's length: growth reads as zeros, a shrink
// drops the bytes past the new end so they cannot resurrect if the file grows
// back. Caller holds dstFS.mu.
func dstNodeSetSizeLocked(node *dstFSNode, size int64) {
	switch {
	case size <= int64(len(node.data)):
		node.data = node.data[:size]
	case size <= int64(cap(node.data)):
		// Re-extending within capacity: zero the gap, or bytes from a previous
		// longer state resurrect (truncate-down then extend).
		old := len(node.data)
		node.data = node.data[:size]
		clear(node.data[old:])
	default:
		// append for amortized growth (an exact-size make would copy O(n^2)
		// over append-heavy workloads).
		node.data = append(node.data, make([]byte, size-int64(len(node.data)))...)
	}
}

func dstNodeReleaseRunLocked() {}
