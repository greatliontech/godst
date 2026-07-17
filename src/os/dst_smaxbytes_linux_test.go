// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
)

// The modeled s_maxbytes: every size-growth site — truncate, fallocate, the
// write clip — answers EFBIG at the filesystem's maximum file size, as the
// vfs does. Before the bound existed, huge growth ran into the page-cache
// mapping reserve and died with a runtime fatal no real kernel produces.
// The near-limit files these tests mint are SPARSE (the page cache is a
// memfd: size is address space, physical pages only where touched), so the
// arms are cheap despite the sizes — but none of them may Sync a near-limit
// file: the durable image is an ordinary copied slice, not a sparse view.

// TestDSTSMaxBytesTruncate: growth to the limit succeeds; one byte past is
// EFBIG, with the size untouched — and never the mapping-reserve fatal.
func TestDSTSMaxBytesTruncate(t *testing.T) {
	limit := os.DSTSMaxBytes()
	simulation.Run(1, func() {
		onHost("h", func() {
			f, err := os.Create("/f")
			mustOK(t, "create", err)
			defer f.Close()
			mustOK(t, "truncate to the limit", f.Truncate(limit))
			st, err := f.Stat()
			mustOK(t, "stat", err)
			if st.Size() != limit {
				t.Fatalf("size = %d, want %d", st.Size(), limit)
			}
			wantErrno(t, "truncate past the limit", f.Truncate(limit+1), syscall.EFBIG)
			wantErrno(t, "name truncate past the limit", os.Truncate("/f", limit+1), syscall.EFBIG)
			st, err = f.Stat()
			mustOK(t, "stat", err)
			if st.Size() != limit {
				t.Fatalf("size after refusals = %d, want %d (a refusal grows nothing)", st.Size(), limit)
			}
			// Shrink back and regrow inside the bound: the limit refuses
			// growth, never operation on a large-but-legal file.
			mustOK(t, "shrink", f.Truncate(4096))
			mustOK(t, "regrow", f.Truncate(8192))
		})
	})
}

// TestDSTSMaxBytesFallocate: a span ending past the limit is EFBIG — on an
// uncapped disk (the case that used to fatal) and on a capped one (EFBIG
// wins over ENOSPC: the vfs checks s_maxbytes before the filesystem op).
func TestDSTSMaxBytesFallocate(t *testing.T) {
	limit := os.DSTSMaxBytes()
	simulation.Run(1, func() {
		onHost("h", func() {
			f, err := os.Create("/f")
			mustOK(t, "create", err)
			defer f.Close()
			wantErrno(t, "uncapped span past the limit", falloc(f, 0, 0, limit+1), syscall.EFBIG)
			wantErrno(t, "uncapped offset span past the limit", falloc(f, 0, limit, 4096), syscall.EFBIG)
			simulation.LimitDisk("h", 4096)
			wantErrno(t, "capped span past the limit (EFBIG beats ENOSPC)", falloc(f, 0, 0, limit+1), syscall.EFBIG)
			wantErrno(t, "capped span inside the limit (ENOSPC)", falloc(f, 0, 0, 8192), syscall.ENOSPC)
			simulation.UnlimitDisk("h")
			st, err := f.Stat()
			mustOK(t, "stat", err)
			if st.Size() != 0 {
				t.Fatalf("size after refusals = %d, want 0", st.Size())
			}
		})
	})
}

// TestDSTSMaxBytesWrite: generic_write_checks' shape — a write starting at
// or past the limit is EFBIG outright; one crossing it is clipped and the
// retry surfaces (n, EFBIG) in one call, exactly the ENOSPC partial-fill
// shape with the boundary's errno.
func TestDSTSMaxBytesWrite(t *testing.T) {
	limit := os.DSTSMaxBytes()
	simulation.Run(1, func() {
		onHost("h", func() {
			f, err := os.Create("/f")
			mustOK(t, "create", err)
			defer f.Close()

			// At the boundary: outright EFBIG, nothing written.
			if _, err := f.WriteAt([]byte("xy"), limit); !errors.Is(err, syscall.EFBIG) {
				t.Fatalf("WriteAt at the limit: %v, want EFBIG", err)
			}

			// Crossing the boundary: the first two bytes land, the retry at
			// the boundary reports EFBIG with the partial count.
			n, err := f.WriteAt([]byte("abcd"), limit-2)
			if n != 2 || !errors.Is(err, syscall.EFBIG) {
				t.Fatalf("WriteAt crossing the limit = (%d, %v), want (2, EFBIG)", n, err)
			}
			st, err := f.Stat()
			mustOK(t, "stat", err)
			if st.Size() != limit {
				t.Fatalf("size after crossing write = %d, want exactly the limit %d", st.Size(), limit)
			}

			// The seek-based single-call Write: same combined shape.
			_, err = f.Seek(limit-2, 0)
			mustOK(t, "seek", err)
			n, err = f.Write([]byte("wxyz"))
			if n != 2 || !errors.Is(err, syscall.EFBIG) {
				t.Fatalf("Write crossing the limit = (%d, %v), want (2, EFBIG)", n, err)
			}

			// The vfs order: generic_write_checks' EFBIG precedes any device
			// submission, so at the bound an injected disk fault loses to
			// EFBIG — while a mid-file write under the same fault is EIO.
			simulation.FailDisk("h")
			if _, err := f.WriteAt([]byte("q"), limit); !errors.Is(err, syscall.EFBIG) {
				t.Fatalf("WriteAt at the limit under EIO fault: %v, want EFBIG (never submitted)", err)
			}
			if _, err := f.WriteAt([]byte("q"), 0); !errors.Is(err, syscall.EIO) {
				t.Fatalf("mid-file WriteAt under EIO fault: %v, want EIO", err)
			}
			simulation.HealDisk("h")

			// O_APPEND at a full-size file: EFBIG, as append's offset is the
			// size and the size IS the limit.
			g, err := os.OpenFile("/f", os.O_WRONLY|os.O_APPEND, 0)
			mustOK(t, "open append", err)
			defer g.Close()
			if _, err := g.Write([]byte("z")); !errors.Is(err, syscall.EFBIG) {
				t.Fatalf("append at the limit: %v, want EFBIG", err)
			}
		})
	})
}
