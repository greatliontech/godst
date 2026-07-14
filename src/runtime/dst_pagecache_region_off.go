// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && (386 || arm)

package runtime

// A 32-bit address space cannot hold the region; the page cache refuses these
// hosts at first use (dstPageCacheCheckHost). Zero geometry so the file
// compiles.
const (
	dstMapRegionBase = 0
	dstMapRegionSize = 0
	_dstMapNoReserve = 0
)
