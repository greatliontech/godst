// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package simulation

import "io/fs"

type closedHostFS struct{}

var (
	_ fs.FS        = closedHostFS{}
	_ fs.StatFS    = closedHostFS{}
	_ fs.ReadDirFS = closedHostFS{}
)

func (closedHostFS) err(op, name string) error {
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	return &fs.PathError{Op: op, Path: name, Err: fs.ErrClosed}
}

func (h closedHostFS) Open(name string) (fs.File, error)          { return nil, h.err("open", name) }
func (h closedHostFS) Stat(name string) (fs.FileInfo, error)      { return nil, h.err("stat", name) }
func (h closedHostFS) ReadDir(name string) ([]fs.DirEntry, error) { return nil, h.err("open", name) }

// HostFS requires building with -tags dst (the simulated filesystem), and is
// only meaningful inside an active simulation run. Outside a run (including in
// untagged builds), the returned fs.FS validates paths and fails operations
// with fs.ErrClosed, matching tagged post-run HostFS handles.
func HostFS(name string) fs.FS {
	withBubbleInspectCaller("HostFS", func() { _ = lookupHost(name) })
	return closedHostFS{}
}
