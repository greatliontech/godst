// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && !linux

package os

func dstFD(file *file) int {
	panic("os: Fd on a simulated file: " + dstErrUnsupportedFS.Error())
}

func dstReleaseFD(file *file) {}

func dstDropClosedNode(backend dstFileBackend) {}
