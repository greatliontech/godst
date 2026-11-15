// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os

import _ "unsafe" // for go:linkname

//go:linkname dstBubbleStdioAppend runtime.dstBubbleStdioAppend
func dstBubbleStdioAppend(fd int, p []byte) int

func dstTryBubbleStdioWrite(file *file, b []byte) (int, bool) {
	if file == nil {
		return 0, false
	}
	fd := dstSysfd(file)
	if (fd != 1 && fd != 2) || !dstFenceActive() || dstHostIOActive() {
		return 0, false
	}
	return dstBubbleStdioAppend(fd, b), true
}
