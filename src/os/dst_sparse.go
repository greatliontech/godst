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

// dstDurablePageMatchesSlice reports whether cur equals the durable bytes on
// [pg*4096, pg*4096+len(cur)), where valid (0 <= valid <= len(cur)) is the
// portion within synced.size and any tail [valid, len(cur)) is zero-padded.
func dstDurablePageMatchesSlice(p dstDurablePage, valid int, cur []byte) bool {
	if p == nil {
		return dstBytesAllZero(cur)
	}
	return slices.Equal(p[:valid], cur[:valid]) && dstBytesAllZero(cur[valid:])
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
