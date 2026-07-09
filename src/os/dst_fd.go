// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import (
	"errors"
	"internal/poll"
	"io"
	"sync"
	"syscall"
	_ "unsafe" // for go:linkname
)

const dstVirtualFDBase = 1 << 30
const dstVirtualFDCount = 1 << 20

//go:linkname dstSetReadHook syscall.dstSetReadHook
func dstSetReadHook(func(fd int, p []byte) (n int, err syscall.Errno, handled bool))

//go:linkname dstSetWriteHook syscall.dstSetWriteHook
func dstSetWriteHook(func(fd int, p []byte) (n int, err syscall.Errno, handled bool))

//go:linkname dstSetPreadHook syscall.dstSetPreadHook
func dstSetPreadHook(func(fd int, p []byte, offset int64) (n int, err syscall.Errno, handled bool))

//go:linkname dstSetPwriteHook syscall.dstSetPwriteHook
func dstSetPwriteHook(func(fd int, p []byte, offset int64) (n int, err syscall.Errno, handled bool))

//go:linkname dstSetCloseHook syscall.dstSetCloseHook
func dstSetCloseHook(func(fd int) (err syscall.Errno, handled bool))

//go:linkname dstSetFstatHook syscall.dstSetFstatHook
func dstSetFstatHook(func(fd int, stat *syscall.Stat_t) (err syscall.Errno, handled bool))

//go:linkname dstSetSeekHook syscall.dstSetSeekHook
func dstSetSeekHook(func(fd int, offset int64, whence int) (off int64, err syscall.Errno, handled bool))

//go:linkname dstSetFsyncHook syscall.dstSetFsyncHook
func dstSetFsyncHook(func(fd int) (err syscall.Errno, handled bool))

//go:linkname dstSetFdatasyncHook syscall.dstSetFdatasyncHook
func dstSetFdatasyncHook(func(fd int) (err syscall.Errno, handled bool))

//go:linkname dstSetFlockHook syscall.dstSetFlockHook
func dstSetFlockHook(func(fd int, how int) (err syscall.Errno, handled bool))

func init() {
	dstSetReadHook(dstFDRead)
	dstSetWriteHook(dstFDWrite)
	dstSetPreadHook(dstFDPread)
	dstSetPwriteHook(dstFDPwrite)
	dstSetCloseHook(dstFDClose)
	dstSetFstatHook(dstFDFstat)
	dstSetSeekHook(dstFDSeek)
	dstSetFsyncHook(dstFDFsync)
	dstSetFdatasyncHook(dstFDFdatasync)
	dstSetFlockHook(dstFDFlock)
}

type dstFDEntry struct {
	backend dstFileBackend
	epoch   uint64
	host    uint32
	proc    uint32
}

var dstFDRegistry struct {
	mu    sync.Mutex
	epoch uint64
	next  int
	fds   map[int]dstFDEntry
}

func dstFDRollLocked() {
	if e := dstFSEpoch(); e != dstFDRegistry.epoch || dstFDRegistry.fds == nil {
		dstFDRegistry.epoch = e
		dstFDRegistry.next = dstVirtualFDBase
		dstFDRegistry.fds = make(map[int]dstFDEntry)
	}
}

func dstFD(file *file) int {
	if !dstFSActive() {
		panic("os: Fd on a simulated file: " + dstErrUnsupportedFS.Error())
	}
	df, ok := file.dstf.(*dstFile)
	if !ok {
		panic("os: Fd on a simulated file: " + dstErrUnsupportedFS.Error())
	}
	df.mu.Lock()
	defer df.mu.Unlock()
	closed := df.closed
	if closed {
		return -1
	}
	host, proc := dstFSCurrentNode()
	epoch := dstFSEpoch()
	key := dstFDKey{epoch: epoch, host: host, proc: proc}
	dstFDRegistry.mu.Lock()
	defer dstFDRegistry.mu.Unlock()
	dstFDRollLocked()
	if fd := file.dstfds[key]; fd != 0 {
		entry, ok := dstFDRegistry.fds[fd]
		if ok && entry.backend == file.dstf && entry.epoch == epoch && entry.host == host && entry.proc == proc {
			return fd
		}
		delete(file.dstfds, key)
	}
	if dstFDRegistry.next >= dstVirtualFDBase+dstVirtualFDCount {
		panic("os: too many simulated file descriptors")
	}
	fd := dstFDRegistry.next
	dstFDRegistry.next++
	if file.dstfds == nil {
		file.dstfds = make(map[dstFDKey]int)
	}
	file.dstfds[key] = fd
	dstFDRegistry.fds[fd] = dstFDEntry{backend: file.dstf, epoch: epoch, host: host, proc: proc}
	return fd
}

