// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package testdeps

import (
	"io"
	"os"
	"sync/atomic"
	"syscall"
	_ "unsafe" // for go:linkname
)

// Lock-free, zero-heap-alloc testlog path (docs/dst/design.md, "Deterministic
// pipes and the stdio stance"). Under `go test`'s test-caching mode
// (-test.testlogfile), every os.Open / os.Stat / os.Getenv / os.Chdir records
// an action line through testLog.add. Acquiring log.mu from a bubble goroutine
// would park the bubble behind any concurrent host goroutine holding log.mu,
// waking at a wall-clock-dependent instant and coupling the seeded schedule to
// host I/O timing. Buffering host lines inside log.w (*bufio.Writer) would also
// flush mid-line whenever a 4096-byte buffer boundary splits an argument,
// splicing partial host lines with concurrent raw bubble writes on the shared
// open file description.
//
// StartTestLog flushes the "# test log\n" header synchronously before returning
// (leaving log.w empty and guaranteeing the header precedes every subsequent
// write at offset 0) and publishes the captured raw file descriptor out of line
// (preserving testLog's exact upstream struct shape for INV-TYPESHAPE). When
// the writer is file-backed, both host callers (under log.mu) and bubble
// goroutines (lock-free) emit each complete "<op> <name>\n" line via runtime
// dstWriteTestlogLine without touching log.w or allocating on the Go heap; any
// write errno is latched in an atomic uint32 and converted to a *os.PathError
// only at StopTestLog outside any bubble.

const dstTestlogEnabled = true

//go:linkname dstInSimBubble runtime.dstInSimBubble
func dstInSimBubble() bool

//go:linkname dstWriteTestlogLine runtime.dstWriteTestlogLine
func dstWriteTestlogLine(fd int, op, name string) int32

type dstTestlogState struct {
	target   *testLog
	fd       int
	name     string
	flushErr error
	errErrno atomic.Uint32
}

var (
	dstActiveTestlog   atomic.Pointer[dstTestlogState]
	dstTestlogInBubble = dstInSimBubble
)

func dstStartTestLogLocked(l *testLog, w io.Writer) {
	// Flush "# test log\n" immediately so raw writes never land ahead of the
	// header that cmd/go's readTestLog expects on line 1, and leave l.w empty
	// thereafter whenever w is file-backed.
	flushErr := l.w.Flush()
	st := &dstTestlogState{target: l, fd: -1, flushErr: flushErr}
	if f, ok := w.(*os.File); ok {
		// Fd() also ensures blocking mode on the descriptor.
		if fd := int(f.Fd()); fd >= 0 {
			st.fd = fd
			st.name = f.Name()
		}
	}
	dstActiveTestlog.Store(st)
}

func dstStopTestLogLocked(l *testLog) error {
	st := dstActiveTestlog.Load()
	if st == nil || st.target != l {
		return nil
	}
	dstActiveTestlog.CompareAndSwap(st, nil)
	if st.flushErr != nil {
		return st.flushErr
	}
	if errno := st.errErrno.Load(); errno != 0 {
		return &os.PathError{Op: "write", Path: st.name, Err: syscall.Errno(errno)}
	}
	return nil
}

func dstBubbleTestlogAdd(l *testLog, op, name string) bool {
	if !dstTestlogInBubble() {
		return false
	}
	st := dstActiveTestlog.Load()
	if st == nil || st.target != l {
		return true
	}
	if st.fd < 0 {
		panic("testing/internal/testdeps: testlog writer is not file-backed: unsupported under deterministic simulation")
	}
	if st.flushErr != nil || st.errErrno.Load() != 0 {
		return true
	}
	if errno := dstWriteTestlogLine(st.fd, op, name); errno != 0 {
		st.errErrno.CompareAndSwap(0, uint32(errno))
	}
	return true
}

func dstHostTestlogAddLocked(l *testLog, op, name string) bool {
	st := dstActiveTestlog.Load()
	if st == nil || st.target != l || st.fd < 0 {
		return false
	}
	if st.flushErr != nil || st.errErrno.Load() != 0 {
		return true
	}
	if errno := dstWriteTestlogLine(st.fd, op, name); errno != 0 {
		st.errErrno.CompareAndSwap(0, uint32(errno))
	}
	return true
}
