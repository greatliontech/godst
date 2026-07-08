// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && !linux

package os

func dstMMapWriteLocked(node *dstFSNode, off int64, p []byte) {}

func dstMMapTruncateLocked(node *dstFSNode, size int64) {}
