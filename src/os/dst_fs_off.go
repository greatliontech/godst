// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package os

import "time"

// Stubs so the dst filesystem gates type-check in a non -tags dst build. The
// gates are all guarded by the dstSimEnabled constant (false here), so every
// reference below is dead code the compiler folds away. The dstFileBackend
// field itself needs no stub type: it is an interface (dst_backend.go) that
// stays nil untagged.

func dstOpenFile(name string, flag int, perm FileMode) (*File, bool, error) {
	return nil, false, nil
}

func dstFSFenced(op, name string) (error, bool) { return nil, false }

func dstFSFencedLink(op, oldname, newname string) (error, bool) { return nil, false }

func dstSameFile(fi1, fi2 FileInfo) (same, handled bool) { return false, false }

func dstOpenDir(name string) (*File, bool, error)         { return nil, false, nil }
func dstMkdir(name string, perm FileMode) (bool, error)   { return false, nil }
func dstRemove(name string) (bool, error)                 { return false, nil }
func dstRemoveAll(name string) (bool, error)              { return false, nil }
func dstRename(oldname, newname string) (bool, error)     { return false, nil }
func dstStatName(op, name string) (FileInfo, bool, error) { return nil, false, nil }
func dstGetwd() (string, bool, error)                     { return "", false, nil }
func dstChdir(dir string) (bool, error)                   { return false, nil }

func dstTempDir() (string, bool) { return "", false }

func dstTruncateName(name string, size int64) (bool, error) { return false, nil }

func dstChmod(name string, mode FileMode) (bool, error) { return false, nil }

func dstChtimes(name string, atime, mtime time.Time) (bool, error) { return false, nil }

func dstNewPipe() (*File, *File, bool) { return nil, nil, false }

func dstFD(file *file) int { return -1 }

func dstReleaseFD(file *file) {}

func dstDropClosedNode(backend dstFileBackend) {}

// dstErrUnsupportedFS exists untagged only so fence gates type-check; every
// reference is behind the folded dstSimEnabled constant.
var dstErrUnsupportedFS error
