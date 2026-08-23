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
// (dstFile) and the in-memory pipe end (dstPipeEnd) implement it. The
// backing is chosen when the File is created (open, Pipe); it is nil on
// every host-backed File and always nil in a non -tags dst build, where the
// gates that consult it are dead code behind the dstSimEnabled constant.
//
// Methods return raw errors (errno, internal/poll sentinels); the os layer
// wraps them into *PathError exactly as it wraps host errors, so error
// identity is production-shaped for free. See docs/dst/design.md,
// "In-memory deterministic filesystem" (the backend-not-fd paragraph).
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
}

// dstFileBackendExt carries the backend methods whose signatures name
// method-bearing std types — time.Time, FileMode, FileInfo — deliberately
// OUTSIDE dstFileBackend: a method signature on the named interface embedded
// in every os.file marks its parameter and result types used-in-interface,
// which retains the whole time package and io/fs method text (~35 KB) in
// untagged binaries. Call sites assert to this interface under the
// dstSimEnabled constant, so untagged the assertions — and the retention —
// fold away; the var pins below keep a missing method a compile error, the
// totality the split would otherwise trade for a runtime panic.
type dstFileBackendExt interface {
	stat() (FileInfo, error)
	readdir(n int) (names []string, infos []FileInfo, err error)
	chmodHandle(mode FileMode) error
	setDeadline(rd, wd bool, t time.Time) error
}

// The var pins for the concrete backends live beside their dst-gated type
// definitions (dst_fs.go, dst_pipe.go, dst_proc.go, dst_inherited_unix.go).

func dstCloseCaller(file *file) error {
	if file == nil {
		return nil
	}
	backend := file.dstBackend()
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
