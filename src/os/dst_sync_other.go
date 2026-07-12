// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && !linux

package os

func dstSyncModes(flag int) (full, data bool) {
	return O_SYNC != 0 && flag&O_SYNC == O_SYNC, false
}
