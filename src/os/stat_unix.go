// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || (js && wasm) || wasip1

package os

import (
	"internal/testlog"
	"syscall"
)

// Stat returns the [FileInfo] structure describing file.
// If there is an error, it will be of type [*PathError].
func (f *File) Stat() (FileInfo, error) {
	if f == nil {
		return nil, ErrInvalid
	}
	// An fd-based stat observes the same metadata a path stat would;
	// leaving it out of the test log lets a caller read an opened
	// file's modification time unobserved, so cache keys built from
	// the log under-pin exactly the metadata this call returns. The
	// log line belongs to the public method only: stdlib-internal
	// helpers whose FileInfo never escapes (ReadFile's buffer-sizing
	// stat) go through fstatNolog, because logging an observation the
	// caller cannot read would over-pin every ReadFile input to
	// metadata nothing consumed.
	testlog.Stat(f.name)
	return f.fstatNolog()
}

// fstatNolog is (*File).Stat with no test logging, for stdlib-internal
// stats whose result does not escape to the caller.
func (f *File) fstatNolog() (FileInfo, error) {
	if dstSimEnabled {
		if dstf := dstBackendOf(f.file); dstf != nil {
			fi, err := dstf.(dstFileBackendExt).stat()
			if err != nil {
				return nil, f.wrapErr("stat", err)
			}
			return fi, nil
		}
	}
	var fs fileStat
	err := f.pfd.Fstat(&fs.sys)
	if err != nil {
		return nil, f.wrapErr("stat", err)
	}
	fillFileStatFromSys(&fs, f.name)
	return &fs, nil
}

// statNolog stats a file with no test logging.
func statNolog(name string) (FileInfo, error) {
	var fs fileStat
	err := ignoringEINTR(func() error {
		return syscall.Stat(name, &fs.sys)
	})
	if err != nil {
		return nil, &PathError{Op: "stat", Path: name, Err: err}
	}
	fillFileStatFromSys(&fs, name)
	return &fs, nil
}

// lstatNolog lstats a file with no test logging.
func lstatNolog(name string) (FileInfo, error) {
	var fs fileStat
	err := ignoringEINTR(func() error {
		return syscall.Lstat(name, &fs.sys)
	})
	if err != nil {
		return nil, &PathError{Op: "lstat", Path: name, Err: err}
	}
	fillFileStatFromSys(&fs, name)
	return &fs, nil
}
