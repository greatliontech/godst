// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

// DSTFSNodeState is the white-box inspector for the durability-monotonicity
// invariant: it reports a simulated node's current vs durable state so tests
// can assert that mutations never advance the durable image and that sync
// alone does. Test-only; the fault feature's crash restoration is the
// eventual production reader of the durable image.
func DSTFSNodeState(name string) (cur, synced string, curEntries, syncedEntries []string, mode, syncedMode FileMode, ok bool) {
	cur, synced, curEntries, syncedEntries, mode, syncedMode, _, _, ok = dstFSNodeStateFull(name)
	return
}

// DSTFSNodeTimes exposes the modTime pair for the metadata-monotonicity pins.
func DSTFSNodeTimes(name string) (modTime, syncedModTime int64, ok bool) {
	_, _, _, _, _, _, mt, smt, ok := dstFSNodeStateFull(name)
	return mt, smt, ok
}

func dstFSNodeStateFull(name string) (cur, synced string, curEntries, syncedEntries []string, mode, syncedMode FileMode, modTime, syncedModTime int64, ok bool) {
	if !dstFSActive() {
		return "", "", nil, nil, 0, 0, 0, 0, false
	}
	dstFS.mu.Lock()
	defer dstFS.mu.Unlock()
	dstFSRoll()
	_, _, node, errno := dstFSResolve(name)
	if errno != nil || node == nil {
		return "", "", nil, nil, 0, 0, 0, 0, false
	}
	sorted := func(m map[string]*dstFSNode) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		for i := 1; i < len(out); i++ {
			for j := i; j > 0 && out[j] < out[j-1]; j-- {
				out[j], out[j-1] = out[j-1], out[j]
			}
		}
		return out
	}
	return string(node.data), string(node.synced),
		sorted(node.entries), sorted(node.syncedEntries),
		node.mode, node.syncedMode,
		node.modTime.UnixNano(), node.syncedModTime.UnixNano(), true
}
