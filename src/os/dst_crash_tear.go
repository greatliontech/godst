// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import (
	"cmp"
	"slices"
	_ "unsafe" // for go:linkname
)

// Crash tear: what a power loss actually leaves on the platters.
//
// The all-or-nothing restore (everything unsynced is lost) is one legal outcome
// of the durability contract; the contract permits many, and a crash-consistent
// database is only exercised by the others. This file adds them, under the
// contract's own bound: "unsynced data and entries MAY be lost, unsynced content
// MAY be torn at arbitrary byte granularity, drawn from the fault RNG."
//
// The model is the page cache, not a write log — and that distinction is the
// soundness argument. Writeback flushes PAGES, and a dirty page carries the
// CURRENT bytes of every byte in it: if two writes touched a byte before the
// crash, the page holds the later one. So replaying "a subset of the write
// history, reordered" would persist an older write's bytes for a byte a newer
// write covered — a state no page cache can produce (the false-positive class
// DST-FAULT-SOUND forbids). What a real crash produces instead is, per page:
// the durable content, or the current content, or — for a sector caught in
// flight — a byte-granular mix of the two. That is exactly the choice below, so
// the "arbitrary subset of unsynced writes" the contract asks for is realized as
// an arbitrary subset of dirty PAGES, and "reorder" is unobservable within a
// file (a file's image is a set of pages, not a sequence).
//
// Across files and names, ordering IS observable, and it falls out for free:
// each node's pages and each directory's unsynced entries draw independently, so
// a crash can persist a file's data and lose its name, or persist one file of a
// two-file transaction — the interleavings a crash-consistency bug hides in.
//
// Every draw comes from the fault RNG (dstFaultRand), which is rooted per bubble
// from the run seed and stream-isolated from the scheduling RNG, so the tear is
// a deterministic function of (seed, fault schedule) — DST-FAULT-REPLAY — and
// the draw ORDER is fixed by iterating pages by index and entries by sorted
// name, never by map iteration order.

//go:linkname dstFaultRandN runtime.dstFaultRandN
func dstFaultRandN(n int64) int64

// dstPageSize is the simulated writeback granularity. Fixed, not the host's
// page size: page geometry is machine state (see the mmap page-size note).
const dstPageSize = 4096

// dstCrashTear reports whether host crashes tear rather than losing every
// unsynced byte. Set once per run from Options.CrashTear before the bubble
// starts, read only inside it.
var dstCrashTear bool

//go:linkname dstSetCrashTear
func dstSetCrashTear(on bool) { dstCrashTear = on }

// dstCrashTearEnabled reports the policy, so an exploration can record it in the
// failure it reports (its Replay restores it). Reached via //go:linkname.
//
//go:linkname dstCrashTearEnabled
func dstCrashTearEnabled() bool { return dstCrashTear }

// dstTearPageOutcome is one page's fate.
type dstTearPageOutcome int

const (
	dstPageDurable dstTearPageOutcome = iota // the write never reached the platter
	dstPageCurrent                           // the page was written back whole
	dstPageTorn                              // a sector caught in flight: a byte-granular mix
)

// dstTearFileLocked returns the content a power loss leaves for a file whose
// durable image is synced and whose page cache holds data. Caller holds dstFS.mu.
func dstTearFileLocked(synced, data []byte) []byte {
	// The file's length is itself unsynced state when the file grew or was
	// truncated: one draw decides whether the size change reached the inode.
	// (A grown file whose tail pages are all lost still reads as zeros there —
	// the sparse tail a real crash leaves.)
	size := len(synced)
	if len(data) != len(synced) && dstFaultRandN(2) == 1 {
		size = len(data)
	}
	out := make([]byte, size)
	copy(out, synced) // durable bytes are stable, always
	for start := 0; start < size; start += dstPageSize {
		end := min(start+dstPageSize, size)
		// Only bytes the page cache actually HOLDS can be written back. Where
		// the current file has ended (an unsynced truncate whose size change did
		// not land), the platter still carries the durable bytes: the blocks
		// were never freed, and nothing overwrote them. Treating the absent
		// bytes as zeros and letting them "land" would erase durable data — a
		// state no crash produces, and the one the truncate-down sweep caught.
		curEnd := min(end, len(data))
		if curEnd <= start {
			continue // wholly past the live file: durable bytes stand
		}
		cur := data[start:curEnd]
		dur := dstPageSlice(synced, start, curEnd)
		if slices.Equal(cur, dur) {
			continue // nothing unsynced in this page
		}
		switch dstTearPageOutcome(dstFaultRandN(3)) {
		case dstPageDurable:
			// leave out[start:curEnd] as the durable bytes already copied
		case dstPageCurrent:
			copy(out[start:curEnd], cur)
		case dstPageTorn:
			// A sector was being written when the power went: bytes before the
			// split landed, the rest did not. This is the physical torn-write
			// shape (bytes go out in order; the cut lands somewhere), so it is a
			// strict SUBSET of the arbitrary byte mixes the contract permits —
			// sound, and the sound direction to be incomplete in.
			split := int(dstFaultRandN(int64(curEnd-start) + 1))
			copy(out[start:start+split], cur[:split])
		}
	}
	return out
}

// dstPageSlice returns b[start:end] padded with zeros where b is shorter — the
// bytes a page of the file holds, whether or not the file reaches that far.
func dstPageSlice(b []byte, start, end int) []byte {
	if start >= len(b) {
		return make([]byte, end-start)
	}
	if end <= len(b) {
		return b[start:end]
	}
	out := make([]byte, end-start)
	copy(out, b[start:])
	return out
}

// dstTearEntriesLocked returns the directory entries a power loss leaves. Names
// whose presence is already durable (in syncedEntries, still linked) stay;
// unsynced changes — a create not yet in the durable set, a removal still in it —
// each independently either reached the platter or did not. Draw order is the
// sorted name order, never the map's. Caller holds dstFS.mu.
func dstTearEntriesLocked(node *dstFSNode) map[string]*dstFSNode {
	names := make([]string, 0, len(node.entries)+len(node.syncedEntries))
	for name := range node.syncedEntries {
		names = append(names, name)
	}
	for name := range node.entries {
		if _, durable := node.syncedEntries[name]; !durable {
			names = append(names, name)
		}
	}
	slices.SortFunc(names, func(a, b string) int { return cmp.Compare(a, b) })

	out := make(map[string]*dstFSNode, len(names))
	for _, name := range names {
		live, inLive := node.entries[name]
		durable, inDurable := node.syncedEntries[name]
		switch {
		case inLive && inDurable && live == durable:
			out[name] = durable // durable name, unchanged
		case inLive && !inDurable:
			// A create whose parent was never fsynced: it may or may not be there.
			if dstFaultRandN(2) == 1 {
				out[name] = live
			}
		case !inLive && inDurable:
			// A remove whose parent was never fsynced: it may or may not have
			// happened. Keeping the durable node resurrects it (see the restore).
			if dstFaultRandN(2) == 1 {
				out[name] = durable
			}
		case inLive && inDurable:
			// The name was rebound (rename-over) without a parent fsync: the
			// platter holds one of the two inodes.
			if dstFaultRandN(2) == 1 {
				out[name] = live
			} else {
				out[name] = durable
			}
		}
	}
	return out
}
