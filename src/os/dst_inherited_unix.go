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

// dstErrNodeScoped is the refusal for an inherited-file capability operated
// from a simulated Host or Process body other than the root simulation body
// that holds the grant. Two real machines cannot share an open file
// description, so cross-node use has no production error shape to mimic — it
// is the cross-node-channel escape the grant contract forbids (design.md:
// Host and Process bodies cannot hold host-file authority), refused with a
// DISTINGUISHABLE error: a closed-file shape here misdirects diagnosis toward
// close bugs, and a logging pipeline that swallows write errors would lose
// every record with no thread to pull. The os layer wraps this into
// *PathError like any backend error; it deliberately does not match
// ErrClosed.
var dstErrNodeScoped error = &dstNodeScopedError{}

type dstNodeScopedError struct{}

func (*dstNodeScopedError) Error() string {
	return "inherited file capability is node-scoped to the root simulation body; relay cross-node I/O through the root"
}

// dstErrStaleRun is the node-scoped refusal's TEMPORAL sibling: an
// inherited-file capability operated outside the run that granted it — after
// the run, during a LATER run, or from a goroutine outside the run's bubble.
// The grant is per run (the capability carries its run epoch), so there is no
// production error to mimic, and the capability is NOT closed: its hidden
// host duplicate stays open until Close, so a closed-file shape here
// misdirects diagnosis toward close bugs exactly as the node-scoped rationale
// records — an error-swallowing pipeline would silently drop every operation
// while the real fix (re-grant inside the run that uses it, or Close the
// leak) goes unnamed. The os layer wraps this into *PathError like any
// backend error; it deliberately does not match ErrClosed. Close remains
// available in every temporal state to release the duplicate.
var dstErrStaleRun error = &dstStaleRunError{}

type dstStaleRunError struct{}

func (*dstStaleRunError) Error() string {
	return "inherited file capability is scoped to the simulation run that granted it; the capability is not closed — grant it inside the run that uses it, or Close it to release the host duplicate"
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
	if f.closed.Load() {
		// Genuinely closed: the one state where the closed shape IS the
		// diagnosis.
		return false, poll.ErrFileClosing
	}
	if !dstFSActive() || !dstInSimBubble() || f.epoch != dstFSEpoch() {
		// Outside the granting run's temporal scope — after the run, in a
		// LATER run, or from a goroutine outside the run's bubble: the typed
		// temporal refusal, never the closed shape (the capability is not
		// closed; see dstErrStaleRun).
		return false, dstErrStaleRun
	}
	if hostID, procID := dstFSCurrentNode(); f.hostID != hostID || f.procID != procID {
		// A live capability reached from inside a Host or Process body: the
		// node-scoped refusal, not the closed shape — the capability is not
		// closed, and the caller needs the actual diagnosis (see
		// dstErrNodeScoped).
		return false, dstErrNodeScoped
	}
	return dstSetHostIO(true), nil
}

func (f *dstInheritedFile) closeCaller() error {
	if !dstFSActive() {
		return nil
	}
	if f.epoch != dstFSEpoch() {
		// A capability from a DEAD run: its grant is gone and the only
		// meaningful operation left is releasing the hidden host duplicate —
		// Close always works (the pipe backend's temporal stance; refusing it
		// here left the duplicate leaked for as long as later runs were
		// active).
		return nil
	}
	if !dstInSimBubble() {
		return dstErrUnsupportedFS
	}
	if hostID, procID := dstFSCurrentNode(); f.hostID != hostID || f.procID != procID {
		// The node-scoped refusal, as in begin.
		return dstErrNodeScoped
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
