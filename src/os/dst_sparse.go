// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package os

import "slices"

// dstBlockSpan is a half-open [start, end) interval of allocated 4 KB pages
// (dstPageSize) on a regular file. Kept sorted and coalesced on dstFSNode and
// dstDurableImage so LimitDisk, fstat(2) st_blocks, and fallocate(2) operate in
// O(extents) rather than O(logical file size).
type dstBlockSpan struct {
	start int64
	end   int64
}

func dstBlocksAdd(spans []dstBlockSpan, start, end int64) []dstBlockSpan {
	if end <= start {
		return spans
	}
	lo, hi := 0, len(spans)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if spans[mid].end < start {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	i := lo
	j := i
	for j < len(spans) && spans[j].start <= end {
		j++
	}
	if i < j {
		if spans[i].start < start {
			start = spans[i].start
		}
		if spans[j-1].end > end {
			end = spans[j-1].end
		}
		spans[i] = dstBlockSpan{start: start, end: end}
		copy(spans[i+1:], spans[j:])
		return spans[:len(spans)-(j-i-1)]
	}
	spans = append(spans, dstBlockSpan{})
	copy(spans[i+1:], spans[i:])
	spans[i] = dstBlockSpan{start: start, end: end}
	return spans
}

func dstBlocksTruncate(spans []dstBlockSpan, maxPages int64) []dstBlockSpan {
	if maxPages <= 0 {
		return spans[:0]
	}
	for i := range spans {
		if spans[i].start >= maxPages {
			return spans[:i]
		}
		if spans[i].end > maxPages {
			spans[i].end = maxPages
			return spans[:i+1]
		}
	}
	return spans
}

func dstBlocksCount(spans []dstBlockSpan) int64 {
	var n int64
	for _, s := range spans {
		n += s.end - s.start
	}
	return n
}

// dstBlocksSpanEndAt returns the end page of the span covering pg, or 0 if pg
// is unallocated.
func dstBlocksSpanEndAt(spans []dstBlockSpan, pg int64) int64 {
	lo, hi := 0, len(spans)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if spans[mid].end <= pg {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(spans) && spans[lo].start <= pg {
		return spans[lo].end
	}
	return 0
}

// dstBlocksNextStartAfter returns the first span start > pg that is < limit, or
// limit if none exists.
func dstBlocksNextStartAfter(spans []dstBlockSpan, pg, limit int64) int64 {
	lo, hi := 0, len(spans)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if spans[mid].start <= pg {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(spans) && spans[lo].start < limit {
		return spans[lo].start
	}
	return limit
}

func dstBlocksUnallocatedIn(spans []dstBlockSpan, startPg, endPg int64) int64 {
	if endPg <= startPg {
		return 0
	}
	lo, hi := 0, len(spans)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if spans[mid].end <= startPg {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	var covered int64
	for i := lo; i < len(spans) && spans[i].start < endPg; i++ {
		s := max(spans[i].start, startPg)
		e := min(spans[i].end, endPg)
		if e > s {
			covered += e - s
		}
	}
	return (endPg - startPg) - covered
}

// dstBlocksSampleByteOff draws a byte offset uniformly across the allocated
// 4 KB spans clamped to [0, size). When no allocated byte exists within size
// (for example, an empty file or an all-hole sparse file), it returns (0, false)
// without invoking randN so skipped targets consume zero fault-RNG draws.
func dstBlocksSampleByteOff(spans []dstBlockSpan, size int64, randN func(int64) int64) (int64, bool) {
	if size <= 0 {
		return 0, false
	}
	var allocBytes int64
	for _, s := range spans {
		lo := s.start * dstPageSize
		if lo >= size {
			break
		}
		hi := min(s.end*dstPageSize, size)
		if hi > lo {
			allocBytes += hi - lo
		}
	}
	if allocBytes <= 0 {
		return 0, false
	}
	idx := randN(allocBytes)
	for _, s := range spans {
		lo := s.start * dstPageSize
		if lo >= size {
			break
		}
		hi := min(s.end*dstPageSize, size)
		spanBytes := hi - lo
		if spanBytes <= 0 {
			continue
		}
		if idx < spanBytes {
			return lo + idx, true
		}
		idx -= spanBytes
	}
	return 0, false
}

// dstDurablePage is an immutable 4 KB platter page. Unchanged pages are shared
// copy-on-write across syncs and crash tears; bytes past the file's end on the
// final partial page are always zero.
type dstDurablePage *[dstPageSize]byte

// dstDurableImage is a regular file's sparse durable state on the platter:
// logical i_size, non-zero 4 KB pages (absent entries read as zeros), and
// durable 4 KB block allocations (including zero-filled fallocate spans).
type dstDurableImage struct {
	size   int64
	pages  map[int64]dstDurablePage
	blocks []dstBlockSpan
}

// dstBlocksClampImage clones blocks truncated to the page count covering size
// and ensures every materialized non-zero page in pages is marked allocated.
func dstBlocksClampImage(blocks []dstBlockSpan, size int64, pages map[int64]dstDurablePage) []dstBlockSpan {
	maxPages := (size + dstPageSize - 1) / dstPageSize
	out := dstBlocksTruncate(slices.Clone(blocks), maxPages)
	for pg := range pages {
		out = dstBlocksAdd(out, pg, pg+1)
	}
	return out
}

func dstBytesAllZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// dstDurablePageFromSlice returns a sparse durable page for cur (0 < len(cur) <= 4096):
// nil when cur is all-zero, oldPg when oldPg already matches cur with a zero
// tail (copy-on-write sharing), or a freshly allocated 4 KB page otherwise.
func dstDurablePageFromSlice(oldPg dstDurablePage, cur []byte) dstDurablePage {
	if dstBytesAllZero(cur) {
		return nil
	}
	if oldPg != nil && slices.Equal(oldPg[:len(cur)], cur) && dstBytesAllZero(oldPg[len(cur):]) {
		return oldPg
	}
	var buf [dstPageSize]byte
	copy(buf[:], cur)
	return &buf
}

// dstDurablePageClipped clips p to [0, limit) (0 <= limit <= 4096), returning
// nil when the clipped prefix is all-zero, p unchanged when its tail is already
// zero, or a copy-on-write clone with [limit, 4096) zeroed otherwise.
func dstDurablePageClipped(p dstDurablePage, limit int) dstDurablePage {
	if p == nil || limit <= 0 || dstBytesAllZero(p[:limit]) {
		return nil
	}
	if dstBytesAllZero(p[limit:]) {
		return p
	}
	var buf [dstPageSize]byte
	copy(buf[:], p[:limit])
	return &buf
}

// dstDurablePageMatchesWindow reports whether a page's current page-cache
// window of length pageLen (0 < pageLen <= 4096) — holding cur on [0, len(cur))
// and zero-filled on [len(cur), pageLen) — equals the durable page p with
// durLen valid bytes (0 <= durLen <= pageLen) and zero-filled on [durLen, pageLen).
func dstDurablePageMatchesWindow(p dstDurablePage, durLen, pageLen int, cur []byte) bool {
	_ = pageLen
	if p == nil {
		return dstBytesAllZero(cur)
	}
	common := min(len(cur), durLen)
	if !slices.Equal(p[:common], cur[:common]) {
		return false
	}
	if len(cur) > common {
		return dstBytesAllZero(cur[common:])
	}
	return dstBytesAllZero(p[common:durLen])
}

// dstDurablePageEqualAt reports whether two durable pages agree on [0, n) when
// a is valid up to aLen (0 <= aLen <= n) and b is valid up to bLen
// (0 <= bLen <= n), with bytes beyond valid length reading as zero.
func dstDurablePageEqualAt(a dstDurablePage, aLen int, b dstDurablePage, bLen int, n int) bool {
	_ = n
	if a == nil {
		return b == nil || dstBytesAllZero(b[:bLen])
	}
	if b == nil {
		return dstBytesAllZero(a[:aLen])
	}
	if a == b && aLen == bLen {
		return true
	}
	common := min(aLen, bLen)
	if !slices.Equal(a[:common], b[:common]) {
		return false
	}
	if aLen > common {
		return dstBytesAllZero(a[common:aLen])
	}
	if bLen > common {
		return dstBytesAllZero(b[common:bLen])
	}
	return true
}

// copyAt copies up to len(b) bytes starting at off (0 <= off < img.size) from
// img without materializing unallocated holes on the heap.
func (img dstDurableImage) copyAt(b []byte, off int64) int {
	if off < 0 || off >= img.size || len(b) == 0 {
		return 0
	}
	n := int(min(int64(len(b)), img.size-off))
	dst := b[:n]
	for len(dst) > 0 {
		pg := off / dstPageSize
		inPage := int(off % dstPageSize)
		take := min(len(dst), dstPageSize-inPage)
		if p := img.pages[pg]; p != nil {
			copy(dst[:take], p[inPage:inPage+take])
		} else {
			clear(dst[:take])
		}
		dst = dst[take:]
		off += int64(take)
	}
	return n
}

// bytes materializes a dense copy of img for small-file white-box test
// inspectors (DSTFSNodeState). Production sync/crash paths never call this.
func (img dstDurableImage) bytes() []byte {
	if img.size == 0 {
		return nil
	}
	out := make([]byte, img.size)
	for pg, p := range img.pages {
		off := pg * dstPageSize
		if off >= img.size {
			continue
		}
		end := min(off+dstPageSize, img.size)
		copy(out[off:end], p[:end-off])
	}
	return out
}
