// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && (aix || dragonfly || freebsd || linux || solaris)

package sysrand

import (
	"os"
	"syscall"
)

func openUrandom() (*os.File, error) {
	const name = "/dev/urandom"
	var fd int
	var err error
	for {
		fd, err = syscall.Open(name, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}
