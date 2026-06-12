// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package os

import "time"

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
	stat() (FileInfo, error)
	closeFile() error
	readdir(n int) (names []string, infos []FileInfo, err error)
	chdirHandle() error
	chmodHandle(mode FileMode) error
	setDeadline(rd, wd bool, t time.Time) error
}
