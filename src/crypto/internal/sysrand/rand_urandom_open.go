// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst || (!aix && !dragonfly && !freebsd && !linux && !solaris)

package sysrand

import "os"

func openUrandom() (*os.File, error) {
	return os.Open("/dev/urandom")
}