func dstReleaseFD(file *file) {
	if file.dstfds == nil {
		return
	}
	type release struct {
		fd    int
		entry dstFDEntry
	}
	var releases []release
	dstFDRegistry.mu.Lock()
	dstFDRollLocked()
	for key, fd := range file.dstfds {
		entry, ok := dstFDRegistry.fds[fd]
		if ok && entry.backend == file.dstf && entry.epoch == key.epoch && entry.host == key.host && entry.proc == key.proc {
			delete(dstFDRegistry.fds, fd)
			releases = append(releases, release{fd: fd, entry: entry})
		}
		delete(file.dstfds, key)
	}
	dstFDRegistry.mu.Unlock()
	for _, rel := range releases {
		dstFlockReleaseFD(rel.entry, rel.fd)
	}
}

func dstReleaseBackendFDs(backend dstFileBackend) {
	type release struct {
		fd    int
		entry dstFDEntry
	}
	var releases []release
	dstFDRegistry.mu.Lock()
	dstFDRollLocked()
	for fd, entry := range dstFDRegistry.fds {
		if entry.backend == backend {
			delete(dstFDRegistry.fds, fd)
			releases = append(releases, release{fd: fd, entry: entry})
		}
	}
	dstFDRegistry.mu.Unlock()
	for _, rel := range releases {
		dstFlockReleaseFD(rel.entry, rel.fd)
	}
}

// dstReleaseProcFDs sweeps the fd registry for entries ATTRIBUTED to proc.
// dstCloseProcFiles already released the fds of every file proc opened; what
// remains is the out-of-model residue of a *File shared across processes — an
// fd minted by proc on a file some OTHER process opened (fd entries key by the
// minting goroutine's node, open-file entries by the opener's). Sweeping those
// keeps the attribution invariant: no fd of a dead process survives teardown.
func dstReleaseProcFDs(proc uint32) {
	dstReleaseFDs(func(e dstFDEntry) bool { return e.proc == proc })
}

func dstReleaseFDs(match func(dstFDEntry) bool) {
	type release struct {
		fd    int
		entry dstFDEntry
	}
	var releases []release
	dstFDRegistry.mu.Lock()
	dstFDRollLocked()
	for fd, entry := range dstFDRegistry.fds {
		if match(entry) {
			delete(dstFDRegistry.fds, fd)
			releases = append(releases, release{fd: fd, entry: entry})
		}
	}
	dstFDRegistry.mu.Unlock()
	for _, rel := range releases {
		dstFlockReleaseFD(rel.entry, rel.fd)
	}
}

func dstDropClosedNode(backend dstFileBackend) {
	if file, ok := backend.(*dstFile); ok {
		file.dropClosedNode()
	}
}

func dstFDLookup(fd int) (entry dstFDEntry, handled bool, errno syscall.Errno) {
	if fd < dstVirtualFDBase || fd >= dstVirtualFDBase+dstVirtualFDCount {
		return dstFDEntry{}, false, 0
	}
	if !dstFSActive() {
		return dstFDEntry{}, true, syscall.EBADF
	}
	host, proc := dstFSCurrentNode()
	dstFDRegistry.mu.Lock()
	dstFDRollLocked()
	entry, ok := dstFDRegistry.fds[fd]
	dstFDRegistry.mu.Unlock()
	if !ok || entry.epoch != dstFSEpoch() || entry.host != host || entry.proc != proc {
		return dstFDEntry{}, true, syscall.EBADF
	}
	return entry, true, 0
}

