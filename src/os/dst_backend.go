// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package os

import (
	"time"
)

type dstFDKey struct {
	epoch uint64
	host  uint32
	proc  uint32
}

// dstFileBackend is the seam between *os.File and its simulated backing
// under deterministic simulation (-tags dst): the in-memory tree file
// (dstFile), in-memory pipe end (dstPipeEnd), procfs overlay (dstProcFile),
// and explicitly inherited host handle (dstInheritedFile) implement it.
// Backends live out of line in dstFileStates (dst_filestate.go) and every
// consultation in os is gated by the dstSimEnabled constant, so untagged
// builds retain zero interface or method-table references.
//
// Methods return raw errors (errno, internal/poll sentinels); the os layer
// wraps them into *PathError exactly as it wraps host errors, so error
// identity is production-shaped for free. See docs/dst/design.md,
// "In-memory deterministic filesystem".
type dstFileBackend interface {
	read(b []byte) (int, error)
	pread(b []byte, off int64) (int, error)
	write(b []byte) (int, error)
	pwrite(b []byte, off int64) (int, error)
	seek(offset int64, whence int) (int64, error)
	truncate(size int64) error
	sync() error
	closeFile() error
	chdirHandle() error
	stat() (FileInfo, error)
	readdir(n int) (names []string, infos []FileInfo, err error)
	chmodHandle(mode FileMode) error
	setDeadline(rd, wd bool, t time.Time) error
}

func dstCloseCaller(file *file) error {
	if dstSimEnabled {
		if file == nil {
			return nil
		}
		backend := dstBackendOf(file)
		if backend == nil {
			if dstFenceActive() && !dstHostIOActive() {
				return dstHostCloseError()
			}
			return nil
		}
		gate, ok := backend.(interface{ closeCaller() error })
		if !ok {
			return nil
		}
		return gate.closeCaller()
	}
	return nil
}
