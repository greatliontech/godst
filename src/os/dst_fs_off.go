// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package os

// Stubs so the dst filesystem gates type-check in a non -tags dst build. The
// gates are all guarded by the dstSimEnabled constant (false here), so every
// reference below is dead code the compiler folds away; the method bodies are
// unreachable by construction.

type dstFile struct{}

func (*dstFile) read(b []byte) (int, error)                { panic("unreachable") }
func (*dstFile) pread(b []byte, off int64) (int, error)    { panic("unreachable") }
func (*dstFile) write(b []byte) (int, error)               { panic("unreachable") }
func (*dstFile) pwrite(b []byte, off int64) (int, error)   { panic("unreachable") }
func (*dstFile) seek(off int64, whence int) (int64, error) { panic("unreachable") }
func (*dstFile) truncate(size int64) error                 { panic("unreachable") }
func (*dstFile) sync() error                               { panic("unreachable") }
func (*dstFile) stat() (FileInfo, error)                   { panic("unreachable") }
func (*dstFile) closeFile() error                          { panic("unreachable") }

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

func (*dstFile) readdir(n int) ([]string, []FileInfo, error) { panic("unreachable") }
func (*dstFile) chdirHandle() error                          { panic("unreachable") }

// dstErrUnsupportedFS exists untagged only so fence gates type-check; every
// reference is behind the folded dstSimEnabled constant.
var dstErrUnsupportedFS error