func dstFDErr(err error) syscall.Errno {
	if err == nil || errors.Is(err, io.EOF) {
		return 0
	}
	if errors.Is(err, poll.ErrFileClosing) {
		return syscall.EBADF
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	return syscall.EIO
}

func dstFDCountResult(n int, err error) (int, syscall.Errno, bool) {
	if n > 0 {
		return n, 0, true
	}
	if errno := dstFDErr(err); errno != 0 {
		return -1, errno, true
	}
	return n, 0, true
}

func dstFDZeroRead(entry dstFDEntry) syscall.Errno {
	if f, ok := entry.backend.(*dstFile); ok {
		f.diskDelay()
		if err := f.enter(); err != nil {
			return dstFDErr(err)
		}
		defer f.leave()
		if !f.rd {
			return syscall.EBADF
		}
		if f.node.isDir {
			return syscall.EISDIR
		}
		if err := f.diskEIO(); err != nil {
			return dstFDErr(err)
		}
		return 0
	}
	_, err := entry.backend.read(nil)
	return dstFDErr(err)
}

func dstFDZeroWrite(entry dstFDEntry) syscall.Errno {
	if f, ok := entry.backend.(*dstFile); ok {
		f.diskDelay()
		if err := f.enter(); err != nil {
			return dstFDErr(err)
		}
		defer f.leave()
		if !f.wr {
			return syscall.EBADF
		}
		if err := f.diskEIO(); err != nil {
			return dstFDErr(err)
		}
		return 0
	}
	_, err := entry.backend.write(nil)
	return dstFDErr(err)
}

func dstFDRead(fd int, p []byte) (int, syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return -1, errno, handled
	}
	if len(p) == 0 {
		if errno := dstFDZeroRead(entry); errno != 0 {
			return -1, errno, true
		}
		return 0, 0, true
	}
	read, err := entry.backend.read(p)
	return dstFDCountResult(read, err)
}

func dstFDWrite(fd int, p []byte) (int, syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return -1, errno, handled
	}
	if len(p) == 0 {
		if errno := dstFDZeroWrite(entry); errno != 0 {
			return -1, errno, true
		}
		return 0, 0, true
	}
	written, err := entry.backend.write(p)
	return dstFDCountResult(written, err)
}

func dstFDPread(fd int, p []byte, off int64) (int, syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return -1, errno, handled
	}
	if off < 0 {
		return -1, syscall.EINVAL, true
	}
	if len(p) == 0 {
		if errno := dstFDZeroRead(entry); errno != 0 {
			return -1, errno, true
		}
		return 0, 0, true
	}
	read, err := entry.backend.pread(p, off)
	return dstFDCountResult(read, err)
}

func dstFDPwrite(fd int, p []byte, off int64) (int, syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return -1, errno, handled
	}
	if off < 0 {
		return -1, syscall.EINVAL, true
	}
	if len(p) == 0 {
		if errno := dstFDZeroWrite(entry); errno != 0 {
			return -1, errno, true
		}
		return 0, 0, true
	}
	written, err := entry.backend.pwrite(p, off)
	return dstFDCountResult(written, err)
}

func dstFDSeek(fd int, off int64, whence int) (int64, syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return -1, errno, handled
	}
	newOff, err := entry.backend.seek(off, whence)
	if errno := dstFDErr(err); errno != 0 {
		return -1, errno, true
	}
	return newOff, 0, true
}

func dstFDFsync(fd int) (syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return errno, handled
	}
	return dstFDErr(entry.backend.sync()), true
}

func dstFDFdatasync(fd int) (syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return errno, handled
	}
	if file, ok := entry.backend.(*dstFile); ok {
		return dstFDErr(file.datasync()), true
	}
	return dstFDErr(entry.backend.sync()), true
}

func dstFDClose(fd int) (syscall.Errno, bool) {
	entry, handled, errno := dstFDLookup(fd)
	if !handled || errno != 0 {
		return errno, handled
	}
	errno = dstFDErr(entry.backend.closeFile())
	if errno != 0 {
		return errno, true
	}
	dstReleaseBackendFDs(entry.backend)
	dstDropClosedNode(entry.backend)
	return 0, true
}
