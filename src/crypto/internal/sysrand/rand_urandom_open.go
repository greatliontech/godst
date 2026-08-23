// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst || (!aix && !dragonfly && !freebsd && !linux && !solaris)

package sysrand

import "os"

// dstOpenUrandom is referenced only from urandomRead's constant-false dst
// branch in this build; the stub keeps it compiling and folds away.
func dstOpenUrandom() (*os.File, error) {
	return nil, nil
}
