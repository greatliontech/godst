// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst || (dst && !(unix || wasip1))

package os

import "time"

type dstRoot struct{}

func dstRootActive(r *Root) bool { return false }

func dstOpenRoot(name string) (*Root, bool, error) { return nil, false, nil }

func dstRootOpenRoot(r *Root, name string) (*Root, error) { panic("unreachable") }

func dstRootOpenFile(r *Root, name string, flag int, perm FileMode) (*File, error) {
	panic("unreachable")
}

func dstRootStat(r *Root, name string, lstat bool) (FileInfo, error) { panic("unreachable") }

func dstRootReadlink(r *Root, name string) (string, error) { panic("unreachable") }

func dstRootChmod(r *Root, name string, mode FileMode) error { panic("unreachable") }

func dstRootChtimes(r *Root, name string, atime, mtime time.Time) error { panic("unreachable") }

func dstRootMkdir(r *Root, name string, perm FileMode) error { panic("unreachable") }

func dstRootMkdirAll(r *Root, name string, perm FileMode) error { panic("unreachable") }

func dstRootRemove(r *Root, name string) error { panic("unreachable") }

func dstRootRemoveAll(r *Root, name string) error { panic("unreachable") }

func dstRootRename(r *Root, oldname, newname string) error { panic("unreachable") }
