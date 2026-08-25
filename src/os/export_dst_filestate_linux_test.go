// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

// dstReapStubBackend is the minimal dstFileBackend for the reap probe:
// the row's lifecycle is what is under test, never the backend.
type dstReapStubBackend struct{}

func (dstReapStubBackend) read(b []byte) (int, error)                   { return 0, nil }
func (dstReapStubBackend) pread(b []byte, off int64) (int, error)       { return 0, nil }
func (dstReapStubBackend) write(b []byte) (int, error)                  { return len(b), nil }
func (dstReapStubBackend) pwrite(b []byte, off int64) (int, error)      { return len(b), nil }
func (dstReapStubBackend) seek(offset int64, whence int) (int64, error) { return 0, nil }
func (dstReapStubBackend) truncate(size int64) error                    { return nil }
func (dstReapStubBackend) sync() error                                  { return nil }
func (dstReapStubBackend) closeFile() error                             { return nil }
func (dstReapStubBackend) chdirHandle() error                           { return nil }

// DSTFileStateReapProbe registers a throwaway file in the out-of-line
// state table and returns a presence check holding only the row's index
// and identity — the file itself is dropped, so once it is collected a
// sweep must release the row (a recycled index holds a different row,
// which counts as released). The sweep mechanism is ownership-blind:
// this probe exercises the identical path a simulated process's rows
// take (os/dst_filestate.go).
func DSTFileStateReapProbe() (present func() bool) {
	f := &file{name: "reap-probe"}
	dstSetFileBackend(f, dstReapStubBackend{})
	idx, ok := dstStateIndex(f.pfd.Sysfd)
	if !ok {
		panic("os: reap probe slot not set")
	}
	row := dstFileStates.get(idx)
	if row == nil || row.backend == nil {
		panic("os: reap probe state row not set")
	}
	return func() bool {
		return dstFileStates.get(idx) == row
	}
}

// DSTFileStateSweep runs the state tables' sweep — the same reclamation
// dstFSRunTeardown triggers at every run teardown.
func DSTFileStateSweep() {
	dstStateTablesSweep()
}
