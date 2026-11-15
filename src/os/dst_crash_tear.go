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

// dstCloneImageWithRot clones synced clipped to size and folds rot XOR masks
// into the affected 4 KB pages, preserving COW sharing on untouched pages.
func dstCloneImageWithRot(synced dstDurableImage, size int64, rot map[int64]byte) map[int64]dstDurablePage {
	var pages map[int64]dstDurablePage
	setPage := func(pg int64, p dstDurablePage) {
		if pages == nil {
			pages = make(map[int64]dstDurablePage)
		}
		pages[pg] = p
	}
	for pg, p := range synced.pages {
		pStart := pg * dstPageSize
		if pStart >= size {
			continue
		}
		limit := int(min(int64(dstPageSize), size-pStart))
		if clipped := dstDurablePageClipped(p, limit); clipped != nil {
			setPage(pg, clipped)
		}
	}
	for off, mask := range rot {
		if off >= size || mask == 0 {
			continue
		}
		pg := off / dstPageSize
		var buf [dstPageSize]byte
		if old := pages[pg]; old != nil {
			buf = *old
		}
		buf[off%dstPageSize] ^= mask
		if dstBytesAllZero(buf[:]) {
			delete(pages, pg)
		} else {
			setPage(pg, &buf)
		}
	}
	return pages
}

// dstTearFileLocked returns the sparse durable image a power loss leaves for
// node. rot is the file's silent-corruption mask set (dstFSNode.rot): it
// modifies the PLATTER baseline, so a page writeback landed (current or the
// torn prefix) overwrites its rot — the sector was rewritten — while a clean
// page, a lost page, and the torn suffix keep it. The landed/lost/torn draws
// compare against the UNROTTED durable bytes in ascending page order, skipping
// hole pages absent from both memfd (SEEK_DATA) and synced.pages (where cur and
// dur are both all-zero and therefore never draw). Caller holds dstFS.mu.
func dstTearFileLocked(node *dstFSNode) dstDurableImage {
	synced := node.synced
	dataLen := int64(len(node.data))
	size := synced.size
	if dataLen != synced.size {
		if dataLen > synced.size {
			first := (synced.size/dstPageSize + 1) * dstPageSize
			boundaries := int64(0)
			if first < dataLen {
				boundaries = (dataLen-1-first)/dstPageSize + 1
			}
			switch k := dstFaultRandN(2 + boundaries); {
			case k == 0:
				// durable size stands
			case k == 1+boundaries:
				size = dataLen
			default:
				size = first + (k-1)*dstPageSize
			}
		} else if dstFaultRandN(2) == 1 {
			size = dataLen
		}
	}
	pages := dstCloneImageWithRot(synced, size, node.rot)
	setPage := func(pg int64, p dstDurablePage) {
		if pages == nil {
			pages = make(map[int64]dstDurablePage)
		}
		pages[pg] = p
	}
	scanEnd := min(size, dataLen)
	if scanEnd > 0 {
		maxCandPg := (scanEnd + dstPageSize - 1) / dstPageSize
		var cands []int64
		if node.pc != nil {
			for off := int64(0); off < scanEnd; {
				d, h, ok := dstPageCacheNextData(node.pc.fd, off, scanEnd)
				if !ok {
					break
				}
				p0 := d / dstPageSize
				p1 := (h + dstPageSize - 1) / dstPageSize
				for pg := p0; pg < p1; pg++ {
					cands = append(cands, pg)
				}
				off = h
			}
		}
		for pg := range synced.pages {
			if pg < maxCandPg {
				cands = append(cands, pg)
			}
		}
		slices.Sort(cands)
		cands = slices.Compact(cands)
		for _, pg := range cands {
			start := pg * dstPageSize
			end := min(start+dstPageSize, size)
			curEnd := min(end, dataLen)
			if curEnd <= start {
				continue
			}
			pageLen := int(end - start)
			cur := node.data[start:curEnd]
			durLen := int(max(0, min(end, synced.size)-start))
			if dstDurablePageMatchesWindow(synced.pages[pg], durLen, pageLen, cur) {
				continue
			}
			if node.isPageDroppedLocked(pg) {
				continue
			}
			switch dstTearPageOutcome(dstFaultRandN(3)) {
			case dstPageDurable:
				// leave pages[pg] as the durable (+ rot) bytes already cloned
			case dstPageCurrent:
				var buf [dstPageSize]byte
				copy(buf[:len(cur)], cur)
				if dstBytesAllZero(buf[:]) {
					delete(pages, pg)
				} else {
					setPage(pg, &buf)
				}
			case dstPageTorn:
				split := int(dstFaultRandN(int64(pageLen) + 1))
				var buf [dstPageSize]byte
				if old := pages[pg]; old != nil {
					buf = *old
				}
				nCopy := min(split, len(cur))
				copy(buf[:nCopy], cur[:nCopy])
				clear(buf[nCopy:split])
				if dstBytesAllZero(buf[:]) {
					delete(pages, pg)
				} else {
					setPage(pg, &buf)
				}
			}
		}
	}
	return dstDurableImage{
		size:   size,
		pages:  pages,
		blocks: dstBlocksClampImage(synced.blocks, size, pages),
	}
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
