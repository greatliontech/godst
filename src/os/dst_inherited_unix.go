// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import (
	"errors"
	"internal/poll"
	"internal/syscall/unix"
	"sync/atomic"
	"syscall"
	"time"
	_ "unsafe" // for go:linkname
)

//go:linkname dstSetHostIO runtime.dstSetHostIO
func dstSetHostIO(active bool) (old bool)

//go:linkname dstInSimBubble runtime.dstInSimBubble
func dstInSimBubble() bool

type dstInheritedFile struct {
	host   *File
	epoch  uint64
	hostID uint32
	procID uint32
	closed atomic.Bool
}

//go:linkname dstInheritFile
func dstInheritFile(src *File) (*File, error) {
	if !dstFSActive() || !dstInSimBubble() {
		return nil, errors.New("simulation.InheritFile called outside a simulation")
	}
	if src == nil || src.file == nil || src.dstf != nil || src.pfd.Sysfd < 0 {
		return nil, ErrInvalid
	}
	hostID, procID := dstFSCurrentNode()
	if hostID != 0 || procID != 0 {
		return nil, errors.New("simulation.InheritFile is restricted to the root simulation body")
	}
	raw, _ := newRawConn(src)
	fd := -1
	nonBlocking := false
	appendMode := false
	var dupErr error
	old := dstSetHostIO(true)
	err := raw.Control(func(srcFD uintptr) {
		fd, _, dupErr = poll.DupCloseOnExec(int(srcFD))
		if dupErr == nil {
			var flags int
			flags, dupErr = unix.Fcntl(fd, syscall.F_GETFL, 0)
			nonBlocking = flags&syscall.O_NONBLOCK != 0
			appendMode = flags&syscall.O_APPEND != 0
		}
	})
	if err == nil {
		err = dupErr
	}
	if err != nil && fd >= 0 {
		_ = syscall.Close(fd)
		fd = -1
	}
	dstSetHostIO(old)
	if err != nil {
		return nil, &PathError{Op: "inherit", Path: src.name, Err: err}
	}
	host := newFile(fd, src.name, kindNewFile, nonBlocking)
	if host == nil {
		syscall.Close(fd)
		return nil, ErrInvalid
	}
	capability := dstNewFile(&dstInheritedFile{host: host, epoch: dstFSEpoch(), hostID: hostID, procID: procID}, src.name)
	capability.appendMode = appendMode
	return capability, nil
}

func (f *dstInheritedFile) begin() (bool, error) {
	hostID, procID := dstFSCurrentNode()
	if f.closed.Load() || !dstFSActive() || !dstInSimBubble() || f.epoch != dstFSEpoch() || f.hostID != hostID || f.procID != procID {
		return false, poll.ErrFileClosing
	}
	return dstSetHostIO(true), nil
}

func (f *dstInheritedFile) closeCaller() error {
	if !dstFSActive() {
		return nil
	}
	hostID, procID := dstFSCurrentNode()
	if !dstInSimBubble() {
		return dstErrUnsupportedFS
	}
	if f.epoch != dstFSEpoch() || f.hostID != hostID || f.procID != procID {
		return poll.ErrFileClosing
	}
	return nil
}

func (f *dstInheritedFile) read(b []byte) (int, error) {
	old, err := f.begin()
	if err != nil {
		return 0, err
	}
	defer dstSetHostIO(old)
	return f.host.read(b)
}

func (f *dstInheritedFile) pread(b []byte, off int64) (int, error) {
	old, err := f.begin()
	if err != nil {
		return 0, err
	}
	defer dstSetHostIO(old)
	return f.host.pread(b, off)
}

func (f *dstInheritedFile) write(b []byte) (int, error) {
	old, err := f.begin()
	if err != nil {
		return 0, err
	}
	defer dstSetHostIO(old)
	return f.host.write(b)
}

func (f *dstInheritedFile) pwrite(b []byte, off int64) (int, error) {
	old, err := f.begin()
	if err != nil {
		return 0, err
	}
	defer dstSetHostIO(old)
	return f.host.pwrite(b, off)
}

func (f *dstInheritedFile) seek(offset int64, whence int) (int64, error) {
	old, err := f.begin()
	if err != nil {
		return 0, err
	}
	defer dstSetHostIO(old)
	return f.host.seek(offset, whence)
}

func (f *dstInheritedFile) truncate(size int64) error {
	old, err := f.begin()
	if err != nil {
		return err
	}
	defer dstSetHostIO(old)
	return f.host.pfd.Ftruncate(size)
}

func (f *dstInheritedFile) sync() error {
	old, err := f.begin()
	if err != nil {
		return err
	}
	defer dstSetHostIO(old)
	return f.host.pfd.Fsync()
}

func (f *dstInheritedFile) stat() (FileInfo, error) {
	old, err := f.begin()
	if err != nil {
		return nil, err
	}
	defer dstSetHostIO(old)
	var fs fileStat
	if err := f.host.pfd.Fstat(&fs.sys); err != nil {
		return nil, err
	}
	fillFileStatFromSys(&fs, f.host.name)
	return &fs, nil
}

func (f *dstInheritedFile) closeFile() error {
	if !f.closed.CompareAndSwap(false, true) {
		return poll.ErrFileClosing
	}
	old := dstSetHostIO(true)
	defer dstSetHostIO(old)
	return f.host.Close()
}

func (f *dstInheritedFile) readdir(n int) ([]string, []FileInfo, error) {
	old, err := f.begin()
	if err != nil {
		return nil, nil, err
	}
	defer dstSetHostIO(old)
	names, _, infos, err := f.host.readdir(n, readdirFileInfo)
	return names, infos, err
}

func (f *dstInheritedFile) chdirHandle() error {
	return dstErrUnsupportedFS
}

func (f *dstInheritedFile) chmodHandle(mode FileMode) error {
	old, err := f.begin()
	if err != nil {
		return err
	}
	defer dstSetHostIO(old)
	return f.host.pfd.Fchmod(syscallMode(mode))
}

func (f *dstInheritedFile) setDeadline(rd, wd bool, t time.Time) error {
	old, err := f.begin()
	if err != nil {
		return err
	}
	defer dstSetHostIO(old)
	switch {
	case rd && wd:
		return f.host.pfd.SetDeadline(t)
	case rd:
		return f.host.pfd.SetReadDeadline(t)
	default:
		return f.host.pfd.SetWriteDeadline(t)
	}
}
