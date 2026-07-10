// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package runtime

const DSTPageCachePageSize = dstPageCachePageSize

var (
	DSTPageCacheNew     = dstPageCacheNew
	DSTPageCacheResize  = dstPageCacheResize
	DSTPageCacheMap     = dstPageCacheMap
	DSTPageCacheUnmap   = dstPageCacheUnmap
	DSTPageCacheClose   = dstPageCacheClose
	DSTMappingFaultAddr = dstMappingFaultAddr
	DSTPageCacheReset   = dstPageCacheResetRegion
)

const DSTMapRegionBase = dstMapRegionBase
const DSTMapRegionSize = dstMapRegionSize

const (
	DSTProtRead  = _PROT_READ
	DSTProtWrite = _PROT_WRITE
)
