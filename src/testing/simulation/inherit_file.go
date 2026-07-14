// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package simulation

import (
	"os"
	_ "unsafe" // for go:linkname
)

//go:linkname dstInheritFile os.dstInheritFile
func dstInheritFile(*os.File) (*os.File, error)

// dstFSRunTeardown eagerly releases a completed run's filesystem host residue
// (page-cache memfds, the mapping region); called from runLocked's teardown so
// a run leaves the host descriptor table as it found it.
//
//go:linkname dstFSRunTeardown os.dstFSRunTeardown
func dstFSRunTeardown()

// InheritFile grants a Linux simulation explicit access to a host file. It
// must be called from the root simulation body with a host-backed file; Host
// and Process bodies cannot grant host files because that would bypass node
// isolation.
// The returned file owns a hidden duplicate and must be closed; closing it
// does not close file. Its Fd and SyscallConn methods are unavailable because
// authority is carried by the returned File, never by a process-global numeric
// descriptor. The capability supports Read, ReadAt, Write, WriteAt, Seek,
// Stat, Sync, Truncate, Chmod, Readdir, deadlines, and Close; other
// methods retain the simulated-file unsupported behavior. Other operating
// systems return an error until they provide the same numeric-fd fence.
func InheritFile(file *os.File) (*os.File, error) {
	return dstInheritFile(file)
}
