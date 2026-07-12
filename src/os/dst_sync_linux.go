// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import "syscall"

func dstSyncModes(flag int) (full, data bool) {
	full = syscall.O_SYNC != syscall.O_DSYNC && flag&syscall.O_SYNC == syscall.O_SYNC
	data = !full && flag&syscall.O_DSYNC != 0
	return full, data
}
